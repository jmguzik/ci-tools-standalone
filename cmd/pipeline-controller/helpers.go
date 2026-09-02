package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
)

// scheduleMode selects how second-stage tests are scheduled.
type scheduleMode int

const (
	// modeForce schedules the entire required set, re-firing jobs that already
	// exist at HEAD. Used only by the explicit /pipeline required command.
	modeForce scheduleMode = iota
	// modeDelta schedules only the required jobs that do not yet have a ProwJob
	// at HEAD (the delta). Used by automatic, LGTM, and /pipeline remaining.
	modeDelta
)

type minimalGhClient interface {
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	CreateComment(org, repo string, number int, comment string) error
	GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error)
	CreateStatus(org, repo, ref string, s github.Status) error
	AddLabel(org, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetFile(org, repo, filepath, commit string) ([]byte, error)
}

type cachedFileResult struct {
	content []byte
	err     error
}

type fileCachingGhClient struct {
	minimalGhClient
	files map[string]cachedFileResult
}

func newFileCachingGhClient(ghc minimalGhClient) minimalGhClient {
	return &fileCachingGhClient{minimalGhClient: ghc, files: make(map[string]cachedFileResult)}
}

func (c *fileCachingGhClient) GetFile(org, repo, filepath, commit string) ([]byte, error) {
	key := org + "\x00" + repo + "\x00" + filepath + "\x00" + commit
	if result, ok := c.files[key]; ok {
		return result.content, result.err
	}
	content, err := c.minimalGhClient.GetFile(org, repo, filepath, commit)
	c.files[key] = cachedFileResult{content: content, err: err}
	return content, err
}

func newDeferredChangedFilesProvider(ghc minimalGhClient, org, repo string, number int) config.ChangedFilesProvider {
	var (
		fetched bool
		files   []string
		err     error
	)
	return func() ([]string, error) {
		if !fetched {
			fetched = true
			var changes []github.PullRequestChange
			changes, err = ghc.GetPullRequestChanges(org, repo, number)
			if err != nil {
				err = fmt.Errorf("error getting pull request changes: %w", err)
			} else {
				files = changedFilePaths(changes)
			}
		}
		return files, err
	}
}

func sendComment(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader) error {
	return sendCommentWithMode(presubmits, pj, ghc, deleteIds, pjLister, modeDelta)
}

func sendCommentWithMode(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader, mode scheduleMode) error {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		deleteIds()
		return fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	// Combine pipelineConditionallyRequired and pipelineSkipOnlyRequired for processing
	allConditionalTests := append([]config.Presubmit{}, presubmits.pipelineConditionallyRequired...)
	allConditionalTests = append(allConditionalTests, presubmits.pipelineSkipOnlyRequired...)

	testContexts, err := acquireConditionalContexts(context.Background(), pj, allConditionalTests, ghc, deleteIds, pjLister, mode)
	if err != nil {
		// In modeDelta a list error fails closed: schedule nothing and post no
		// comment. deleteIds() lets the automatic path retry on the next event
		// (and, because no comment is posted here, does not defeat the
		// one-comment-per-SHA guarantee).
		deleteIds()
		return err
	}

	var comment string

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef

	var protectedCommands string
	for _, presubmit := range presubmits.protected {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		// Skip re-triggering if a ProwJob already exists at the same SHA
		// (unless this is an explicit /pipeline required command).
		if mode == modeDelta && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
			if existsAtSHA(context.Background(), pjLister, pj, presubmit.Name) {
				continue
			}
		}
		protectedCommands += "\n" + presubmit.RerunCommand
	}
	if protectedCommands != "" {
		comment += "Scheduling required tests:" + protectedCommands
	}
	if testContexts != "" {
		if protectedCommands != "" {
			comment += "\n"
		}
		comment += "\nScheduling tests matching the `pipeline_run_if_changed`, `pipeline_run_if_dockerfile_changed`, or not excluded by `pipeline_skip_if_only_changed` parameters:"
		comment += testContexts
	}

	// If nothing was scheduled, post an informative comment instead of staying
	// silent. In modeDelta the empty result can mean the second stage was already
	// triggered earlier for this HEAD; in that case avoid the misleading "no
	// second-stage tests were triggered" wording.
	if comment == "" {
		if mode == modeDelta && secondStageTriggeredAtSHA(context.Background(), pjLister, pj, presubmits) {
			comment = fmt.Sprintf("**Pipeline controller notification**\n\nAll applicable second-stage tests for this HEAD have already been triggered. Nothing new to schedule.\n\nUse `/test ?` to see all available tests, or `/pipeline required` to re-run the full required set for the `%s` branch.", pj.Spec.Refs.BaseRef)
		} else {
			comment = fmt.Sprintf("**Pipeline controller notification**\n\nNo second-stage tests were triggered for this PR.\n\nThis can happen when:\n- The changed files don't match any `pipeline_run_if_changed` patterns\n- The changed files don't feed into any configured `pipeline_run_if_dockerfile_changed` Dockerfiles\n- All files match `pipeline_skip_if_only_changed` patterns\n- No pipeline-controlled jobs are defined for the `%s` branch\n\nUse `/test ?` to see all available tests.", pj.Spec.Refs.BaseRef)
		}
	}

	if err := ghc.CreateComment(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number, comment); err != nil {
		deleteIds()
		return err
	}
	return nil
}

