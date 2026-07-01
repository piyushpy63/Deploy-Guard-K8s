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

func Score(current, baseline *Metrics) *Result {
	score := 1.0
	reasons := []string{}

	// --- Error Rate ---
	// Only check if baseline has traffic, otherwise skip
	if baseline.ErrorRate > 0 {
		if current.ErrorRate > baseline.ErrorRate*1.5 {
			score -= 0.4
			reasons = append(reasons, fmt.Sprintf(
				"error_rate %.4f > 1.5x baseline %.4f",
				current.ErrorRate, baseline.ErrorRate,
			))
		}
	}

	// --- P95 Latency ---
	if baseline.P95Latency > 0 {
		if current.P95Latency > baseline.P95Latency*2.0 {
			score -= 0.3
			reasons = append(reasons, fmt.Sprintf(
				"p95_latency %.2fms > 2x baseline %.2fms",
				current.P95Latency, baseline.P95Latency,
			))
		}
	}

	// --- Pod Restarts ---
	// More than 3 new restarts since baseline
	newRestarts := current.PodRestarts - baseline.PodRestarts
	if newRestarts > 3 {
		score -= 0.5
		reasons = append(reasons, fmt.Sprintf(
			"%.0f new pod restarts since baseline",
			newRestarts,
		))
	}

	// --- OOM Kills ---
	// Any OOM kill is serious
	if current.OOMKills > baseline.OOMKills {
		score -= 0.5
		reasons = append(reasons, fmt.Sprintf(
			"%.0f OOM kills detected",
			current.OOMKills-baseline.OOMKills,
		))
	}

	// --- Log Errors ---
	// More than 2x error logs compared to baseline
	if baseline.LogErrors > 0 {
		if current.LogErrors > baseline.LogErrors*2.0 {
			score -= 0.2
			reasons = append(reasons, fmt.Sprintf(
				"log errors %.0f > 2x baseline %.0f",
				current.LogErrors, baseline.LogErrors,
			))
		}
	} else if current.LogErrors > 10 {
		// baseline had 0 errors but now we have errors
		score -= 0.2
		reasons = append(reasons, fmt.Sprintf(
			"%.0f new log errors (baseline was 0)",
			current.LogErrors,
		))
	}

	// Clamp score between 0 and 1
	score = math.Max(0.0, math.Min(1.0, score))

	// Determine verdict
	verdict := "SAFE"
	if score < 0.6 {
		verdict = "ROLLBACK"
	} else if score < 0.8 {
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
