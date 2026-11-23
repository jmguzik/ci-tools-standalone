package main

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/plugins"
	"sigs.k8s.io/prow/pkg/repoowners"
)

// gateLoginRE matches exact GitHub user and App bot logins.
var gateLoginRE = regexp.MustCompile(`(?i)^[a-z0-9]+(-[a-z0-9]+)*(\[bot\])?$`)

// gateCommitRE prevents interpreting a base commit ID as a ref or revision expression.
var gateCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gateAuthorizationOptions configures exceptions and optional Prow policy for this command only.
type gateAuthorizationOptions struct {
	allowedUsers       string
	parsedAllowedUsers []string
	pluginConfigPath   string
}

func (o *gateAuthorizationOptions) addFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.allowedUsers, "mark-pipeline-gate-allowed-users", "redhat-chai-bot", "Comma-separated GitHub logins allowed to mark the gate in addition to Prow override permissions. Replaces the default; empty disables the list.")
	fs.StringVar(&o.pluginConfigPath, "plugin-config", "", "Path to Prow's plugin config for team and OWNERS gate authorization.")
}

func (o *gateAuthorizationOptions) validate() error {
	o.parsedAllowedUsers = nil
	if o.allowedUsers == "" {
		return nil
	}
	for _, user := range strings.Split(o.allowedUsers, ",") {
		user = strings.ToLower(strings.TrimSpace(user))
		if !gateLoginRE.MatchString(user) || len(strings.TrimSuffix(user, "[bot]")) > 39 {
			return fmt.Errorf("invalid --mark-pipeline-gate-allowed-users login %q: use an exact GitHub login", user)
		}
		o.parsedAllowedUsers = append(o.parsedAllowedUsers, user)
	}
	return nil
}

// gateAuthorizationClient is the GitHub read surface for admin and team eligibility.
type gateAuthorizationClient interface {
	HasPermission(org, repo, user string, roles ...string) (bool, error)
	ListTeams(org string) ([]github.Team, error)
	ListTeamMembersBySlug(org, teamSlug, role string) ([]github.TeamMember, error)
}

// gateOwnersClient provides cancellable reads for top-level OWNERS authorization.
type gateOwnersClient interface {
	GetRefWithContext(ctx context.Context, org, repo, ref string) (string, error)
	QueryWithGitHubAppsSupport(ctx context.Context, query interface{}, vars map[string]interface{}, org string) error
}

// gateAuthorization grants only /pipeline mark-pipeline-gate.
type gateAuthorization struct {
	allowedUsers     []string
	pluginConfigPath string
	owners           gateOwnersClient
	prowConfig       func() *config.Config
}

func (a gateAuthorization) authorized(gh gateAuthorizationClient, log *logrus.Entry, org, repo, user, base string) bool {
	if user == "" {
		return false
	}
	for _, allowed := range a.allowedUsers {
		if strings.EqualFold(user, allowed) {
			return true
		}
	}
	admin, err := gh.HasPermission(org, repo, user, github.RoleAdmin)
	if err != nil {
		log.WithError(err).Warn("Cannot check repository admin permission for pipeline gate")
	}
	if err == nil && admin {
		return true
	}
	if a.pluginConfigPath == "" {
		return false
	}
	// Re-read policy so a missing or invalid file cannot retain prior grants.
	var agent plugins.ConfigAgent
	if err := agent.Load(a.pluginConfigPath, nil, "", false, true); err != nil {
		log.Warn("Cannot read Prow plugin config for pipeline gate authorization")
		return false
	}
	pc := agent.Config()
	if gateTeamMember(gh, log, pc.Override.AllowedGitHubTeams, org, repo, user) {
		return true
	}
	if !pc.Override.AllowTopLevelOwners || a.owners == nil || base == "" {
		return false
	}
	ok, err := a.topLevelOwner(pc, org, repo, user, base)
	if err != nil {
		log.WithError(err).Warn("Cannot verify top-level OWNERS for pipeline gate")
	}
	return err == nil && ok
}