// secondStageTriggeredAtSHA reports whether any second-stage (protected or
// conditional) job already has a ProwJob at the ProwJob's HEAD SHA. It reads the
// controller cache only (no GitHub calls) and is used solely to word the
// empty-delta comment. It returns false when there is no lister or HEAD SHA.
func secondStageTriggeredAtSHA(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, presubmits presubmitTests) bool {
	if pjLister == nil || pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 || pj.Spec.Refs.Pulls[0].SHA == "" {
		return false
	}

	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false
	}

	existing := map[string]bool{}
	for _, pjob := range pjs.Items {
		if pjob.Spec.Refs != nil && len(pjob.Spec.Refs.Pulls) > 0 &&
			pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			existing[pjob.Spec.Job] = true
		}
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef
	secondStage := append([]config.Presubmit{}, presubmits.protected...)
	secondStage = append(secondStage, presubmits.pipelineConditionallyRequired...)
	secondStage = append(secondStage, presubmits.pipelineSkipOnlyRequired...)
	for _, presubmit := range secondStage {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if existing[presubmit.Name] {
			return true
		}
	}
	return false
}

func acquireConditionalContexts(ctx context.Context, pj *v1.ProwJob, pipelineConditionallyRequired []config.Presubmit, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader, mode scheduleMode) (string, error) {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		return "", fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef
	var testCommands string
	if len(pipelineConditionallyRequired) != 0 {
		cfp := newDeferredChangedFilesProvider(ghc, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)
		fileClient := newFileCachingGhClient(ghc)

		// First, determine which tests should run based on file changes
		var testsToRun []config.Presubmit
		for _, presubmit := range pipelineConditionallyRequired {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}

			shouldRun := false
			// Check pipeline_run_if_changed first (takes precedence)
			if run, ok := presubmit.Annotations["pipeline_run_if_changed"]; ok && run != "" {
				psList := []config.Presubmit{presubmit}
				psList[0].RegexpChangeMatcher = config.RegexpChangeMatcher{RunIfChanged: run}
				if err := config.SetPresubmitRegexes(psList); err != nil {
					deleteIds()
					return "", err
				}
				_, shouldRunResult, err := psList[0].RegexpChangeMatcher.ShouldRun(cfp)
				if err != nil {
					deleteIds()
					return "", err
				}
				shouldRun = shouldRunResult
			} else if skip, ok := presubmit.Annotations["pipeline_skip_if_only_changed"]; ok && skip != "" {
				// Check pipeline_skip_if_only_changed if pipeline_run_if_changed is not present
				psList := []config.Presubmit{presubmit}
				psList[0].RegexpChangeMatcher = config.RegexpChangeMatcher{SkipIfOnlyChanged: skip}
				if err := config.SetPresubmitRegexes(psList); err != nil {
					deleteIds()
					return "", err
				}
				_, shouldRunResult, err := psList[0].RegexpChangeMatcher.ShouldRun(cfp)
				if err != nil {
					deleteIds()
					return "", err
				}
				shouldRun = shouldRunResult
			} else if dockerfileAnnotation, ok := presubmit.Annotations["pipeline_run_if_dockerfile_changed"]; ok && dockerfileAnnotation != "" {
				changedFiles, err := cfp()
				if err != nil {
					deleteIds()
					return "", err
				}
				shouldRunResult, err := evaluateDockerfileAnnotation(dockerfileAnnotation, changedFiles, pj, fileClient)
				if err != nil {
					logrus.WithError(err).WithField("test", presubmit.Name).Error("failed to evaluate Dockerfile condition, conservatively scheduling test")
				}
				shouldRun = shouldRunResult
			} else {
				shouldRun = true
			}

			if shouldRun {
				testsToRun = append(testsToRun, presubmit)
			}
		}

		// In modeDelta, complement the jobs already triggered at HEAD by scheduling
		// only the ones still missing. List the PR's ProwJobs from the controller
		// cache once (not per job) and subtract the ones already present at the same
		// SHA. modeForce (/pipeline required) skips this entirely and schedules all.
		//
		// Preserve today's guards: skip the cache lookup when there is no lister or
		// no HEAD SHA (several tests pass a nil lister).
		existing := map[string]bool{}
		if mode == modeDelta && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
			// Same label selector today's latch block built (from ProwJob spec).
			selector := map[string]string{
				kube.OrgLabel:         pj.Spec.Refs.Org,
				kube.RepoLabel:        pj.Spec.Refs.Repo,
				kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
				kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
				kube.ProwJobTypeLabel: string(v1.PresubmitJob),
			}

			var pjs v1.ProwJobList
			if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
				// Fail closed: a list error must NOT be treated as "all jobs absent"
				// (that would mass-trigger). Return the error so sendCommentWithMode
				// short-circuits and schedules nothing; the automatic path retries on
				// the next ProwJob event.
				return "", fmt.Errorf("listing prowjobs for delta: %w", err)
			}
			for _, pjob := range pjs.Items {
				if pjob.Spec.Refs != nil && len(pjob.Spec.Refs.Pulls) > 0 &&
					pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
					existing[pjob.Spec.Job] = true
				}
			}
		}

		for _, presubmit := range testsToRun {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}
			if mode == modeForce || !existing[presubmit.Name] {
				testCommands += "\n" + presubmit.RerunCommand
			}
			// else: already present at HEAD (manual trigger or a prior delta) → skip it
		}
	}
	return testCommands, nil
}

