# Agentic job selection

Agentic selection changes **which** second-stage jobs run, not the existing
`manual`, `auto`, or LGTM trigger mode. Omit the `agentic` block to retain normal
behavior. Chai and the controller communicate through PR comments only.

## Configuration

Use this repository entry in the existing main or LGTM configuration file:

```yaml
- name: example
  branches: [main]
  mode:
    trigger: auto
    agentic:
      mode: chai
```

Set timeout and trusted authors globally through controller arguments:

```text
--agentic-timeout=20m
--agentic-trusted-author=your-chai-bot-login
```

Timeout defaults to 20 minutes. Repeat the author flag for multiple exact GitHub
logins; there is no default identity. Enrollment without it fails closed.
Repository-level timeout/trust fields are rejected. The LGTM configuration
retains its existing LGTM trigger behavior.

Dispatch requires first-stage success and the configured trigger (or an
authorized manual command). Unlike normal manual commands, agentic manual
commands also wait for first-stage success. Chai can send its list earlier.

The timeout starts only when the other dispatch prerequisites hold. If no valid
list arrives, the controller records normal-selection fallback for that HEAD.
Late Chai replies cannot replace it. Fallback does not skip tests, bypass the
trigger, or permanently opt the PR out of Chai.

## Chai comment contract

The controller reads the visible Markdown directly; there is no hidden JSON:

```markdown
Chai test plan for `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` → `main`

- `pull-ci-example-main-e2e`
```

The full SHA and target branch identify what Chai analyzed.
[GitHub comment events](https://docs.github.com/en/webhooks/webhook-events-and-payloads#issue_comment)
do not include a HEAD SHA; fetching the current PR cannot identify a delayed
reply's original revision. The heading prevents stale push/retarget decisions.

Job names must identify reporting, branch-applicable static
second-stage presubmits: pipeline-annotated jobs or protected non-optional
manual jobs. The controller validates names but does not redo Chai's selection.

Replace the bullets with `None.` for an explicit empty selection; a missing list
is invalid. An optional final `Reason: ...` line may explain the choice.
Post a new comment for a new decision. Do not add another payload or job list.

Chai must additionally handle controller-authored re-review requests:

```markdown
Chai test selection requested for `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` → `main`.

Request: `bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`
```

Echo the `Request:` line in the reply, before any reason. Chai must verify the
controller identity and current PR refs. Normal planning starts on PR creation
or new commits and omits `Request:`; dispatch timing remains unchanged.
After a same-SHA base retarget, use `/pipeline agent-review` for a fresh plan.

This is a new integration contract: controller support does not install or
modify Chai. Configure Chai to produce and consume these comments before
enrolling repositories.

## Commands and safety

| Command | Agentic behavior |
| --- | --- |
| `/pipeline required` | Force the selected set, including reruns |
| `/pipeline remaining` | Dispatch only selected jobs missing at HEAD |
| `/pipeline auto` | Existing LGTM-mode timing override |
| `/pipeline agent-review` | Request a new list; clear opt-out before dispatch |
| `/pipeline skip-agent-review` | Persist normal-selection fallback as a PR label |

Commands require an authorized repository collaborator or organization member.
Early manual requests are recorded for the current HEAD/base. Once dispatch
starts, the plan cannot shrink: published job contexts are not deletable.
New HEADs or base branches require a fresh decision.

The controller alone writes `ci/tests-dispatched`. It stays `in_progress` while
waiting or dispatching and succeeds only after each selected execution reports
its job context. This means **dispatched**, not **tests passed**; existing job
contexts still gate their results. No unselected placeholders or fabricated
passing test results are posted.
If a rerun replaces a selected execution's latest report, recovery checks status
history for that SHA/context/execution URL. First-stage checks still require
current success; the recovered report is saved in the existing journal.

Gate state records the plan, request, deadline and dispatch identity. One short
controller tracking comment records the PR's current revision and is edited
across pushes. GitHub/ProwJob events drive reconciliation; there is no periodic
GitHub polling. One startup pass restores state and pending Chai deadline timers.
Actual enrollment-config changes also trigger recovery. Recognized transient
failures retry their affected PR/repository with backoff from 5 seconds up to
5 minutes, continuing until recovery. Available server rate-limit delays take
precedence for that work item, including over its Chai deadline. Validation,
permission and unclassified errors wait for another event; invalid plans retain
their one-shot Chai deadline. Idle PRs have no recurring timer.
Missed webhooks need redelivery, another relevant event, or restart recovery.
Repost commands older than the recorded revision boundary. Stable job names make
retries idempotent.

If the PR moves A → B → A without the controller observing B, the old
decision/gate may be reused; retries and startup recovery cannot reconstruct
that missed revision history.

## Rollout requirements

- Controller GitHub App: Checks read/write, PR/comment and label access,
  commit-status reads, and organization Members read for command authorization.
  Chai needs PR-comment write access, not gate ownership.
- Controller Kubernetes Role: `get`, `list`, `watch`, and `create` for ProwJobs.
  The existing deployment Role lacks `create`; deployment configuration is in
  `openshift/release`, outside this change.
- Forward `status` events alongside `pull_request` and `issue_comment`.
  Existing external-plugin configuration forwards only the latter two.
- Run one active controller replica. The existing deployment uses `Recreate`.
- Use distinct HEAD commits for enrolled PRs; a shared SHA shares GitHub checks
  and is blocked by this version.
- Require `ci/tests-dispatched` from the controller App in a GitHub ruleset
  and explicitly in Tide, scoped to the same enabled branches:

```yaml
tide:
  context_options:
    orgs:
      example-org:
        repos:
          example:
            branches:
              main:
                required-contexts: [ci/tests-dispatched]
              release: # not enrolled; ignore a gate retained after retargeting
                optional-contexts: [ci/tests-dispatched]
```

Do not make the gate repository-wide when only selected branches are enrolled.
Keep any optional-gate exception confined to non-enrolled branches.
Keep independently required contexts mandatory. Verify Tide's batch selection
covers every selectable required job; a larger batch test set is acceptable.
Existing job placeholders on an already-open PR cannot be deleted: finish those
jobs or use a new HEAD when enrolling it. Rerun first-stage jobs if they were
garbage-collected before the controller could record their successful reports.