// gateTeamMember checks only the org and repository scopes recognized by /override.
func gateTeamMember(gh gateAuthorizationClient, log *logrus.Entry, teams map[string][]string, org, repo, user string) bool {
	orgTeams, repoTeams := teams[org], teams[org+"/"+repo]
	if len(orgTeams)+len(repoTeams) == 0 {
		return false
	}
	// Prow /override requires a successful org team listing before checking membership.
	if _, err := gh.ListTeams(org); err != nil {
		log.WithError(err).Warn("Cannot list teams for pipeline gate authorization")
		return false
	}
	for _, scope := range [][]string{repoTeams, orgTeams} {
		for _, slug := range scope {
			members, err := gh.ListTeamMembersBySlug(org, slug, github.RoleAll)
			if err != nil {
				log.WithError(err).WithField("team", slug).Warn("Cannot check pipeline gate team membership")
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

// gateOwnersBlob describes a small, complete GitHub text blob.
type gateOwnersBlob struct {
	Text                  *string
	IsBinary, IsTruncated bool
}

// gateOwnersQuery reads only the two policy blobs, root modes, and the caller's collaborator record.
type gateOwnersQuery struct {
	Repository struct {
		Object struct {
			Commit struct {
				Tree struct {
					Entries []struct {
						Name string
						Mode int
					}
				}
			} `graphql:"... on Commit"`
		} `graphql:"object(oid: $sha)"`
		Owners struct {
			Blob gateOwnersBlob `graphql:"... on Blob"`
		} `graphql:"owners: object(expression: $owners)"`
		Aliases struct {
			Blob gateOwnersBlob `graphql:"... on Blob"`
		} `graphql:"aliases: object(expression: $aliases)"`
		Collaborators struct{ Nodes []struct{ Login string } } `graphql:"collaborators(login: $login, first: 1) @include(if: $checkCollaborator)"`
	} `graphql:"repository(owner: $org, name: $repo)"`
}

func (a gateAuthorization) topLevelOwner(pc *plugins.Configuration, org, repo, user, base string) (bool, error) {
	names := pc.OwnersFilenames(org, repo)
	if names.Owners == "" || strings.ContainsAny(names.Owners+names.OwnersAliases, `/\`) ||
		(pc.MDYAMLEnabled(org, repo) && strings.HasSuffix(names.Owners, ".md")) {
		return false, nil
	}
	if a.prowConfig != nil {
		if cfg := a.prowConfig(); cfg != nil && cfg.OwnersDirDenylist != nil {
			for _, pattern := range cfg.OwnersDirDenylist.ListIgnoredDirs(org, repo) {
				if matches, err := regexp.MatchString(pattern, "."); err != nil || matches {
					return false, err
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sha, err := a.owners.GetRefWithContext(ctx, org, repo, "heads/"+base)
	if err != nil {
		return false, fmt.Errorf("resolving the PR base: %w", err)
	}
	if !gateCommitRE.MatchString(sha) {
		return false, fmt.Errorf("invalid PR base commit SHA")
	}
	login := github.NormLogin(user)
	checkCollaborator := !pc.SkipCollaborators(org, repo)
	var q gateOwnersQuery
	vars := map[string]interface{}{
		"org": githubv4.String(org), "repo": githubv4.String(repo), "sha": githubv4.GitObjectID(sha),
		"owners": githubv4.String(sha + ":" + names.Owners), "aliases": githubv4.String(sha + ":" + names.OwnersAliases),
		"login": githubv4.String(login), "checkCollaborator": githubv4.Boolean(checkCollaborator),
	}
	if err := a.owners.QueryWithGitHubAppsSupport(ctx, &q, vars, org); err != nil {
		return false, fmt.Errorf("loading root OWNERS files: %w", err)
	}
	if checkCollaborator {
		nodes := q.Repository.Collaborators.Nodes
		if len(nodes) != 1 || github.NormLogin(nodes[0].Login) != login {
			return false, nil
		}
	}
	var ownersFile, aliasesFile bool
	for _, entry := range q.Repository.Object.Commit.Tree.Entries {
		regular := entry.Mode == 0o100644 || entry.Mode == 0o100755
		if entry.Name == names.Owners {
			ownersFile = regular
		}
		if entry.Name == names.OwnersAliases {
			if !regular {
				return false, fmt.Errorf("OWNERS_ALIASES is not a regular file")
			}
			aliasesFile = true
		}
	}
	if !ownersFile {
		return false, nil
	}
	owners, aliases := q.Repository.Owners.Blob, q.Repository.Aliases.Blob
	if owners.Text == nil || owners.IsBinary || owners.IsTruncated ||
		(aliasesFile && (aliases.Text == nil || aliases.IsBinary || aliases.IsTruncated)) {
		return false, fmt.Errorf("incomplete OWNERS or OWNERS_ALIASES blob")
	}
	var aliasText string
	if aliasesFile {
		aliasText = *aliases.Text
	}
	return eligibleRootApprover(*owners.Text, aliasText, login)
}

// eligibleRootApprover uses Prow's parsers and matches filters against the root (".").
func eligibleRootApprover(owners, aliases, login string) (bool, error) {
	parsedAliases, err := repoowners.ParseAliasesConfig([]byte(aliases))
	if err != nil {
		return false, fmt.Errorf("invalid OWNERS_ALIASES: %w", err)
	}
	simple, err := repoowners.LoadSimpleConfig([]byte(owners))
	if err == nil && !simple.Empty() {
		return gateApprover(simple.Config, parsedAliases, login), nil
	}
	full, err := repoowners.LoadFullConfig([]byte(owners))
	if err != nil {
		return false, fmt.Errorf("invalid OWNERS: %w", err)
	}
	for pattern, rules := range full.Filters {
		if match, err := regexp.MatchString(pattern, "."); err == nil && match && gateApprover(rules, parsedAliases, login) {
			return true, nil
		}
	}
	return false, nil
}

// gateApprover includes advisory approvers because Prow counts them in TopLevelApprovers.
func gateApprover(rules repoowners.Config, aliases repoowners.RepoAliases, login string) bool {
	return aliases.ExpandAliases(repoowners.NormLogins(slices.Concat(rules.Approvers, rules.AdvisoryApprovers))).Has(login)
}
