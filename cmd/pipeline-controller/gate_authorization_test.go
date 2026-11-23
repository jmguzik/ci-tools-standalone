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

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
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

// gateTestTeams provides one matching membership for scope tests.
type gateTestTeams struct{ fakeAgenticGitHub }

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
			got := gateTeamMember(&gateTestTeams{}, logrus.NewEntry(logrus.New()), map[string][]string{tc.scope: {"team"}}, "org", "repo", "alice")
			require.Equal(t, tc.want, got)
		})
	}
}

// gateTestTransport exercises the real Prow request and GraphQL decoder without network access.
type gateTestTransport struct {
	t       *testing.T
	sha     string
	owners  string
	aliases string
	fail    bool
}

func (g gateTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	_, bounded := req.Context().Deadline()
	require.True(g.t, bounded)
	var body string
	switch req.URL.Path {
	case "/repos/org/repo/git/refs/heads/main":
		body = fmt.Sprintf(`{"object":{"sha":%q}}`, g.sha)
	case "/graphql":
		if g.fail {
			return nil, context.DeadlineExceeded
		}
		var query struct{ Variables map[string]interface{} }
		require.NoError(g.t, json.NewDecoder(req.Body).Decode(&query))
		require.Equal(g.t, g.sha, query.Variables["sha"])
		require.Equal(g.t, g.sha+":OWNERS_ALIASES", query.Variables["aliases"])
		body = fmt.Sprintf(`{"data":{"repository":{"object":{"tree":{"entries":[{"name":"OWNERS","mode":33188},{"name":"OWNERS_ALIASES","mode":33188}]}},"owners":{"text":%q},"aliases":{"text":%q},"collaborators":{"nodes":[{"login":"owners-team"}]}}}}`, g.owners, g.aliases)
	default:
		g.t.Fatalf("unexpected request path %s", req.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// TestGateTopLevelOwners covers the bounded base lookup and fail-closed aliases.
func TestGateTopLevelOwners(t *testing.T) {
	for _, tc := range []struct {
		name    string
		owners  string
		aliases string
		fail    bool
		want    bool
	}{
		{name: "root approver", aliases: "aliases: {owners-team: [owners-team]}", want: true},
		{name: "advisory approver", owners: "advisory_approvers: [owners-team]", want: true},
		{name: "filtered advisory approver", owners: "filters:\n  '^\\.$': {advisory_approvers: [owners-team]}", want: true},
		{name: "alias login is not a member", aliases: "aliases: {owners-team: [alice]}"},
		{name: "malformed aliases cannot grant the literal login", aliases: "aliases: {owners-team: alice}"},
		{name: "GitHub timeout fails closed", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugins.yaml")
			require.NoError(t, os.WriteFile(path, []byte("override: {allow_top_level_owners: true}"), 0o600))
			owners := tc.owners
			if owners == "" {
				owners = "filters:\n  '^\\.$': {approvers: [owners-team]}"
			}
			_, _, gh, err := github.NewClientFromOptions(nil, github.ClientOptions{
				Bases: []string{"https://github.invalid"}, GraphqlEndpoint: "https://github.invalid/graphql", MaxRetries: 1,
				BaseRoundTripper: gateTestTransport{t: t, sha: strings.Repeat("a", 40), owners: owners, aliases: tc.aliases, fail: tc.fail},
			})
			require.NoError(t, err)
			policy := gateAuthorization{pluginConfigPath: path, owners: gh}
			got := policy.authorized(&fakeAgenticGitHub{}, logrus.NewEntry(logrus.New()), "org", "repo", "owners-team", "main")
			require.Equal(t, tc.want, got)
		})
	}
}
