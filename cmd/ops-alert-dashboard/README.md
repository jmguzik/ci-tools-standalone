# Ops alert dashboard

A standalone, Slack-backed operations dashboard. It reads the ops channel, serves
an in-memory queue, and posts accepted workflow actions as DPTP-bot thread replies.
Slack holds the durable record. There is no database, PVC, alert-proxy dependency,
or dependency on `openshift/ci-tools`.

## Try it locally

```sh
make build-ops-alert-dashboard
_output/ops-alert-dashboard --demo --listen=127.0.0.1:8080
```

Open `http://127.0.0.1:8080`. Demo mode uses synthetic Slack messages and the real
classification, replay and action code. It seeds all five views and simulates a
Chai reply. It never connects to Slack. Demo state resets when the process stops.

## Live configuration

Copy `examples/config.yaml` and replace its Slack identity and access placeholders.
Mount the classification file from
`openshift/release/core-services/slack-bot/_ops-alerts.yaml`. The copy in
`examples/classification.yaml` is a demo fixture, not another production catalog.

```sh
ops-alert-dashboard \
  --config=/etc/ops-alerts/config.yaml \
  --classification=/etc/ops-alerts/_ops-alerts.yaml \
  --slack-token-file=/etc/slack/token \
  --history-window=48h \
  --listen=127.0.0.1:8080
```

Validate files without credentials or Slack calls:

```sh
ops-alert-dashboard --validate-only \
  --config=/etc/ops-alerts/config.yaml \
  --classification=/etc/ops-alerts/_ops-alerts.yaml
```

Use the DPTP-bot token for writes. Startup checks its workspace, user, bot and
workspace URL against `auth.test`. Configure trusted producers by Slack-issued
user, bot or app IDs, never display names. The Chai source must include its bot
user ID so the dashboard can mention it explicitly. Tokens are read from files
and never sent to the browser. If needed, `--slack-history-token-file` selects a
separate read token in the same workspace.

SHIP notifications are published by DPTP-bot. Configure that producer as
`kind: ship` with the same user ID as `slack.writer.user_id`, as in the example.
Only SHIP may share the writer identity. Dashboard action replies do not become
alerts, and other DPTP-bot parent notifications default to Orange / Other.

