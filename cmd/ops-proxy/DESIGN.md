# ops-proxy design

**Status:** v1 binary exists in this repo (`cmd/ops-proxy`, `internal/opsproxy`). Deploy YAML, Alertmanager `webhook_configs`, and the slack-bot button forwarder are still other repos.

This document is the build spec for `cmd/ops-proxy` in [openshift/ci-tools-standalone](https://github.com/openshift/ci-tools-standalone). Deploy YAML, Alertmanager `webhook_configs`, stay-firing PromQL, and P0/P1/P2 labels live in [openshift/release](https://github.com/openshift/release).

## Problem

`#ops-testplatform` is a firehose of independent FIRING lines, not a workbench. Two Slack dumps (about 10–14 Aug and 18–22 Aug 2026) had **zero human replies**. The **mix** of alerts changed between weeks; the **mechanics** did not.

What operators actually need:

1. One Slack object per incident while it is still red (a **banner**, not a 2h nag that scrolls away).
2. **Ack for a while** without resolving: a periodic can fail every 2h; mute Slack/PD for a day; keep it on the board.
3. That mute must work **without Alertmanager UI**. OpenShift Observe → Alerting can create silences but is two clicks away, easy to aim at the wrong AM (platform vs user-workload), and often “does not work.”
4. PagerDuty ack does **not** stop Slack. Slack mute does **not** stop PD. Today those notifiers are independent.

Alertmanager already has the right primitive: a **silence** with matchers and `endsAt`. The gap is a Slack-native writer aimed at the correct AM.

## Goals

- Receive Alertmanager webhooks for ops-bound alerts (UWM first).
- Collapse to one incident identity → one Slack card / board row until success or explicit resolve.
- Let an operator **Ack until T** from Slack. Proxy writes `POST /api/v2/silences` on user-workload AM. Alert stays firing; notifications stop until T.
- Keep a standing board in `#ops-testplatform` (topic and/or pinned message updated in place).
- Survive proxy crash by reconciling from AM (firing + silences) plus a small ConfigMap for Slack `ts`.
- Leave PagerDuty on Alertmanager for P0/P1 so a dead proxy does not mute pages.
- Reuse existing DPTP Bot Slack token. No new Slack app in v1.

## Non-goals

- Replacing Prometheus or Alertmanager evaluation/grouping.
- A Postgres / ITSM / incident.io product, on-call schedules, war rooms, status pages.
- A **second mute database**. Ack until T exists only as an Alertmanager silence. The ConfigMap may cache silence id / `endsAt` for display; it must never be the authority for “should we notify?”
- Forever silence, free-form matcher DSL in Slack, silence-all-criticals.
- Chat-ops `kubectl` or guessed auto-remediation.
- Auto-Jira per card.
- Karma, Grafana OnCall, or a second paging system.
- Moving this into `openshift/ci-tools` `cmd/slack-bot` (helpdesk + paging share one replica and keel `:latest`).
- Adding a **new image to `openshift/ci-tools`**. That is why this repo exists.
- Farm Alertmanager routing fixes, stay-firing PromQL, `group_by` identity, inhibit dual `creating_release_images` — those are `openshift/release` changes. This binary consumes their result.

## Placement

| Piece | Repo | Why |
|---|---|---|
| Proxy binary + image | **this repo** `cmd/ops-proxy` | New tools get new images here, not in ci-tools |
| Slack Ack **button** forwarder (required for production UX) | `openshift/ci-tools` slack-bot | DPTP Bot has **one** Interactivity URL (`slack.ci.openshift.org`) |
| AM webhook_configs, UWM RBAC, Deployment | `openshift/release` | Cluster config |
| Stay-firing / grouping / severity labels | `openshift/release` jsonnet | Prometheus/AM source of truth |

Duplicate Slack HTTP vs ci-tools is about **50–150 lines** (`pr-reminder` already uses `slack-go` the same way). `pkg/slack` in ci-tools is helpdesk Events/modals, not a posting library. This repo **must not** import `github.com/openshift/ci-tools`.

Do **not** fold the proxy into the existing slack-bot image to avoid a new image. That restores the SPOF.

## Architecture

```text
Prometheus  --evaluates-->  Alertmanager (UWM)
                               |  slack_configs: off for ops (after cutover)
                               |  pagerduty_configs: P0/P1 only (release jsonnet)
                               |  webhook_configs --> ops-proxy
                               v
                         ops-proxy (this process)
                               |
                               +--> Slack chat.update / pin / topic  (dptp-bot oauth_token)
                               +--> POST /api/v2/silences           (ack until T)
                               +--> ConfigMap incident bookkeeping  (ts, silence id)
                               |
                               v
                    #ops-testplatform banner + card

Optional: slack-bot forwards Block Kit action_id --> ops-proxy /ack
          (AM webhooks never hit slack-bot)
```

```mermaid
sequenceDiagram
  participant Prom as Prometheus
  participant AM as Alertmanager UWM
  participant P as ops-proxy
  participant Slack as Slack
  participant PD as PagerDuty

  Prom->>AM: firing (stay-firing until success)
  AM->>P: webhook FIRING
  AM->>PD: P0/P1 only
  P->>Slack: upsert card + banner
  Note over Slack: Operator: Ack 24h
  Slack->>P: ack (button via slack-bot or HTTP)
  P->>AM: POST silence endsAt=+24h
  P->>Slack: card ACKED until T
  Note over Prom,AM: job still fails every 2h; AM matches silence
  AM--xP: notifications suppressed
  AM--xPD: suppressed for that incident
  Note over AM: T expires, still firing
  AM->>P: webhook FIRING
  AM->>PD: P0/P1
  P->>Slack: card OPEN again
```

## Sources of truth

| State | Source of truth | Proxy role |
|---|---|---|
| Still firing? | Prometheus → AM `GET /api/v2/alerts` | Reconcile on boot; webhook is a hint |
| Acked until T? | **AM silence** (`endsAt`) | Create/expire via API; never the only copy |
| Who is paged? | **PagerDuty** (AM receiver, P0/P1) | Do not own PD in v1 |
| Slack message `ts` / pin | Slack + ConfigMap | Bookkeeping so `chat.update` hits the same message |
| Card cosmetics (OPEN / ACKED / NEEDS HUMAN) | **ACKED** iff a matching AM silence exists. NEEDS HUMAN is an explicit ConfigMap flag. | Reconstruct mute from AM on every reconcile; fail-open (re-notify) if CM is lost |
| Crier / brancher / chai / Statuspage | Not in AM | Out of v1 ingest; they bypass ack until they upsert |

Alertmanager persists silences on its PVC. It does **not** persist firing alerts; Prometheus resends them after AM restart. Copy that model: the proxy is not an incident database.

## Operator workflow

### New incident

1. Rule fires (after stay-firing in release: one incident until success, not fail/resolve every 2h run).
2. AM webhooks ops-proxy (`send_resolved: true`).
3. Proxy upserts by identity. Slack: first post or `chat.update`. Banner: `RED N OPEN · …`.
4. P0/P1 also page PD from AM (not from the proxy).

### Mute a 2h periodic for a day

1. Card is OPEN (`job_name=periodic-openshift-release-merge-blockers`).
2. Operator picks **24h** from the locked menu.
3. Proxy `POST /api/v2/silences` with narrow matchers and `endsAt=now+24h`, comment `acked by @user via ops-proxy`.
4. Banner: `ACKED by @user until <rfc3339>`.
5. Job fails at +2h, +4h, … Slack and PD stay quiet. Prometheus still shows firing; AM shows silenced.
6. At T, if still firing: silence gone → OPEN + nags again. If a run succeeded: RESOLVED, row drops.

Unack expires the silence immediately.

### Identity

Use labels that exist on the payload:

- `alertname` + `job_name` (infra jobs)
- `alertname` + `reason` (ci-operator error-rate)
- `alertname` + `job_tail` (priv-image class; storm cap: one card, not FIRING:50)
- Later: brancher `repo+from+to`; Prow job name for crier

Never group only on scrape label `job`. Dual `high-ci-operator-error-rate` + `high-ci-operator-infra-error-rate` for the same `reason` must be one card (prefer inhibit in AM jsonnet; proxy may still coalesce).

## Slack

**v1 credentials:** mount `slack-credentials-dptp-bot` `oauth_token` only (same as `pr-reminder`). No `signing_secret` on the proxy. No new Vault item. Do not use `ci-slack-api-url` (incoming webhook cannot update or pin).

**Bot user:** DPTP Bot in `#ops-testplatform`. Other outbound clients already share this token (brancher, dispatcher, ship-status, pr-reminder). Only slack-bot mounts the signing secret.

**Ack input (v1 production):** Slack Block Kit buttons on the card for the locked duration menu, plus Unack and NEEDS HUMAN. Clicks go to slack-bot. slack-bot forwards signed payloads with our `action_id` prefix to ops-proxy over in-cluster HTTP. Do not point Interactivity URL at ops-proxy (breaks helpdesk).

`POST /ack` on the proxy is the in-cluster API that slack-bot (and tests) call. It is not a substitute for Slack buttons in production.

**Banner:** channel topic (`RED N OPEN · names`, 250 char limit) and/or one pinned “CURRENT INCIDENTS” message that is `chat.update`d. `chat.update` without pin/topic still scrolls away. Extra Slack scopes (pins, topic) are added on the **existing** DPTP Bot app if missing — not a new app.

**Test channel:** `#dptp-robot-testing` before `#ops-testplatform`.

## Ack options (locked)

Menu:

- 2h (skip the next AM `repeat_interval` nag)
- 4h
- 8h
- 16h
- 24h
- 2 days
- Until Monday: **next Monday 00:00 UTC**. Ack after Monday 00:00 UTC → the **following** Monday. A different timezone is a separate product change, not a v1 option.

No 1h. No forever. No until-next-success in v1.

Also:

- **Unack** — delete/expire the silence now
- **NEEDS HUMAN** — do not mute; keep visible (UNKNOWN signatures start here)

**Matchers must be narrow:** `alertname` plus the identity label (`job_name` / `reason` / `job_tail`). **Refuse** a silence that matches only `severity=critical` or only `alertname=infrastructure-job-failures`. **Refuse** if the silence would cover more than one current incident.

**Who may ack:** configurable allowlist (Slack user IDs and/or OpenShift group / rover mapping). Default: Test Platform admins, not every channel member.

**Audit:** who, incident id, matchers, `endsAt`, silence id, which AM. Log + ConfigMap field is enough for v1.

## Persistence and replicas

| v1 | Rule |
|---|---|
| Database | **None.** No Postgres, Redis, S3, or PVC. |
| ConfigMap in `ci` | `incident_id → slack_ts, channel, silence_id, acked_by, ends_at, needs_human`. `silence_id` / `acked_by` / `ends_at` are **display cache only**. Mute authority is always AM `GET /api/v2/silences`. |
| Writes | `resourceVersion` conflict retry |
| Replicas | **1**, RollingUpdate, no Recreate |
| Timers | Do not use a goroutine on the pod that received FIRING as the only clock. AM silence expiry is the mute clock. Reconcile loop rebuilds Slack. |
| Scale to 2 | Only after ConfigMap is proven; HTTP on all pods; optional lease for topic/pin writer |

Helpdesk-faq’s 15-minute in-process cache is **wrong** here. Lost ConfigMap: may post a **second** Slack root (bad but visible). Lost silence because ack never hit AM: **re-page** (fail-safe). Never fail quiet.

On startup:

1. `GET /api/v2/alerts` (firing) and `GET /api/v2/silences` on UWM AM.
2. Load ConfigMap.
3. Upsert cards; recover `ts` from CM, else search recent bot history, else new root + abandon orphan.

## HTTP (this process)

Proposed in-cluster service (names can change at implement time):

| Path | Auth | Purpose |
|---|---|---|
| `GET /api/health` | none (cluster) | Same idea as helpdesk-faq |
| `POST /hook/alertmanager` | shared bearer or HMAC from AM `http_config` | AM webhook. **Not** Slack signing. |
| `POST /ack` | Slack-bot service account or shared token | Called **by slack-bot** after a button click (or by tests). Body: incident id or AM group key, duration enum, Slack user id. Not a public operator UI. |
| `POST /unack` | same | Expire silence |
| `POST /needs-human` | same | Set NEEDS HUMAN |

Do not expose `/hook/alertmanager` on a public Route without auth. slack-bot’s public host stays helpdesk-only.

AM webhook payload: standard Alertmanager JSON (`status`, `groupLabels`, `commonLabels`, `alerts[]`). Honor `send_resolved`. Idempotent upsert on group key + identity.

## RBAC and Alertmanager target

chai-bot today has `alertmanagers/api` **get/list** on platform `main` in `openshift-monitoring`. That is the **wrong AM** for DPTP ci-alerts.

ops-proxy needs:

- `get`/`list`/`create` (and expire/delete as required by the AM API) on **user-workload** Alertmanager in `openshift-user-workload-monitoring` (`alertmanagers/api`, resource name typically `user-workload` — confirm on cluster; do not copy-paste chai-bot RBAC).
- ConfigMap get/create/update in `ci` for the incident store (narrow to one ConfigMap name).
- No cluster-admin. No farm AM write in v1 unless an explicit list of farm AMs is added later.

Silence API: `POST /api/v2/silences`, `GET /api/v2/silences`, expire via the AM v2 delete/expire mechanism. Matchers from incident labels only.

UWM Alertmanager is HTTPS with a service-CA serving cert. The AM HTTP client must load OpenShift `service-ca.crt` (`--alertmanager-ca-path`); the default system trust store is not enough.

## Relationship to openshift/release

Cutover order:

1. Ship ops-proxy (1 replica) + test channel. AM **adds** `webhook_configs` **and keeps** `slack_configs` until the board is trusted (dual path).
2. Bind UWM `monitoring-alertmanager-edit` (or equivalent `alertmanagers/api` create) to the proxy SA.
3. Stay-firing + grouping PRs so cards do not flicker every job run.
4. Drop AM `slack_configs` for ops. **Leave PD on AM** for P0/P1.
5. Farm AM webhooks and crier/brancher upsert are later.

If the proxy is down after Slack cutover: PD still pages P0/P1; Slack board is stale; do not also move PD onto the proxy until it is as reliable as AM.

## v1 vs later

**v1 (this binary + slack-bot forwarder + release wiring)**

- AM UWM webhook ingest
- Identity upsert + board (topic and/or pin)
- Slack Block Kit ack menu + Unack + NEEDS HUMAN (slack-bot forwards clicks)
- AM silences, audit, ConfigMap `ts` cache
- Reconcile on boot (mute from AM silences, not from ConfigMap)
- 1 replica
- Test channel then ops

**Later**

- Farm AM list (git only has build01/02 templates; dumps showed other farms)
- Crier, brancher, chai-bot, Statuspage, ship-status Degraded upsert
- Hours-still-red for weekly review
- P0 cannot be wide-acked (second confirm)
- Playbook id on the card (`UNKNOWN` default); closed scripts stay out of this process
- 2 replicas

**Do not build**

Postgres, Karma/OnCall as the product, forever silence, auto-ack, auto-Jira, war rooms, on-call schedules (PD already), stuffing paging into slack-bot, new Slack app unless isolation is explicitly required later.

## Failure modes

| Failure | Effect | Mitigation |
|---|---|---|
| Proxy down, AM still has slack_configs | Old firehose | Dual path until cutover |
| Proxy down, Slack cut over, PD on AM | Pages still fire; Slack quiet | Intended; fix proxy |
| Proxy down, PD moved to proxy | **Silent pages** | Forbidden in v1 |
| Ack only in ConfigMap, no AM silence | Slack quiet, PD still pages, console “broken” | Ack **is** AM silence |
| Wrong AM (platform `main`) | Silences do nothing for ci-alerts | UWM user-workload only |
| Broad matchers | Mute all infra jobs | Refuse |
| 2 replicas, in-memory map | Duplicate banners, split-brain silences | 1 replica until CM |
| Lost Slack `ts` | Second root message | History search; pin |
| Non-AM pipe (deprovision week) | Firehose bypasses ack | Document; upsert later |

## Implementation sketch (when coding starts)

1. `cmd/ops-proxy/main.go` — flags, health, AM webhook, reconcile loop (`prow/interrupts` like helpdesk-faq).
2. `internal/opsproxy/` — identity, silence client, Slack upsert, ConfigMap store.
3. `images/ops-proxy/Dockerfile` — same ubi-minimal pattern as helpdesk-faq.
4. Makefile `TOOLS` += `ops-proxy`.
5. `openshift/release`: Deployment (1 replica, RollingUpdate, keel like siblings or pin digest), Service (no public Route for AM hook), SA+Role, secret mount `oauth_token`, AM jsonnet `webhook_configs`.
6. Image promotion: add to `ci-operator/config/openshift/ci-tools-standalone/` in release.

Do not git-sync `openshift/release` into this pod. Job config is slack-bot’s problem, not ours.

## Open questions

- Confirm UWM Alertmanager CR / API resource name on app.ci (`user-workload` vs secret name).
- Whether v1 banner is pin-only vs topic+pin (topic overwrites a human-set topic).
- Exact allowlist source (Slack IDs vs OpenShift group).
- Whether dual-path AM Slack+webhook is acceptable noise during soak.
