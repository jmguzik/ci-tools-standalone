package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/plugins"
	"sigs.k8s.io/prow/pkg/plugins/ownersconfig"
	"sigs.k8s.io/prow/pkg/repoowners"
)

// fakeGateOwnersGitHub implements only the two lookups used by the OWNERS path.
type fakeGateOwnersGitHub struct {
	github.Client
	t                 *testing.T
	sha               string
	collaborators     []github.User
	collaboratorsErr  error
	collaboratorCalls int
}

func (f *fakeGateOwnersGitHub) GetRef(org, repo, ref string) (string, error) {
	require.Equal(f.t, "org", org)
	require.Equal(f.t, "repo", repo)
	require.Equal(f.t, "heads/release", ref)
	return f.sha, nil
}

func (f *fakeGateOwnersGitHub) ListCollaborators(org, repo string) ([]github.User, error) {
	require.Equal(f.t, "org", org)
	require.Equal(f.t, "repo", repo)
	f.collaboratorCalls++
	return f.collaborators, f.collaboratorsErr
}

// fakeGateOwnersGit pins the fake repository's requested commit and provides fixture files.
type fakeGateOwnersGit struct {
	git.ClientFactory
	t        *testing.T
	repo     *fakeGateOwnersRepo
	cloneFor string
}

func (f *fakeGateOwnersGit) ClientForWithRepoOpts(org, repo string, opts git.RepoOpts) (git.RepoClient, error) {
	require.Equal(f.t, "org", org)
	require.Equal(f.t, "repo", repo)
	require.Equal(f.t, sets.New(f.cloneFor), opts.NeededCommits)
	return f.repo, nil
}

// fakeGateOwnersRepo records checkout and can simulate a stale local branch or tag.
type fakeGateOwnersRepo struct {
	git.RepoClient
	t          *testing.T
	dir        string
	head       string
	checkedOut string
}

func (f *fakeGateOwnersRepo) Checkout(revision string) error {
	f.checkedOut = revision
	return nil
}

func (f *fakeGateOwnersRepo) RevParse(revision string) (string, error) {
	require.Equal(f.t, "HEAD", revision)
	return f.head + "\n", nil
}

func (f *fakeGateOwnersRepo) Directory() string { return f.dir }
func (f *fakeGateOwnersRepo) Clean() error      { return nil }

// fakeGateProwConfig provides the repository-wide OWNERS parsing defaults.
type fakeGateProwConfig struct{}

func (fakeGateProwConfig) read() *config.Config { return &config.Config{} }

// blockingGateOwners keeps a lookup in flight until it is explicitly released.
type blockingGateOwners struct{ unblock chan struct{} }

func (b blockingGateOwners) LoadRepoOwners(_, _, _, _ string, _ *plugins.Configuration) (repoowners.RepoOwner, error) {
	<-b.unblock
	return fakeGateRepoOwners{}, nil
}

// TestLimitedGateOwners ensures a slow Git operation cannot hold the caller indefinitely.
func TestLimitedGateOwners(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout bool
	}{
		{name: "completed lookup"},
		{name: "expired lookup retains its slot", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loader := blockingGateOwners{unblock: make(chan struct{})}
			policy := limitedGateOwners{loader: loader, inFlight: make(chan struct{}, 1)}
			if !tc.timeout {
				close(loader.unblock)
				owners, err := policy.loadBefore(nil, "org", "repo", "main", "bob", nil)
				require.NoError(t, err)
				require.True(t, owners.TopLevelApprovers().Has("bob"))
				return
			}
			defer close(loader.unblock)
			deadline := make(chan time.Time, 1)
			deadline <- time.Time{}
			owners, err := policy.loadBefore(deadline, "org", "repo", "main", "bob", nil)
			require.ErrorContains(t, err, "timed out")
			require.Nil(t, owners)
			owners, err = policy.loadBefore(nil, "org", "repo", "main", "bob", nil)
			require.ErrorContains(t, err, "lookup is busy")
			require.Nil(t, owners)
		})
	}
}

// TestGateOwnersLoader verifies that checkout, collaborator and parsing decisions fail closed.
func TestGateOwnersLoader(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name              string
		staleCheckout     bool
		invalidRemoteRef  bool
		collaboratorErr   error
		skipCollaborators bool
		notCollaborator   bool
		changeFilenames   bool
		wantCheckout      bool
		want              bool
		wantErr           string
	}{
		{name: "exact live base", wantCheckout: true, want: true},
		{name: "stale checkout", staleCheckout: true, wantCheckout: true, wantErr: "checkout does not match the resolved PR base commit"},
		{name: "invalid remote ref", invalidRemoteRef: true, wantErr: "did not resolve to a full commit SHA"},
		{name: "not a collaborator", notCollaborator: true},
		{name: "failed collaborator lookup", collaboratorErr: errors.New("unavailable"), wantErr: "listing repository collaborators: unavailable"},
		{name: "explicitly skip collaborator lookup", skipCollaborators: true, collaboratorErr: errors.New("unavailable"), wantCheckout: true, want: true},
		{name: "changed OWNERS filenames at same SHA", wantCheckout: true, want: true, changeFilenames: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "OWNERS"), []byte("approvers: [bob]\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "GATE_OWNERS"), []byte("approvers: [another-user]\n"), 0o600))
			repo := &fakeGateOwnersRepo{t: t, dir: dir, head: sha}
			if tc.staleCheckout {
				repo.head = strings.Repeat("b", 40)
			}
			gh := &fakeGateOwnersGitHub{t: t, sha: sha, collaboratorsErr: tc.collaboratorErr}
			if !tc.notCollaborator {
				gh.collaborators = []github.User{{Login: "Bob"}}
			}
			if tc.invalidRemoteRef {
				gh.sha = "release"
			}
			pc := &plugins.Configuration{}
			if tc.skipCollaborators {
				pc.Owners.SkipCollaborators = []string{"org/repo"}
			}
			gitClient := &fakeGateOwnersGit{t: t, repo: repo, cloneFor: sha}
			loader := gateOwnersLoader{gitClient: gitClient, gh: gh, prowConfig: fakeGateProwConfig{}.read}
			owners, err := loader.LoadRepoOwners("org", "repo", "release", "bob", pc)
			wantCheckout := ""
			if tc.wantCheckout {
				wantCheckout = sha
			}
			require.Equal(t, wantCheckout, repo.checkedOut)
			wantCollaboratorCalls := 1
			if tc.skipCollaborators {
				wantCollaboratorCalls = 0
			}
			require.Equal(t, wantCollaboratorCalls, gh.collaboratorCalls)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, owners)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, owners != nil && owners.TopLevelApprovers().Has("bob"))
			if tc.changeFilenames {
				pc.Owners.Filenames = map[string]ownersconfig.Filenames{"org/repo": {Owners: "GATE_OWNERS", OwnersAliases: "OWNERS_ALIASES"}}
				owners, err = loader.LoadRepoOwners("org", "repo", "release", "bob", pc)
				require.NoError(t, err)
				require.NotNil(t, owners)
				require.False(t, owners.TopLevelApprovers().Has("bob"), "changed policy must not use previously parsed OWNERS")
			}
		})
	}
}
