# Technical Rationale: Dynamic Health Scoring

This document outlines the design decisions and technical rationale behind Deploy Guard’s dynamic health scoring mechanism.

## 1. The Failure of Static Thresholds

Automated canary analysis systems often rely on static thresholds (e.g., "rollback if error rate > 5%"). In production, this approach fails due to two primary dimensions of operational variance:

1. **Traffic Scale Disparity:** A 5% error rate on a low-traffic service (e.g., a 20 requests/minute payments service) might represent a single transient network timeout. On a high-traffic service (e.g., a 50,000 requests/second catalog service), it represents 2,500 failed operations per second—a severe outage. Using the same static threshold triggers false positives on low-traffic deployments while failing to protect high-traffic ones.
2. **Temporal Variance:** Systems experience normal fluctuations based on time of day or batch processing loads. For instance, a database migration batch job might normally run with a 2% baseline error rate. Setting a static threshold of 1% to protect API endpoints would constantly block healthy batch job deployments, whereas setting it to 5% would let a broken payments endpoint fail silently.

## 2. Dynamic Ratio-Based Scoring

To handle this variance, Deploy Guard evaluates health dynamically. For each rollout, it captures a pre-deployment baseline snapshot (the preceding stable state of the specific deployment) and compares the new deployment's metrics against it as a ratio.

Instead of hard gates, multiple metrics are combined into a unified score (clamped between `0.0` and `1.0`), starting at `1.0` and applying penalties for breaches:

| Metric | Condition | Score Penalty | Rationale |
| :--- | :--- | :--- | :--- |
| **Pod Restarts** | `restarts > baseline + 3` | `-0.5` | Unambiguous failure; container crash looping. |
| **OOM Kills** | `OOMKills > baseline` | `-0.5` | Unambiguous failure; resource limit exceeded. |
| **Error Rate** | `current > baseline * 1.5` | `-0.4` | Server-side degradation; scale-independent. |
| **P95 Latency** | `current > baseline * 2.0` | `-0.3` | Performance degradation; can be transient. |
| **Log Errors** | `current > baseline * 2.0` | `-0.2` | Application-level logging warnings/errors. |

*Note: Score < 0.6 triggers an automatic ROLLBACK; Score 0.6 to 0.8 triggers a WARN.*

OOM kills and Pod restarts carry the highest weight (`-0.5`). Unlike latencies or error rates—which can fluctuate due to downstream database locks or external network hiccups—container crashes and out-of-memory events are deterministic, absolute indicators of internal application instability.

## 3. The Zero-Baseline Edge Case

A naive implementation of ratio-based scoring breaks when the baseline is zero (e.g., a service with a history of zero restarts or zero errors). If a metric baseline is `0`, checking `current > baseline * Multiplier` simplifies to `current > 0`, which is overly sensitive, or the check is skipped entirely if not handled explicitly.

Deploy Guard solves this using an absolute fallback floor configuration (e.g., `ErrorRateFloor` at `5%` and `P95LatencyFloor` at `1000ms`). When the baseline is `0`, the logic automatically switches to this floor:
```go
if baseline.ErrorRate > 0 {
    if current.ErrorRate > baseline.ErrorRate * policy.ErrorRateMultiplier {
        score -= policy.ErrorRatePenalty
    }
} else if current.ErrorRate > policy.ErrorRateFloor {
    score -= policy.ErrorRatePenalty
}
```
This ensures zero-error deployments are protected against catastrophic failure while avoiding spurious alerts on transient anomalies.

## 4. Operational Limitations

Deploy Guard's scoring model contains explicit trade-offs:
- **Cold-Start Problem:** Dynamic baselines require active traffic. A brand-new service with zero traffic cannot establish a meaningful baseline, reducing the system's sensitivity until traffic starts.
- **External Dependency Noise:** The engine evaluates namespace-wide Prometheus metrics. If a shared downstream database degrades, the rollout under observation will baseline against normal and subsequently fail the check, triggering a false-positive rollback.
- **Static Multipliers:** Although metrics are compared dynamically to baselines, the multipliers (e.g., `1.5x`, `2.0x`) and penalties are static defaults rather than adaptive or ML-driven.
