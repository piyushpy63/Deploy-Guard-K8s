# Deploy Guard 🛡️
> Automated Kubernetes Deployment Safety System — detects bad deployments and triggers automatic rollbacks within 30 seconds using deterministic health scoring against pre-deployed baselines.

---

## Tech Stack

| Layer | Tool |
|---|---|
| Language | Go 1.25 |
| Metrics | Prometheus |
| Logs | Grafana Loki + Vector |
| Audit | SQLite |
| Alerts | Slack Webhooks |
| Runtime | Kubernetes (k3s) on AWS EC2 |

---

## Why Go?

- **Single binary** — `go build` produces one file, no runtime dependencies, just copy and run
- **Memory efficient** — Deploy Guard uses ~20MB RAM vs ~200MB for Python equivalent — critical on a small EC2 instance
- **Native Kubernetes support** — `client-go` is Go-first; rollback, RBAC, pod watching all feel native
- **Built-in concurrency** — goroutines handle polling loop, HTTP calls, and audit writes without threads

---

## Requirements

- Kubernetes cluster (k3s/ K8s)
- Prometheus + kube-state-metrics
- Grafana Loki
- Vector DaemonSet (log collector)
- Go 1.25+
- Docker
- Slack Webhook URL

---

## Architecture & Data Flow

```
┌──────────────────────────────────────────┐
│              EC2 / k3s                   │
│                                          │
│  Prometheus ──┐                          │
│               ├──► Deploy Guard Pod      │
│  Loki ────────┘         │                │
│                         │                │
│            ┌────────────┼────────────┐   │
│            ▼            ▼            ▼   │
│        Rollback      Slack         SQLite│
│       (kubectl)     Notify         Audit │
└──────────────────────────────────────────┘

Every 30s:
  Query Prometheus + Loki
        ↓
  Compare vs pre-deploy baseline
        ↓
  Score health (0.0 → 1.0)
        ↓
  SAFE / WARN / ROLLBACK
```

---

## Problem & Solution

**Without Deploy Guard**
```
Bad deploy → users see errors → someone paged
→ human checks Grafana → manual rollback
= 10-30 minutes downtime
```

**With Deploy Guard**
```
Bad deploy → guard detects in 30s
→ automatic rollback + Slack alert
= 30 seconds impact
```

**Why not Grafana alerts?** They fire on static thresholds — if your normal error rate is already 3%, a "alert above 5%" rule is meaningless. Deploy Guard captures what normal looks like *before* the deploy and compares against that.

---

## Scoring Algorithm

Deterministic rule engine — no AI, no LLM. Safety-critical decisions need to be predictable and explainable.

```go
score := 1.0

if error_rate    > baseline * 1.5  → score -= 0.4
if p95_latency   > baseline * 2.0  → score -= 0.3
if pod_restarts  > baseline + 3    → score -= 0.5
if oom_kills     > baseline        → score -= 0.5
if log_errors    > baseline * 2.0  → score -= 0.2

score >= 0.8  → SAFE
score 0.6-0.8 → WARN      + Slack ⚠️
score < 0.6   → ROLLBACK  + Slack 🚨 + kubectl rollout undo
```

Every decision is written to SQLite with timestamp, score, verdict, and reasons — full audit trail.

Audit Log Sample Output

ID   TIMESTAMP              SCORE  VERDICT   REASONS
52   2026-06-29 05:54:08    0.50   ROLLBACK  8 new pod restarts since baseline
51   2026-06-29 05:53:54    0.70   WARN      8 new pod restarts since baseline
20   2026-06-29 05:38:23    1.00   SAFE      all metrics within baseline thresholds

---

## Quick Start

```bash
# Run locally in dry-run mode
./deploy-guard \
  --namespace demo \
  --deployment sample-app \
  --dry-run \
  --prometheus-url http://localhost:30769 \
  --loki-url http://localhost:3100 \
  --slack-webhook https://hooks.slack.com/YOUR/URL

# Deploy to k3s
kubectl apply -f k8s/
kubectl logs -n deploy-guard deployment/deploy-guard -f
```


