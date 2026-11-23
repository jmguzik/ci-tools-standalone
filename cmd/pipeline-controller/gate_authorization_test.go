package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/plugins"
	"sigs.k8s.io/prow/pkg/repoowners"
)

// TestGateAllowedUsersFlag verifies that explicit lists replace the default grant.
func TestGateAllowedUsersFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
		err  string
	}{
		{name: "default", want: []string{"redhat-chai-bot"}},
		{name: "replacement list", args: []string{"--mark-pipeline-gate-allowed-users=Alice, another-app[bot]"}, want: []string{"Alice", "another-app[bot]"}},
		{name: "disabled", args: []string{"--mark-pipeline-gate-allowed-users="}},
		{name: "profile URL", args: []string{"--mark-pipeline-gate-allowed-users=https://github.com/alice"}, err: "use an exact GitHub login"},
		{name: "empty entry", args: []string{"--mark-pipeline-gate-allowed-users=alice,"}, err: "use an exact GitHub login"},
		{name: "malformed bot suffix", args: []string{"--mark-pipeline-gate-allowed-users=alice[anything]"}, err: "use an exact GitHub login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var options gateAuthorizationOptions
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			options.addFlags(fs)
			require.NoError(t, fs.Parse(tc.args))
			err := options.validate()
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			if diff := cmp.Diff(tc.want, options.parsedAllowedUsers); diff != "" {
				t.Errorf("unexpected allowed users (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGatePluginConfigRevocation checks that stale policy never authorizes a later command.
func TestGatePluginConfigRevocation(t *testing.T) {
	const allowedTeamConfig = "override:\n  allowed_github_teams:\n    org: [ops]\n"
	for _, tc := range []struct {
		name           string
		updated        string
		missing        bool
		missingAtStart bool
		want           bool
	}{
		{name: "unchanged policy", updated: allowedTeamConfig, want: true},
		{name: "removed grant", updated: "override: {}\n"},
		{name: "malformed policy", updated: "override: ["},
		{name: "missing config file", missing: true},
		{name: "config appears after startup", updated: allowedTeamConfig, missingAtStart: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "plugins.yaml")
			if !tc.missingAtStart {
				require.NoError(t, os.WriteFile(path, []byte(allowedTeamConfig), 0o600))
			}
			o := options{gateAuthorization: gateAuthorizationOptions{pluginConfigPath: path}}
			o.config.InRepoConfigCacheDirBase = dir
			policy, err := o.newGateAuthorization(nil, nil)
			require.NoError(t, err)
			gh := &fakeGateAuthorizationClient{t: t}
			logger := logrus.NewEntry(logrus.New())
			require.Equal(t, !tc.missingAtStart, policy.authorized(gh, logger, "org", "repo", "alice", "main"))
			if tc.missing {
				require.NoError(t, os.Remove(path))
			}
			if !tc.missing {
				require.NoError(t, os.WriteFile(path, []byte(tc.updated), 0o600))
			}
			require.Equal(t, tc.want, policy.authorized(gh, logger, "org", "repo", "alice", "main"))
		})
	}
}

// fakeGateAuthorizationClient records authorization lookups and simulates API failures.
type fakeGateAuthorizationClient struct {
	t         *testing.T
	admin     bool
	adminErr  error
	teamsErr  error
	memberErr error
	teamCalls []string
	adminCall bool
}

// HasPermission verifies the exact permission requested from GitHub.
func (f *fakeGateAuthorizationClient) HasPermission(org, repo, login string, roles ...string) (bool, error) {
	f.adminCall = true
	require.Equal(f.t, "org", org)
	require.Equal(f.t, "repo", repo)
	require.NotEmpty(f.t, login)
	require.Equal(f.t, []string{github.RoleAdmin}, roles)
	return f.admin, f.adminErr
}

// ListTeams can simulate missing access to an organization's teams.
func (f *fakeGateAuthorizationClient) ListTeams(org string) ([]github.Team, error) {
	require.Equal(f.t, "org", org)
	return []github.Team{{Slug: "ops"}, {Slug: "dev"}}, f.teamsErr
}

// ListTeamMembersBySlug returns the actor only for the ops team, including on errors.
func (f *fakeGateAuthorizationClient) ListTeamMembersBySlug(org, slug, role string) ([]github.TeamMember, error) {
	require.Equal(f.t, "org", org)
	require.Equal(f.t, github.RoleAll, role)
	f.teamCalls = append(f.teamCalls, slug)
	if slug == "ops" {
		return []github.TeamMember{{Login: "alice"}}, f.memberErr
	}
	return nil, nil
}

// fakeGateOwnersClient checks that only OWNERS at the base ref can authorize the command.
type fakeGateOwnersClient struct {
	t   *testing.T
	err error
}

// LoadRepoOwners returns top-level approvers for the test PR base ref.
func (f fakeGateOwnersClient) LoadRepoOwners(org, repo, base, _ string, _ *plugins.Configuration) (repoowners.RepoOwner, error) {
	require.Equal(f.t, "org", org)
	require.Equal(f.t, "repo", repo)
	require.Equal(f.t, "release", base)
	return fakeGateRepoOwners{}, f.err
}

// fakeGateRepoOwners only implements the root approvers lookup used for authorization.
type fakeGateRepoOwners struct{ repoowners.RepoOwner }

// TopLevelApprovers returns a different user from the fake team member.
func (fakeGateRepoOwners) TopLevelApprovers() sets.Set[string] { return sets.New("bob") }

// fakeGatePluginConfig holds settings for policy tests without reading deployment files.
type fakeGatePluginConfig struct{ config *plugins.Configuration }

func (f fakeGatePluginConfig) load() (*plugins.Configuration, error) { return f.config, nil }

// TestGateAuthorization exercises all grant paths and denies cross-repo grants and failed lookups.
func TestGateAuthorization(t *testing.T) {
	unavailable := errors.New("unavailable")
	for _, tc := range []struct {
		name      string
		user      string
		allowed   []string
		admin     bool
		adminErr  error
		teams     map[string][]string
		teamsErr  error
		memberErr error
		owners    bool
		ownersErr error
		teamCalls []string
		skipAdmin bool
		want      bool
	}{
		{name: "explicit allowlist skips GitHub", user: "ALICE", allowed: []string{"alice"}, skipAdmin: true, want: true},
		{name: "allowlist does not infer bot suffix", user: "alice[bot]", allowed: []string{"alice"}},
		{name: "empty author", admin: true, skipAdmin: true},
		{name: "repository admin", user: "alice", admin: true, want: true},
		{name: "non-admin", user: "alice"},
		{name: "admin lookup error", user: "alice", admin: true, adminErr: unavailable},
		{name: "repository team with admin error", user: "alice", adminErr: unavailable, teams: map[string][]string{"org/repo": {"ops"}}, teamCalls: []string{"ops"}, want: true},
		{name: "organization team after repository miss", user: "alice", teams: map[string][]string{"org/repo": {"dev"}, "org": {"ops"}}, teamCalls: []string{"dev", "ops"}, want: true},
		{name: "team scoped to different repository", user: "alice", teams: map[string][]string{"org/other": {"ops"}}},
		{name: "failed org team list", user: "alice", teams: map[string][]string{"org": {"ops"}}, teamsErr: unavailable},
		{name: "failed team member list", user: "alice", teams: map[string][]string{"org": {"ops"}}, memberErr: unavailable, teamCalls: []string{"ops"}},
		{name: "root approver after failed team list", user: "BOB", teams: map[string][]string{"org": {"ops"}}, teamsErr: unavailable, owners: true, want: true},
		{name: "root approver support disabled", user: "bob"},
		{name: "root approver lookup failed", user: "bob", owners: true, ownersErr: unavailable},
		{name: "not a root approver", user: "alice", owners: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := fakeGatePluginConfig{config: &plugins.Configuration{Override: plugins.Override{AllowTopLevelOwners: tc.owners, AllowedGitHubTeams: tc.teams}}}
			gh := &fakeGateAuthorizationClient{t: t, admin: tc.admin, adminErr: tc.adminErr, teamsErr: tc.teamsErr, memberErr: tc.memberErr}
			a := gateAuthorization{allowedUsers: tc.allowed, pluginConfig: pc.load, owners: fakeGateOwnersClient{t: t, err: tc.ownersErr}}
			got := a.authorized(gh, logrus.NewEntry(logrus.New()), "org", "repo", tc.user, "release")
			require.Equal(t, tc.want, got)
			require.Equal(t, !tc.skipAdmin, gh.adminCall)
			if diff := cmp.Diff(tc.teamCalls, gh.teamCalls); diff != "" {
				t.Errorf("unexpected team lookups (-want +got):\n%s", diff)
			}
		})
	}
}
