# ops-proxy

Notification proxy for Test Platform on-call. Prometheus and Alertmanager still decide what is firing. This process owns the Slack board, ack-until-T, and Alertmanager silences so operators do not need the Alertmanager UI.

**Status:** v1 binary in this repo. See [DESIGN.md](DESIGN.md) for the full contract.

- Binary: `cmd/ops-proxy`
- Image: `quay.io/openshift/ci:ci_ops-proxy_latest` (name confirmed at image wiring)
- Deploy manifests: `openshift/release` (not this repo)
- Slack: existing DPTP Bot `oauth_token` (`slack-credentials-dptp-bot`); no new Slack app
- Related: `openshift/ci-tools` slack-bot stays helpdesk; it **must** forward Ack button payloads (`action_id` prefix `ops-proxy:`) because DPTP Bot has a single Interactivity URL

## HTTP

| Path | Auth | Purpose |
|------|------|---------|
| `GET /api/health` | none | Liveness. JSON `{"ok":true}` |
| `POST /hook/alertmanager` | `Authorization: Bearer` from `--hook-token-path` | Alertmanager webhook |
| `POST /ack` | `Authorization: Bearer` from `--api-token-path` | Create AM silence until T |
| `POST /unack` | same | Expire matching silences |
| `POST /needs-human` | same | ConfigMap flag only; does not silence |

`/ack`, `/unack`, and `/needs-human` also require `slack_user_id` in `--ack-allowlist`. An empty allowlist denies all (fail closed).

Ack JSON body:

```json
{"incident_id":"infrastructure-job-failures/periodic-foo","duration":"24h","slack_user_id":"U123"}
```

Durations: `2h`, `4h`, `8h`, `16h`, `24h`, `2d`, `monday` (next Monday 00:00 UTC; if already Monday, the following Monday).

## Flags

| Flag | Default | Required | Meaning |
|------|---------|----------|---------|
| `--port` | `8080` | | HTTP listen port |
| `--gracePeriod` | `10s` | | Shutdown grace period |
| `--log-level` | `info` | | logrus level |
| `--kubeconfig` | in-cluster | | From `flagutil.KubernetesOptions` |
| `--kubeconfig-dir` | | | From `flagutil.KubernetesOptions` |
| `--slack-token-path` | | yes | DPTP Bot `oauth_token` file |
| `--slack-channel` | `#dptp-robot-testing` | | Cards and CURRENT INCIDENTS board |
| `--set-channel-topic` | `true` | | Topic `RED N OPEN · names` (max 250) |
| `--hook-token-path` | | yes | Bearer for `/hook/alertmanager` |
| `--api-token-path` | | yes | Bearer for `/ack` `/unack` `/needs-human` |
| `--ack-allowlist` | empty (deny all) | | Comma-separated Slack user IDs |
| `--alertmanager-url` | | yes | AM base URL (`/api/v2/alerts`, `/api/v2/silences`) |
| `--alertmanager-token-path` | | | Optional AM API bearer (in-cluster SA) |
| `--configmap-namespace` | `ci` | | Incident store |
| `--configmap-name` | `ops-proxy` | | Single JSON blob in data key `state` |
| `--reconcile-interval` | `1m` | | Rebuild cards from AM firing + silences |

## Mute authority

**ACKED** only if a matching Alertmanager silence exists (`GET /api/v2/silences`). ConfigMap `silence_id` / `acked_by` / `ends_at` are a display cache. If ListSilences fails, cards stay **OPEN** (unknown mute, fail visible); the cache is not copied into muted state.

Ack of a coalesced `ci-operator-error/<reason>` card POSTs **two** silences (`high-ci-operator-error-rate` and `high-ci-operator-infra-error-rate` AND that `reason`). Slack still shows one card.

## v1 limitations

- **Slack `ts` recovery:** ConfigMap only. If the ConfigMap is lost, the proxy posts a new root message (fail visible). Searching `conversations.history` is not implemented.
- **`POST /ack` body:** `incident_id` from card buttons. Alertmanager `groupKey` is not accepted.
- **`--set-channel-topic`:** defaults to `true` (topic banner plus pinned CURRENT INCIDENTS). DESIGN discussed defaulting this off; not changed in this pass.
