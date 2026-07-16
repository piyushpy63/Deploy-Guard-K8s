package scorer

import (
	"fmt"
	"math"
)

type Metrics struct {
	ErrorRate   float64
	P95Latency  float64
	PodRestarts float64
	OOMKills    float64
	LogErrors   float64
}

type Result struct {
	Score   float64
	Verdict string
	Reasons []string
}

// Policy holds every tunable threshold and penalty used by the scorer.
// Use DefaultPolicy() to get the current production defaults.
type Policy struct {
	// --- Error Rate ---
	ErrorRateMultiplier float64 // current vs baseline multiplier (e.g. 1.5 = 50% above baseline)
	ErrorRatePenalty    float64 // score deduction when threshold is breached
	ErrorRateFloor      float64 // absolute floor when baseline is 0 (placeholder default)

	// --- P95 Latency ---
	P95LatencyMultiplier float64 // current vs baseline multiplier (e.g. 2.0 = double baseline)
	P95LatencyPenalty    float64 // score deduction when threshold is breached
	P95LatencyFloor      float64 // absolute floor in ms when baseline is 0 (placeholder default)

	// --- Pod Restarts ---
	PodRestartThreshold float64 // new restarts above baseline to trigger penalty
	PodRestartPenalty   float64 // score deduction when threshold is breached

	// --- OOM Kills ---
	OOMKillPenalty float64 // score deduction per OOM event

	// --- Log Errors ---
	LogErrorMultiplier float64 // current vs baseline multiplier
	LogErrorPenalty    float64 // score deduction when threshold is breached
	LogErrorFloor      float64 // absolute floor when baseline is 0

	// --- Verdict Thresholds ---
	RollbackThreshold float64 // score below this → ROLLBACK
	WarnThreshold     float64 // score below this (but above RollbackThreshold) → WARN
}

// DefaultPolicy returns a Policy pre-filled with the current production
// defaults — the same values that were previously hardcoded in Score().
func DefaultPolicy() *Policy {
	return &Policy{
		ErrorRateMultiplier: 1.5,
		ErrorRatePenalty:    0.4,
		ErrorRateFloor:      0.05, // placeholder default: 5% (not tuned)

		P95LatencyMultiplier: 2.0,
		P95LatencyPenalty:    0.3,
		P95LatencyFloor:      1000, // placeholder default: 1000ms (not tuned)

		PodRestartThreshold: 3,
		PodRestartPenalty:   0.5,

		OOMKillPenalty: 0.5,

		LogErrorMultiplier: 2.0,
		LogErrorPenalty:    0.2,
		LogErrorFloor:      10,

		RollbackThreshold: 0.6,
		WarnThreshold:     0.8,
	}
}

// Score scores current metrics against a baseline using the default policy.
// This is a backward-compatible wrapper around ScoreWithPolicy.
func Score(current, baseline *Metrics) *Result {
	return ScoreWithPolicy(current, baseline, DefaultPolicy())
}

// ScoreWithPolicy scores current metrics against a baseline using the
// supplied policy. The function is pure and stateless — all tunables come
// from the policy struct.
func ScoreWithPolicy(current, baseline *Metrics, policy *Policy) *Result {
	score := 1.0
	reasons := []string{}

	// --- Error Rate ---
	if baseline.ErrorRate > 0 {
		if current.ErrorRate > baseline.ErrorRate*policy.ErrorRateMultiplier {
			score -= policy.ErrorRatePenalty
			reasons = append(reasons, fmt.Sprintf(
				"error_rate %.4f > %.1fx baseline %.4f",
				current.ErrorRate, policy.ErrorRateMultiplier, baseline.ErrorRate,
			))
		}
	} else if current.ErrorRate > policy.ErrorRateFloor {
		// Baseline had 0 errors but canary is showing real problems.
		score -= policy.ErrorRatePenalty
		reasons = append(reasons, fmt.Sprintf(
			"error_rate %.4f exceeds floor %.4f (baseline was 0)",
			current.ErrorRate, policy.ErrorRateFloor,
		))
	}

	// --- P95 Latency ---
	if baseline.P95Latency > 0 {
		if current.P95Latency > baseline.P95Latency*policy.P95LatencyMultiplier {
			score -= policy.P95LatencyPenalty
			reasons = append(reasons, fmt.Sprintf(
				"p95_latency %.2fms > %.1fx baseline %.2fms",
				current.P95Latency, policy.P95LatencyMultiplier, baseline.P95Latency,
			))
		}
	} else if current.P95Latency > policy.P95LatencyFloor {
		// Baseline had 0 latency but canary is showing real problems.
		score -= policy.P95LatencyPenalty
		reasons = append(reasons, fmt.Sprintf(
			"p95_latency %.2fms exceeds floor %.2fms (baseline was 0)",
			current.P95Latency, policy.P95LatencyFloor,
		))
	}

	// --- Pod Restarts ---
	newRestarts := current.PodRestarts - baseline.PodRestarts
	if newRestarts > policy.PodRestartThreshold {
		score -= policy.PodRestartPenalty
		reasons = append(reasons, fmt.Sprintf(
			"%.0f new pod restarts since baseline",
			newRestarts,
		))
	}

	// --- OOM Kills ---
	if current.OOMKills > baseline.OOMKills {
		score -= policy.OOMKillPenalty
		reasons = append(reasons, fmt.Sprintf(
			"%.0f OOM kills detected",
			current.OOMKills-baseline.OOMKills,
		))
	}

	// --- Log Errors ---
	if baseline.LogErrors > 0 {
		if current.LogErrors > baseline.LogErrors*policy.LogErrorMultiplier {
			score -= policy.LogErrorPenalty
			reasons = append(reasons, fmt.Sprintf(
				"log errors %.0f > %.0fx baseline %.0f",
				current.LogErrors, policy.LogErrorMultiplier, baseline.LogErrors,
			))
		}
	} else if current.LogErrors > policy.LogErrorFloor {
		// baseline had 0 errors but now we have errors
		score -= policy.LogErrorPenalty
		reasons = append(reasons, fmt.Sprintf(
			"%.0f new log errors (baseline was 0)",
			current.LogErrors,
		))
	}

	// Clamp score between 0 and 1
	score = math.Max(0.0, math.Min(1.0, score))

	// Determine verdict
	verdict := "SAFE"
	if score < policy.RollbackThreshold {
		verdict = "ROLLBACK"
	} else if score < policy.WarnThreshold {
		verdict = "WARN"
	}

	// If no reasons, deployment looks healthy
	if len(reasons) == 0 {
		reasons = append(reasons, "all metrics within baseline thresholds")
	}

	return &Result{
		Score:   score,
		Verdict: verdict,
		Reasons: reasons,
	}
}