// existsAtSHA checks whether a ProwJob with the given job name already exists
// for the same org/repo/PR/baseRef at the same HEAD SHA. This is used to avoid
// re-triggering tests that were already triggered (e.g. via /pipeline required)
// when an event like /lgtm fires at the same commit.
func existsAtSHA(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, jobName string) bool {
	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false
	}

	for _, pjob := range pjs.Items {
		if pjob.Spec.Job == jobName &&
			pjob.Spec.Refs != nil &&
			len(pjob.Spec.Refs.Pulls) > 0 &&
			pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			return true
		}
	}
	return false
}

// checkFirstStageComplete checks if all first-stage tests have completed
// successfully for the given ProwJob's SHA. This is used by the /pipeline auto
// handler to trigger second-stage tests immediately when first-stage is already
// done, since the event-driven reconciler won't fire if all ProwJob updates
// occurred before the pipeline-auto label was added.
func checkFirstStageComplete(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, presubmits presubmitTests) (bool, error) {
	if pj == nil || pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) != 1 {
		return false, nil
	}

	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false, fmt.Errorf("cannot list prowjobs: %w", err)
	}

	latestBatch := make(map[string]v1.ProwJob)
	for _, pjob := range pjs.Items {
		if pjob.Spec.Refs == nil || len(pjob.Spec.Refs.Pulls) == 0 {
			continue
		}
		if pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			if existing, ok := latestBatch[pjob.Spec.Job]; !ok {
				latestBatch[pjob.Spec.Job] = pjob
			} else if pjob.CreationTimestamp.After(existing.CreationTimestamp.Time) {
				latestBatch[pjob.Spec.Job] = pjob
			}
		}
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef

	// Second-stage (protected) jobs must not already be running
	for _, presubmit := range presubmits.protected {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if _, ok := latestBatch[presubmit.Name]; ok {
			return false, nil
		}
	}

	// All always-required first-stage jobs must have succeeded
	for _, presubmit := range presubmits.alwaysRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pjob, ok := latestBatch[presubmit.Name]; !ok || pjob.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	// Conditionally-required first-stage jobs, if present, must have succeeded
	for _, presubmit := range presubmits.conditionallyRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pjob, ok := latestBatch[presubmit.Name]; ok && pjob.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	return true, nil
}