The app must be a member of the channel and its token must be authorized for
channel history and replies, `chat:write`, and `reactions:write`.
See Slack's [history](https://docs.slack.dev/reference/methods/conversations.history/)
and [replies](https://docs.slack.dev/reference/methods/conversations.replies/)
documentation for scopes and workspace limits. Read requests fetch every page,
honor `Retry-After`, and stop on cancellation. Writes are never blindly retried.

Run **one replica** behind an authenticated OpenShift SSO reverse proxy. Keep the
backend private; its default listen address accepts only loopback connections.
The proxy must remove incoming identity headers and inject the verified identity
in `access.user_header` (default `X-Forwarded-Email`). Configure the exact public
HTTPS origin, trusted proxy CIDRs and allowed operator identities. The backend
checks them for both reads and writes, derives the actor server-side, and requires
a user-bound CSRF token and matching Origin on mutations. There is no live
unauthenticated mode. Changing the listen address does not change proxy trust.

By default, live mode requires a nonempty `access.operators` allowlist. To use a
proxy that also authorizes group access, set `access.authorization: proxy`, omit
`access.operators`, and explicitly configure `access.trusted_proxies`. The proxy
must enforce authorization before forwarding any dashboard request, as well as
replacing identity headers. For core-ci, configure its OAuth proxy to authorize
`get` on the `ci/ops-alert-dashboard` Service through SubjectAccessReview, with a
RoleBinding granting the existing `test-platform-ci-admins` group that access.
This mode accepts any well-formed authenticated identity forwarded by that trusted
proxy, so keep direct backend access private. It still requires the public HTTPS
origin and exactly one nonempty identity header; actions retain origin and
user-bound CSRF checks and record the current SSO identity for audits. Unknown
authorization values, combining proxy authorization with an operator allowlist,
and using proxy authorization in demo mode are rejected.

## Classification and views

- Managed Alertmanager and Prow messages publish
  `[ops_color=red ops_category=ci_platform]`. Valid pairs from trusted producers
  are used before narrow platform and legacy fallback rules. Unknown or invalid
  classifications are Orange / Other.
- Recognized Chai scheduled reports are always Green / Chai reports. They have
  their own latest-per-task overview, report details and history, and are excluded
  from alert counts. The scheduler's `scheduled_task` Slack metadata identifies
  each group, falling back to the task ID before `| Instructions:` in the visible
  header. Instruction paths are source context, not separate task names. Clearly
  labeled coverage and limits remain visible alongside the findings. Presentation keeps
  the original wording and full source, without inferring health from prose or
  treating Green as a successful scan. Unknown report formats remain readable.
  Ordinary Chai conversations do not create alerts. Demo examples use the six
  recurring task envelopes with entirely synthetic findings. Healthy scheduled
  scans may deliberately post nothing, so absence of a report does not establish
  scan success or failure.
- SHIP root notifications beginning `🚨 Outage Detected: component/subcomponent`
  are Yellow. Build Farm maps to Cluster fleet and Downstream CI / CI-Config to
  Repo automation. Description prose does not determine classification.
- A grouped notification remains one occurrence, retaining its parsed alert
  identifiers. It uses the highest alert color and Other if categories differ.

Every new parent message is a new occurrence, identified by channel and Slack
timestamp within the configured workspace. Repeated delivery and edits update
that occurrence. Replies add evidence. A resolved notification does not close an
operator investigation automatically.

Alerts with a published SOP link show an expandable SOP panel beside their
workflow actions. Opening it reads the linked Markdown from the public
`openshift/release` repository, with headings, lists, tables and command blocks.
A link to a heading opens that section first; the original GitHub document stays
available. Commands are displayed as text. The preview does not perform workflow
actions or run commands.

Only SOP Markdown under `docs/dptp-triage-sop/` on `main` or `master` is previewed.
The authenticated endpoint accepts a link already present on that occurrence,
uses a bounded request to the fixed raw GitHub host and rejects redirects.
Unreachable or missing documents leave the original source link available.
Browser memory retains opened documents for the page visit. Demo alerts are
synthetic, while their SOP previews read the real public documentation on request.

## Core-ci metrics

The optional `monitoring` configuration targets deployment on core-ci. Its query
URL is the internal Thanos Query frontend already used by core-ci Grafana:

```yaml
monitoring:
  enabled: true
  rules_namespace: ci-monitoring
  query_url: http://thanos-query-frontend-ci-query.ci-monitoring.svc.cluster.local:9090
```

Use the pod's in-cluster ServiceAccount credentials and CA to read PrometheusRule
definitions in `ci-monitoring`. `examples/monitoring-rbac.yaml` grants only
namespace-scoped `get` and `list` on those rules; adapt its ServiceAccount namespace
to the Deployment. Kubernetes credentials stay on the Kubernetes API client.
The internal Thanos query request uses a separate client, matching Grafana's
existing unauthenticated service connection. Allow network access to that service
and the Kubernetes API. No permissions or cluster resources are applied by the
dashboard itself.

For a recognized Alertmanager occurrence, expand **Core-ci metrics** to inspect
that source explicitly. The server matches an exact alert name against the
configured namespace's rule definitions, then queries its expression unchanged.
It rejects ambiguous rules and does not accept browser-supplied PromQL or URLs.
Build-cluster alerts must not be assumed to originate on core-ci merely because
they share a name or Slack channel.

The graph uses a bounded two-hour window and shows the exact expression, rule
resource, `for` duration, source and time range. Historical occurrences use a
window around the notification rather than silently substituting today's data.
Alert expressions can return no series when a condition does not match. Missing
samples remain gaps; empty results do not establish health or live firing state.
Kubernetes rule definitions do not provide the evaluator's current firing state.

Reads happen on demand, with explicit refresh/retry and bounded response sizes.
Monitoring failures leave the alert feed, SOPs and workflow actions usable.
Disable `monitoring.enabled` to run without cluster monitoring access. Demo mode
uses synthetic rule/graph data and never reads cluster credentials or metrics.

## Actions and persistence

Acknowledge and Close investigation affect only the selected occurrence. Undo
returns it to its previous workflow view. A later parent alert starts fresh.

Snooze creates a Go regexp rule scoped to this channel and producer. Its default
is the escaped, anchored parsed identifier; the preview lets the operator edit it.
The server sets expiry to the next **06:00 UTC**. Subsequent matching firings stay
snoozed without extending the deadline. Expiry or Unsnooze restores each alert's
prior Feed/Acknowledged view. Closed investigations remain closed. Unsnooze
cancels the displayed rule for every occurrence it covers. With overlapping
rules, another active rule may still apply. Snooze affects the dashboard workflow;
Slack reporting and paging continue.

Each accepted mutation saves a readable thread reply with a versioned
`OPS_ALERT_ACTION_V1` record, request ID, actor/time, target and complete workflow
state. Only replies from the verified writer identity are replayed as actions.
An action is successful only after Slack confirms the save. The visible reactions
are decoration; removing one does not undo an action. Use the dashboard's explicit
Undo/Unsnooze controls. Expired snooze decoration for visible occurrences is
reconciled on the next scan. Untouched feed messages without status reactions
require no reaction writes. Startup reconciliation uses the status reactions
already returned with channel history; subsequent scans retain successful icon
changes, including partial transitions. Removal affects only the token owner's
reaction, preserving other users' reactions.

A reaction rate limit stops the optional reaction batch until Slack's
`Retry-After` deadline without delaying history reads or durable action writes.
If Slack rejects a reaction with `missing_scope`, optional reactions are disabled
until the process restarts; the dashboard feed and workflow actions continue.
After granting `reactions:write` and reinstalling the Slack app if needed, restart
the dashboard to resume decoration.

The browser submits the selected revision and a stable request ID. Stale changes
are rejected; confirmed duplicate requests are reconciled without a second post.
Unconfirmed requests are retained in per-user browser session storage across
reloads, without storing credentials or CSRF tokens. Retry is always explicit.
On uncertain delivery, check the thread and retry the **same** retained request.
Do not create a new request to work around uncertainty. Unconfirmed retry guards
are in memory; after a restart, recovery can prove only records retained by Slack.
If its occurrence leaves the history window, the browser offers an explicit
Dismiss expired local retry control. It clears only this browser's retry guard;
delivery remains unconfirmed and the operator must check the original thread.

Ask Chai previews an editable question, then DPTP-bot posts an explicit mention,
request ID, source context and read-only investigation instructions in that alert's
thread. It does not acknowledge or close the alert. Only one request may be
outstanding per occurrence. A later Chai-authored reply is shown as Reply received;
delivery does not prove that an investigation executed. This shared-thread flow
does not use Chai's private-DM shortcut.

## Recovery and operation

The dashboard shows a rolling **48-hour** window by default. Change it with
`--history-window`, for example `--history-window=72h`. The lower boundary moves
with the clock, including across restarts; older occurrences and Chai reports
leave all dashboard views while their original messages remain in Slack.
`slack.cutover` is optional and can set an earliest go-live date within that window.

Recovery reads up to 24 additional hours of parent messages to preserve active
snoozes whose original occurrence has left the visible window. With the default,
the scan covers at most 72 hours. Buffer occurrences are hidden and cannot receive
new actions. Current-day snoozes still expire at the next 06:00 UTC reset.
Reconciliation rescans this bounded history (default every minute), rereading
threads when reply counts or latest-reply timestamps change. Browser refreshes
read memory every 15 seconds, not Slack. Config changes require a process restart.

Incomplete replay or malformed trusted action records expose Catching up/Degraded
coverage and pause actions. Verify channel retention and full rebuild time before
rollout. Deleted or expired Slack records cannot be recovered by this service.
The channel is the persistence boundary, so retention must cover the history
window and replay buffer (72 hours with the default settings).

`/healthz` checks process liveness; `/readyz` succeeds only with complete archive
coverage. Other routes require authenticated operator access. No production
deployment or live Slack smoke test is performed by the demo.

## Development

```sh
go test ./internal/opsalerts ./cmd/ops-alert-dashboard
go vet ./internal/opsalerts ./cmd/ops-alert-dashboard
make test
make lint
```

The focused table-driven tests cover producer/classification trust, occurrence
identity, scoped daily-reset snooze and replay, stale/idempotent/uncertain writes,
Chai thread binding, HTTP authorization/CSRF, and Slack pagination. They use fixed
clocks and in-memory Slack transports. There is no integration or browser test
framework.
