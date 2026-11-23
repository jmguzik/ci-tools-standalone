package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
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
	QueryWithGitHubAppsSupport(ctx context.Context, query interface{}, vars map[string]interface{}, org string) error
}

// gateAuthorization grants only /pipeline mark-pipeline-gate.
type gateAuthorization struct {
	allowedUsers     []string
	pluginConfigPath string
	owners           gateOwnersClient
	prowConfig       func() *config.Config
}

func (a gateAuthorization) authorized(gh gateAuthorizationClient, org, repo, user, base string) (bool, error) {
	if user == "" {
		return false, nil
	}
	for _, allowed := range a.allowedUsers {
		if strings.EqualFold(user, allowed) {
			return true, nil
		}
	}
	admin, err := gh.HasPermission(org, repo, user, github.RoleAdmin)
	if err == nil && admin {
		return true, nil
	}
	if err != nil {
		err = fmt.Errorf("checking repository admin permission: %w", err)
	}
	if a.pluginConfigPath == "" {
		return false, err
	}
	// Re-read policy so a missing or invalid file cannot retain prior grants.
	var agent plugins.ConfigAgent
	if loadErr := agent.Load(a.pluginConfigPath, nil, "", false, true); loadErr != nil {
		// Parser errors can include raw configuration values.
		return false, utilerrors.NewAggregate([]error{err, errors.New("cannot read Prow plugin config for pipeline gate authorization")})
	}
	pc := agent.Config()
	member, teamErr := gateTeamMember(gh, pc.Override.AllowedGitHubTeams, org, repo, user)
	if member {
		return true, nil
	}
	var ownerErr error
	if pc.Override.AllowTopLevelOwners && a.owners != nil && base != "" {
		var owner bool
		owner, ownerErr = a.topLevelOwner(pc, org, repo, user, base)
		if ownerErr == nil && owner {
			return true, nil
		}
	}
	return false, utilerrors.NewAggregate([]error{err, teamErr, ownerErr})
}

// gateTeamMember checks only the org and repository scopes recognized by /override.
func gateTeamMember(gh gateAuthorizationClient, teams map[string][]string, org, repo, user string) (bool, error) {
	orgTeams, repoTeams := teams[org], teams[org+"/"+repo]
	if len(orgTeams)+len(repoTeams) == 0 {
		return false, nil
	}
	// Prow /override requires a successful org team listing before checking membership.
	if _, err := gh.ListTeams(org); err != nil {
		return false, fmt.Errorf("listing teams: %w", err)
	}
	var errs []error
	for _, scope := range [][]string{repoTeams, orgTeams} {
		for _, slug := range scope {
			members, err := gh.ListTeamMembersBySlug(org, slug, github.RoleAll)
			if err != nil {
				errs = append(errs, fmt.Errorf("checking membership of team %s: %w", slug, err))
				continue
			}
			for _, member := range members {
				if member.Login == user {
					return true, nil
				}
			}
		}
	}
	return false, utilerrors.NewAggregate(errs)
}

// gateBaseQuery resolves a literal branch name without REST path parsing or retry sleeps.
type gateBaseQuery struct {
	Repository struct {
		Ref *struct {
			Name, Prefix string
			Target       struct{ OID string }
		} `graphql:"ref(qualifiedName: $ref)"`
	} `graphql:"repository(owner: $org, name: $repo)"`
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
	var baseQuery gateBaseQuery
	baseVars := map[string]interface{}{
		"org": githubv4.String(org), "repo": githubv4.String(repo), "ref": githubv4.String("refs/heads/" + base),
	}
	if err := a.owners.QueryWithGitHubAppsSupport(ctx, &baseQuery, baseVars, org); err != nil {
		return false, fmt.Errorf("resolving the PR base: %w", err)
	}
	ref := baseQuery.Repository.Ref
	if ref == nil || ref.Prefix != "refs/heads/" || ref.Name != base {
		return false, fmt.Errorf("could not verify the requested PR base branch")
	}
	sha := ref.Target.OID
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
