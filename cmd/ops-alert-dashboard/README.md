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
classification, replay and action code. It seeds the workflow views, a bot PR list and a
Chai reply. It never connects to Slack or the GitHub PR API. Demo state resets when the process stops.

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
  Latest and historical reports render a safe Markdown subset: headings,
  paragraphs, ordered and unordered lists (up to eight nesting levels), indented
  list continuations, blockquotes, fenced code, inline code, simple pipe tables,
  HTTPS Markdown links and Slack links/emoji. Double stars render bold; single
  stars retain Slack bold semantics, underscores render emphasis, and tildes
  render strikethrough. Fence language markers are omitted from code content.
  HTML and remote images are never rendered. Original source remains available
  unchanged; readable coverage excerpts use the same display time conversion.
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

## Browser preferences and presentation

Times use the browser's local timezone, including daylight saving. Readable Slack
and Chai report prose also converts explicit UTC ISO timestamps (`Z`, `+00:00`
or `+0000`) and `YYYY-MM-DD HH:mm[:ss] UTC` values, showing the local UTC offset.
Clock-only `H:mm[:ss] UTC` values use that occurrence or report's UTC calendar
date, including each historical report and Slack reply's own timestamp. Without
a source date, clock-only values remain literal. Naive times, code and link
URLs are preserved; Original Slack disclosures retain the exact source text.
The channel name defaults to `ops-testplatform`; set `--slack-channel-name` when
configuring a different channel. The UI displays channel names in cards, dialogs and rules.
Titles prefer parsed identifiers and preserve a leading source 🚨, with source
firing/resolved status shown separately. Prow run links remain directly accessible. Message evidence supports
safe Slack formatting and common Unicode emoji aliases; unsupported custom emoji
remain readable. Raw message HTML is never executed.

Unread IDs and notification preferences are scoped to the authenticated user in
this browser's local storage. They survive page reloads and SSO renewal but do not
synchronize across devices. Existing occurrences form the first-visit baseline;
explicitly opening a card or Mark all read clears personal unread state. Severity
colors, shared WIP state and personal unread indicators are independent.
Feed navigation always shows its shared occurrence total; a separate red badge
appears only for a nonzero personal unread count.
New Feed occurrences since the last complete sync remain unread when returning
after a reload or sign-in. Incomplete coverage preserves the saved read state.

Desktop notifications are opt-in while the page is open. Enable notifications
requests browser permission through a button; the browser retains that permission
separately from the saved dashboard preference. New Feed arrivals are grouped into
bursts, deduplicated across supported browser tabs, and do not include Chai reports
or suppressed occurrences. Suspended or closed tabs are not guaranteed to notify;
there is no push subscription service. Authentication or stale coverage pauses
notification delivery. Personal storage failures do not weaken action retry guards.

An expired proxy session pauses background requests and provides Sign in again.
Old data is marked stale and action delivery remains unconfirmed until reconciled.
No pending mutation is replayed automatically after sign-in. The external proxy
controls session lifetime; the core-ci deployment configures twelve hours.

## Unmerged bot PRs

The separate **Unmerged PRs** view lists open PRs in `openshift/release` from exact
configured bot logins, including drafts and older PRs outside the Slack history
window. Defaults are `openshift-bot` and `openshift-ci-robot`; Chai bot PRs are
excluded from the default watch list. The Chai reports view is independent.
Override with `--unmerged-pr-authors=login1,login2`, or use an empty value to disable
the view. It is read-only and never merges PRs or posts GitHub comments.

One server cache refreshes every five minutes, independent of browser requests.
A complete paginated search supplies title, link, author, created/updated time,
draft and labels. Incomplete or over-1,000-result searches fail explicitly; narrow
the configured authors rather than treating a partial list as complete. Failures
retain the last complete list with a stale indicator and last-sync timestamp.
No per-PR CI or review queries are issued. GitHub rate limits and retry deadlines
are respected independently of Slack. Implausible header-based delays are capped
at six hours so a malformed response cannot suspend polling indefinitely.

Public GitHub reads work anonymously. `--github-token-file` optionally supplies a
server-only token with the access needed to read this public repository when a
higher API quota is needed. It is not the Slack token and is never returned in
snapshots or logs. Demo mode supplies synthetic PR data and makes no GitHub calls.

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

The UI calls acknowledgement **WIP** and shows the operator. Reading an alert is
personal and does not acknowledge it. Each WIP, Snooze and Close dialog offers
**This occurrence** or **Matching identifiers**. Regex previews use the server's
Go regexp engine, show up to 200 current matching occurrences, and restrict scope
to the selected channel and producer. Patterns match parsed source identifiers,
not arbitrary message text. Regex requires a parsed identifier; individual actions
do not. A stale revision is rejected before saving.

