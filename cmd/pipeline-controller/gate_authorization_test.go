package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/prow/pkg/github"
)

// TestGateAllowlist covers the only default grant and its replacement or removal.
func TestGateAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "default", want: []string{"redhat-chai-bot"}},
		{name: "replace", args: []string{"--mark-pipeline-gate-allowed-users=Alice, bob[bot]"}, want: []string{"alice", "bob[bot]"}},
		{name: "disable", args: []string{"--mark-pipeline-gate-allowed-users="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts gateAuthorizationOptions
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			opts.addFlags(fs)
			require.NoError(t, fs.Parse(tc.args))
			require.NoError(t, opts.validate())
			require.Equal(t, tc.want, opts.parsedAllowedUsers)
		})
	}
}

// gateTestTeams provides team membership even when the admin lookup fails.
type gateTestTeams struct{ *fakeAgenticGitHub }

func (gateTestTeams) HasPermission(_, _, _ string, _ ...string) (bool, error) {
	return false, context.DeadlineExceeded
}

func (gateTestTeams) ListTeamMembersBySlug(_, slug, role string) ([]github.TeamMember, error) {
	if slug == "team" && role == github.RoleAll {
		return []github.TeamMember{{Login: "alice"}}, nil
	}
	return nil, nil
}

// TestGateTeamScope prevents a repo-scoped permission from leaking to other repositories.
func TestGateTeamScope(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  bool
	}{{"org", true}, {"org/repo", true}, {"org/other", false}} {
		t.Run(tc.scope, func(t *testing.T) {
			got, err := gateTeamMember(&gateTestTeams{}, map[string][]string{tc.scope: {"team"}}, "org", "repo", "alice")
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestAgenticGateLookupError retries an indeterminate decision without marking the gate.
func TestAgenticGateLookupError(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		allowTeam  bool
		wantRetry  bool
	}{
		{name: "lookup error without another grant", user: "maintainer", wantRetry: true},
		{name: "explicitly allowed author", user: "redhat-chai-bot"},
		{name: "team grant despite admin lookup failure", user: "alice", allowTeam: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			f.a.checks.gh = gateTestTeams{fakeAgenticGitHub: f.gh}
			if tc.allowTeam {
				path := filepath.Join(t.TempDir(), "plugins.yaml")
				require.NoError(t, os.WriteFile(path, []byte("override: {allowed_github_teams: {org: [team]}}"), 0o600))
				f.a.checks.authorization.pluginConfigPath = path
			}
			command := f.command(10, "mark-pipeline-gate")
			command.User.Login = tc.user
			err := f.a.reconcilePull(t.Context(), f.work, command)
			if tc.wantRetry {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.True(t, agenticRetryFor(err).transient)
				require.Zero(t, f.gh.checkAttempts)
				return
			}
			require.NoError(t, err)
			require.Len(t, f.gh.checks, 1)
			require.Equal(t, "success", f.gh.checks[0].Conclusion)
		})
	}
}

// gateTestTransport exercises the real Prow request and GraphQL decoder without network access.
type gateTestTransport struct {
	t                    *testing.T
	sha, owners, aliases string
	base, returnedBase   string
	refFailure           string
	refStatus            int
}

func (g gateTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	require.Equal(g.t, "/graphql", req.URL.Path, "OWNERS lookups must not use the REST ref endpoint")
	_, bounded := req.Context().Deadline()
	require.True(g.t, bounded)
	var query struct{ Variables map[string]interface{} }
	require.NoError(g.t, json.NewDecoder(req.Body).Decode(&query))
	var body string
	if ref, ok := query.Variables["ref"]; ok {
		require.Equal(g.t, "refs/heads/"+g.base, ref)
		switch g.refFailure {
		case "timeout":
			<-req.Context().Done()
			return nil, req.Context().Err()
		case "rate limit":
			return &http.Response{Status: "403 Forbidden", StatusCode: http.StatusForbidden,
				Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader("rate limited")), Request: req}, nil
		}
		if g.refStatus != 0 {
			return &http.Response{Status: fmt.Sprintf("%d %s", g.refStatus, http.StatusText(g.refStatus)), StatusCode: g.refStatus,
				Header: http.Header{}, Body: io.NopCloser(strings.NewReader("lookup failed")), Request: req}, nil
		}
		body = fmt.Sprintf(`{"data":{"repository":{"ref":{"name":%q,"prefix":"refs/heads/","target":{"oid":%q}}}}}`, g.returnedBase, g.sha)
	} else {
		require.Equal(g.t, g.base, g.returnedBase, "do not read OWNERS after an unexpected ref")
		require.Equal(g.t, g.sha, query.Variables["sha"])
		require.Equal(g.t, g.sha+":OWNERS_ALIASES", query.Variables["aliases"])
		body = fmt.Sprintf(`{"data":{"repository":{"object":{"tree":{"entries":[{"name":"OWNERS","mode":33188},{"name":"OWNERS_ALIASES","mode":33188}]}},"owners":{"text":%q},"aliases":{"text":%q},"collaborators":{"nodes":[{"login":"owners-team"}]}}}}`, g.owners, g.aliases)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// TestAgenticGateGraphQLRetry checks which HTTP failures retain the command in the queue.
func TestAgenticGateGraphQLRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		delay  time.Duration
	}{
		{name: "rate limit", status: http.StatusTooManyRequests, delay: time.Minute},
		{name: "bad gateway", status: http.StatusBadGateway},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway timeout", status: http.StatusGatewayTimeout},
		{name: "permission denied", status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugins.yaml")
			require.NoError(t, os.WriteFile(path, []byte("override: {allow_top_level_owners: true}"), 0o600))
			_, _, gh, err := github.NewClientFromOptions(nil, github.ClientOptions{
				Bases: []string{"https://github.invalid"}, GraphqlEndpoint: "https://github.invalid/graphql",
				MaxRetries: 2, InitialDelay: time.Nanosecond,
				BaseRoundTripper: gateTestTransport{t: t, base: "main", refStatus: tc.status},
			})
			require.NoError(t, err)
			f := newAgenticFixture(t, "manual")
			f.gh.repoPermission = "read"
			f.a.checks.authorization = gateAuthorization{pluginConfigPath: path, owners: gh}
			f.a.queue = workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[agenticWork](time.Second, time.Minute))
			defer f.a.queue.ShutDown()
			cmd := f.command(10, "mark-pipeline-gate")
			f.a.enqueue(f.work.org, f.work.repo, f.work.number, cmd)
			require.True(t, f.a.processNext(t.Context()))
			require.Zero(t, f.gh.checkAttempts)
			input := f.a.inputs[f.work]
			if tc.status == http.StatusForbidden {
				require.Nil(t, input)
				return
			}
			require.NotNil(t, input)
			require.Equal(t, 1, input.retries)
			require.Equal(t, []github.IssueComment{*cmd}, input.comments)
			if tc.delay != 0 {
				require.Equal(t, f.now.Add(tc.delay), input.retryAt)
			}
		})
	}
}

