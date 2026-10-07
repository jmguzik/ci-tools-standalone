package main

import "regexp"

const pipelineHelp = "Pipeline commands:\n\n" +
	"- `/pipeline help` — list commands.\n" +
	"- `/pipeline required` — rerun the required/selected second-stage tests.\n" +
	"- `/pipeline remaining` — run missing second-stage tests.\n" +
	"- `/pipeline auto` — enable automatic dispatch in LGTM mode.\n" +
	"- `/pipeline agent-review` — request a fresh Chai plan before dispatch (agentic only).\n" +
	"- `/pipeline skip-agent-review` — use normal job selection (agentic only).\n" +
	"- `/pipeline tests-dispatched` — manually mark `ci/tests-dispatched` successful; does not run tests or override their results (members/collaborators only)."

var (
	pipelineHelpRE            = regexp.MustCompile(`(?im)^/pipeline[\t ]+help[\t ]*$`)
	pipelineTestsDispatchedRE = regexp.MustCompile(`(?im)^/pipeline[\t ]+tests-dispatched[\t ]*$`)
)

type pipelineCommandClient interface {
	IsMember(org, user string) (bool, error)
	IsCollaborator(org, repo, user string) (bool, error)
}

func trustedPipelineCommandAuthor(gh pipelineCommandClient, org, repo, login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	member, err := gh.IsMember(org, login)
	if err != nil || member {
		return member, err
	}
	return gh.IsCollaborator(org, repo, login)
}