Individual WIP stays assigned until released or closed. Regex WIP covers current
and future matches until the next **06:00 UTC**; unfinished rule-owned occurrences
then return to Feed. Explicit individual actions override older family assignment.
The UI can release an individual occurrence or cancel its whole WIP rule.

Individual Snooze covers only that occurrence. Regex Snooze covers current and
future identifier matches in the same scope. Both expire at the next **06:00 UTC**,
shown in browser-local time. Repeated firings do not extend the deadline. Unsnooze
cancels the displayed rule; another overlapping rule may still apply. Expiry
restores prior workflow, and closed investigations remain closed. Slack reporting
and paging continue.

Close affects individual occurrences permanently until reopened. Regex Close
freezes the current preview's IDs and revisions and sends individual close actions
sequentially. It never creates a rule for future firings. Partial progress remains
visible and the browser retains the original plan and request IDs. Conflict,
rejection or uncertain delivery stops the batch; resuming or discarding the
remaining plan is explicit and never undoes already confirmed actions.

Each accepted mutation saves a short readable thread reply, with the complete
versioned action record in Slack message metadata (`ops_alert_action_v1`, payload
field `record`). Request ID, actor/time, target, rule scope and deadline, and
complete workflow state remain durable without printing JSON in the conversation.
Existing `OPS_ALERT_ACTION_V1` text records are still read. Only replies from the
verified writer identity are replayed as actions; malformed metadata, conflicting
copies, or a dashboard receipt missing its record pause actions rather than
guessing state. An action is successful only after a targeted readback returns the
exact saved metadata. Failed or rate-limited confirmation leaves delivery uncertain,
without repeating the write or sleeping in a confirmation retry loop. The visible reactions
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

Before deploying metadata writes, verify posting and reading metadata with the
actual writer and history tokens in an operator-approved test thread. Metadata can
be ignored with a Slack schema warning even when posting succeeds, so a timestamp
alone is insufficient. Preserve the existing history/replies permissions and
`include_all_metadata` reads. A rejected metadata payload does not fall back to a
visible JSON write. New writes enforce a conservative 3,000-byte application budget
on the serialized metadata envelope, including string escaping. This is not a claim
about Slack's undocumented server limit. Oversized durable records, including a large
retained investigation question, are rejected before posting and leave workflow state
unchanged. Shorten oversized questions or records; there is no visible-JSON fallback.
Chai response text is reconstructed from trusted thread replies for display and is
not copied into action metadata, so long responses do not block workflow actions.
Slack metadata is workspace-readable, not a place for secrets.
Once metadata records exist, rollback requires a metadata-compatible image; older
binaries can ignore those records and must not be used while they are retained.

The browser submits the selected revision and a stable request ID. Stale changes
are rejected; confirmed duplicate requests are reconciled without a second post.
Unconfirmed requests are retained in per-user browser session storage across
reloads, without storing credentials or CSRF tokens. Retry is always explicit.
On uncertain delivery, check the thread and retry the **same** retained request.
Do not create a new request to work around uncertainty. Unconfirmed retry guards
are in memory. An unconfirmed WIP family-rule cancellation pauses new actions
across occurrences until that cancellation is reconciled, because later actions
must not rely on a rule that might already be canceled. After a restart, recovery
can prove only records retained by Slack.
If its occurrence leaves the history window, the browser offers an explicit
Dismiss expired local retry control. It clears only this browser's retry guard;
delivery remains unconfirmed and the operator must check the original thread.

Ask Chai previews only the bot routing and selected destination thread. The request
uses the selected alert as its question; DPTP-bot then posts an explicit mention,
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
daily Snooze and WIP rules whose original occurrence has left the visible window. With the default,
the scan covers at most 72 hours. Buffer occurrences are hidden and cannot receive
new actions. Current-day rules still expire at the next 06:00 UTC reset.
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
identity, scoped daily-reset WIP/Snooze and replay, stale/idempotent/uncertain
writes, Chai thread binding, HTTP authorization/CSRF, Slack pagination, and complete
GitHub search caching. Browser logic unit cases run with Node when available and
cover personal unread state, notification deduplication, safe formatting, and
explicit recovery of partial close batches. Tests use fixed clocks and in-memory
transports. There is no integration or browser test framework.

Scoped workflow fields extend durable v1 records. New versions still read old
records, but older binaries reject scoped fields during strict replay. Keep a
compatible image for rollback while scoped records remain in retained history;
do not delete shared action records to force an older writer to start.