// TestGateTopLevelOwners covers the bounded base lookup and fail-closed aliases.
func TestGateTopLevelOwners(t *testing.T) {
	for _, tc := range []struct {
		name, owners, aliases   string
		base, returned, failure string
		want                    bool
		wantErr                 string
	}{
		{name: "root approver", aliases: "aliases: {owners-team: [owners-team]}", want: true},
		{name: "advisory approver", owners: "advisory_approvers: [owners-team]", want: true},
		{name: "filtered advisory approver", owners: "filters:\n  '^\\.$': {advisory_approvers: [owners-team]}", want: true},
		{name: "alias login is not a member", aliases: "aliases: {owners-team: [alice]}"},
		{name: "malformed aliases cannot grant the literal login", aliases: "aliases: {owners-team: alice}", wantErr: "invalid OWNERS_ALIASES"},
		{name: "special characters in the branch are sent literally", base: "release/maint#2026", want: true},
		{name: "unexpected prefix match is rejected", base: "release#2026", returned: "release", wantErr: "could not verify the requested PR base branch"},
		{name: "GitHub timeout fails closed", failure: "timeout", wantErr: "context deadline exceeded"},
		{name: "rate limit does not extend the ref lookup", failure: "rate limit", wantErr: "403 Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugins.yaml")
			require.NoError(t, os.WriteFile(path, []byte("override: {allow_top_level_owners: true}"), 0o600))
			owners := tc.owners
			if owners == "" {
				owners = "filters:\n  '^\\.$': {approvers: [owners-team]}"
			}
			base := "main"
			if tc.base != "" {
				base = tc.base
			}
			returned := base
			if tc.returned != "" {
				returned = tc.returned
			}
			synctest.Test(t, func(t *testing.T) {
				_, _, gh, err := github.NewClientFromOptions(nil, github.ClientOptions{
					Bases: []string{"https://github.invalid"}, GraphqlEndpoint: "https://github.invalid/graphql",
					BaseRoundTripper: gateTestTransport{t: t, sha: strings.Repeat("a", 40), owners: owners, aliases: tc.aliases,
						base: base, returnedBase: returned, refFailure: tc.failure},
				})
				require.NoError(t, err)
				policy := gateAuthorization{pluginConfigPath: path, owners: gh}
				start := time.Now()
				got, err := policy.authorized(&fakeAgenticGitHub{}, "org", "repo", "owners-team", base)
				require.LessOrEqual(t, time.Since(start), 10*time.Second)
				require.Equal(t, tc.want, got)
				if tc.wantErr != "" {
					require.ErrorContains(t, err, tc.wantErr)
					if tc.failure == "timeout" {
						require.True(t, agenticRetryFor(err).transient)
					}
					return
				}
				require.NoError(t, err)
			})
		})
	}
}
