package main

import (
	"flag"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/prow/pkg/config"
	git "sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/plugins"
	"sigs.k8s.io/prow/pkg/plugins/ownersconfig"
	"sigs.k8s.io/prow/pkg/repoowners"
)

// defaultGateAllowedUser is the only login allowed without Prow override permissions by default.
const defaultGateAllowedUser = "redhat-chai-bot"

// gateOwnersWait bounds the time a shared command handler can wait on Prow's uncancellable Git operations.
const gateOwnersWait = 10 * time.Second

// gateLoginRE accepts GitHub usernames and exact GitHub App bot logins.
var gateLoginRE = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*(\[[Bb][Oo][Tt]\])?$`)

// gateCommitSHARe restricts Git operations to a full GitHub commit ID, not a ref or option.
var gateCommitSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gateAuthorizationOptions configures the gate command independently of agentic author trust.
type gateAuthorizationOptions struct {
	allowedUsers       string
	pluginConfigPath   string
	parsedAllowedUsers []string
}

func (o *gateAuthorizationOptions) addFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.allowedUsers, "mark-pipeline-gate-allowed-users", defaultGateAllowedUser, "Comma-separated GitHub logins additionally allowed to mark the pipeline gate (case-insensitive). An explicit value replaces the default; empty disables the allowlist.")
	fs.StringVar(&o.pluginConfigPath, "plugin-config", "", "Optional path to Prow's plugin config for team and top-level OWNERS authorization of the pipeline gate command.")
}

func (o *gateAuthorizationOptions) validate() error {
	users, err := o.parseAllowedUsers()
	o.parsedAllowedUsers = users
	return err
}

func (o *gateAuthorizationOptions) parseAllowedUsers() ([]string, error) {
	if o.allowedUsers == "" {
		return nil, nil
	}
	users := strings.Split(o.allowedUsers, ",")
	for i, user := range users {
		user = strings.TrimSpace(user)
		if !gateLoginRE.MatchString(user) || len(strings.TrimSuffix(strings.ToLower(user), "[bot]")) > 39 {
			return nil, fmt.Errorf("invalid --mark-pipeline-gate-allowed-users login %q: use an exact GitHub login", user)
		}
		users[i] = user
	}
	return users, nil
}

// gateAuthorizationClient is the GitHub read surface used by Prow's /override authorization.
type gateAuthorizationClient interface {
	HasPermission(org, repo, user string, role ...string) (bool, error)
	ListTeams(org string) ([]github.Team, error)
	ListTeamMembersBySlug(org, teamSlug, role string) ([]github.TeamMember, error)
}

// gateOwnersClient loads Prow OWNERS data at the target branch, not the untrusted PR head.
type gateOwnersClient interface {
	LoadRepoOwners(org, repo, base, user string, pluginConfig *plugins.Configuration) (repoowners.RepoOwner, error)
}

// limitedGateOwners bounds concurrent OWNERS work and the time it can hold up command processing.
type limitedGateOwners struct {
	loader   gateOwnersClient
	inFlight chan struct{}
}

// gateOwnersResult carries the completed lookup without blocking if the caller timed out.
type gateOwnersResult struct {
	owners repoowners.RepoOwner
	err    error
}

func (l limitedGateOwners) LoadRepoOwners(org, repo, base, user string, pluginConfig *plugins.Configuration) (repoowners.RepoOwner, error) {
	timer := time.NewTimer(gateOwnersWait)
	defer timer.Stop()
	return l.loadBefore(timer.C, org, repo, base, user, pluginConfig)
}

func (l limitedGateOwners) loadBefore(deadline <-chan time.Time, org, repo, base, user string, pluginConfig *plugins.Configuration) (repoowners.RepoOwner, error) {
	select {
	case l.inFlight <- struct{}{}:
	default:
		return nil, fmt.Errorf("OWNERS authorization lookup is busy")
	}
	result := make(chan gateOwnersResult, 1)
	go l.lookup(result, org, repo, base, user, pluginConfig)
	select {
	case done := <-result:
		return done.owners, done.err
	case <-deadline:
		return nil, fmt.Errorf("OWNERS authorization lookup timed out")
	}
}

func (l limitedGateOwners) lookup(result chan<- gateOwnersResult, org, repo, base, user string, pluginConfig *plugins.Configuration) {
	owners, err := l.loader.LoadRepoOwners(org, repo, base, user, pluginConfig)
	<-l.inFlight
	result <- gateOwnersResult{owners: owners, err: err}
}

// gateOwnersLoader shares the Git object cache, but never reuses parsed owners across grants.
type gateOwnersLoader struct {
	gitClient  git.ClientFactory
	gh         github.Client
	prowConfig func() *config.Config
}

func (l gateOwnersLoader) LoadRepoOwners(org, repo, base, user string, pluginConfig *plugins.Configuration) (repoowners.RepoOwner, error) {
	if pluginConfig == nil {
		return nil, fmt.Errorf("missing Prow plugin config for OWNERS authorization")
	}
	if !pluginConfig.SkipCollaborators(org, repo) {
		collaborators, err := l.gh.ListCollaborators(org, repo)
		if err != nil {
			return nil, fmt.Errorf("listing repository collaborators: %w", err)
		}
		if !gateOwnersCollaborator(user, collaborators) {
			return nil, nil
		}
	}
	sha, err := l.gh.GetRef(org, repo, "heads/"+base)
	if err != nil {
		return nil, fmt.Errorf("resolving PR base branch: %w", err)
	}
	if !gateCommitSHARe.MatchString(sha) {
		return nil, fmt.Errorf("PR base branch did not resolve to a full commit SHA")
	}
	c := gateOwnersConfig{pluginConfig: pluginConfig}
	if l.prowConfig != nil {
		c.prowConfig = l.prowConfig()
	}
	factory := gateOwnersGitFactory{ClientFactory: l.gitClient, sha: sha}
	client := repoowners.NewClient(factory, l.gh, c.mdYAMLEnabled, skipRepoOwnersCollaboratorFilter, c.dirDenylist, c.filenames)
	return client.LoadRepoOwnersSha(org, repo, base, sha, false)
}

// gateOwnersCollaborator matches repoowners' normalization for the caller's eligibility.
func gateOwnersCollaborator(user string, collaborators []github.User) bool {
	login := github.NormLogin(user)
	for _, collaborator := range collaborators {
		if github.NormLogin(collaborator.Login) == login {
			return true
		}
	}
	return false
}

// skipRepoOwnersCollaboratorFilter avoids a second check whose error Prow would ignore.
// The loader has already checked the author unless the Prow configuration opts out.
func skipRepoOwnersCollaboratorFilter(_, _ string) bool {
	return true
}

// gateOwnersGitFactory makes Prow's checkout of a branch name use the resolved commit instead.
type gateOwnersGitFactory struct {
	git.ClientFactory
	sha string
}

func (f gateOwnersGitFactory) ClientFor(org, repo string) (git.RepoClient, error) {
	client, err := f.ClientForWithRepoOpts(org, repo, git.RepoOpts{NeededCommits: sets.New(f.sha)})
	if err != nil {
		return nil, err
	}
	return gateOwnersGitRepo{RepoClient: client, sha: f.sha}, nil
}

// gateOwnersGitRepo verifies that the files Prow parses come from the resolved commit.
type gateOwnersGitRepo struct {
	git.RepoClient
	sha string
}

func (r gateOwnersGitRepo) Checkout(_ string) error {
	if err := r.RepoClient.Checkout(r.sha); err != nil {
		return fmt.Errorf("checking out PR base commit: %w", err)
	}
	head, err := r.RevParse("HEAD")
	if err != nil {
		return fmt.Errorf("verifying PR base checkout: %w", err)
	}
	if strings.TrimSpace(head) != r.sha {
		return fmt.Errorf("checkout does not match the resolved PR base commit")
	}
	return nil
}

// gateAuthorization grants only /pipeline mark-pipeline-gate, in either scheduling mode.
type gateAuthorization struct {
	allowedUsers []string
	pluginConfig func() (*plugins.Configuration, error)
	owners       gateOwnersClient
}

func (a gateAuthorization) authorized(gh gateAuthorizationClient, log *logrus.Entry, org, repo, user, base string) bool {
	if user == "" {
		return false
	}
	for _, allowed := range a.allowedUsers {
		if strings.EqualFold(allowed, user) {
			return true
		}
	}
	log = log.WithFields(logrus.Fields{"org": org, "repo": repo, "user": user})
	admin, err := gh.HasPermission(org, repo, user, github.RoleAdmin)
	if err != nil {
		log.WithError(err).Warn("Cannot determine whether the pipeline gate command author is a repository administrator")
	}
	if err == nil && admin {
		return true
	}
	if a.pluginConfig == nil {
		return false
	}
	plugConfig, err := a.pluginConfig()
	if err != nil {
		// YAML errors can quote configuration values. Do not log them here.
		log.Warn("Cannot load Prow plugin config to authorize the pipeline gate command")
		return false
	}
	if plugConfig == nil {
		return false
	}
	options := plugConfig.Override
	if authorizedGateTeamMember(gh, log, org, repo, user, options.AllowedGitHubTeams) {
		return true
	}
	if !options.AllowTopLevelOwners || a.owners == nil || base == "" {
		return false
	}
	owners, err := a.owners.LoadRepoOwners(org, repo, base, user, plugConfig)
	if err != nil {
		log.WithError(err).Warn("Cannot load OWNERS to authorize the pipeline gate command")
		return false
	}
	return owners != nil && owners.TopLevelApprovers().Has(github.NormLogin(user))
}

// authorizedGateTeamMember mirrors /override's team eligibility, excluding diagnostic-only slug validation.
func authorizedGateTeamMember(gh gateAuthorizationClient, log *logrus.Entry, org, repo, user string, teams map[string][]string) bool {
	repoTeams, orgTeams := teams[org+"/"+repo], teams[org]
	if len(repoTeams)+len(orgTeams) == 0 {
		return false
	}
	// Prow's /override fails this authorization branch if the org team listing fails.
	if _, err := gh.ListTeams(org); err != nil {
		log.WithError(err).Warn("Cannot list GitHub teams to authorize the pipeline gate command")
		return false
	}
	for _, scope := range [][]string{repoTeams, orgTeams} {
		for _, team := range scope {
			members, err := gh.ListTeamMembersBySlug(org, team, github.RoleAll)
			if err != nil {
				log.WithError(err).WithField("team", team).Warn("Cannot list GitHub team members to authorize the pipeline gate command")
				continue
			}
			for _, member := range members {
				if member.Login == user {
					return true
				}
			}
		}
	}
	return false
}

// gateOwnersConfig adapts the Prow configuration snapshots for a single grant check.
type gateOwnersConfig struct {
	pluginConfig *plugins.Configuration
	prowConfig   *config.Config
}

func (c gateOwnersConfig) mdYAMLEnabled(org, repo string) bool {
	return c.pluginConfig.MDYAMLEnabled(org, repo)
}

func (c gateOwnersConfig) dirDenylist() *config.OwnersDirDenylist {
	if c.prowConfig != nil && c.prowConfig.OwnersDirDenylist != nil {
		return c.prowConfig.OwnersDirDenylist
	}
	return &config.OwnersDirDenylist{}
}

func (c gateOwnersConfig) filenames(org, repo string) ownersconfig.Filenames {
	return c.pluginConfig.OwnersFilenames(org, repo)
}

// gatePluginConfigFile uses a fresh agent per read so invalid config never retains previous grants.
type gatePluginConfigFile string

func (path gatePluginConfigFile) load() (*plugins.Configuration, error) {
	var agent plugins.ConfigAgent
	// The controller does not register or execute plugins or resolve ConfigUpdater paths.
	if err := agent.Load(string(path), nil, "", false, true); err != nil {
		return nil, fmt.Errorf("loading Prow plugin configuration: %w", err)
	}
	return agent.Config(), nil
}

// newGateAuthorization wires the plugin config and OWNERS loaders only when configured.
func (o *options) newGateAuthorization(gh github.Client, prowConfig func() *config.Config) (gateAuthorization, error) {
	a := gateAuthorization{allowedUsers: o.gateAuthorization.parsedAllowedUsers}
	path := gatePluginConfigFile(o.gateAuthorization.pluginConfigPath)
	if path == "" {
		return a, nil
	}
	gitClient, err := o.github.GitClientFactory("", &o.config.InRepoConfigCacheDirBase, o.dryrun, false)
	if err != nil {
		return gateAuthorization{}, fmt.Errorf("creating git client for gate OWNERS authorization: %w", err)
	}
	a.pluginConfig = path.load
	a.owners = limitedGateOwners{
		loader:   gateOwnersLoader{gitClient: gitClient, gh: gh, prowConfig: prowConfig},
		inFlight: make(chan struct{}, 1),
	}
	return a, nil
}
