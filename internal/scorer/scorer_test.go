package scorer

import "testing"

// TestZeroBaselineHighErrorRate proves the bug: when baseline metrics are all
// zero (no prior traffic), a canary with a 50% error rate should be flagged
// as ROLLBACK, not SAFE.
func TestZeroBaselineHighErrorRate(t *testing.T) {
	baseline := &Metrics{
		ErrorRate:   0,
		P95Latency:  0,
		PodRestarts: 0,
		OOMKills:    0,
		LogErrors:   0,
	}
	current := &Metrics{
		ErrorRate:   0.5,  // 50% error rate — clearly broken
		P95Latency:  2000, // 2000ms — way above the 1000ms floor
		PodRestarts: 0,
		OOMKills:    0,
		LogErrors:   0,
	}

	result := Score(current, baseline)

	if result.Verdict != "ROLLBACK" {
		t.Errorf("expected verdict ROLLBACK, got %s (score=%.2f, reasons=%v)",
			result.Verdict, result.Score, result.Reasons)
	}
}

// TestScoreWithCustomPolicy verifies that ScoreWithPolicy respects a
// custom Policy rather than only using hardcoded defaults.
func TestScoreWithCustomPolicy(t *testing.T) {
	baseline := &Metrics{
		ErrorRate:   0.01, // 1% baseline error rate
		P95Latency:  100,
		PodRestarts: 0,
		OOMKills:    0,
		LogErrors:   0,
	}
	current := &Metrics{
		ErrorRate:   0.012, // 1.2% — only 20% above baseline
		P95Latency:  100,
		PodRestarts: 0,
		OOMKills:    0,
		LogErrors:   0,
	}

	// With default policy (1.5x multiplier), 1.2x is fine → SAFE.
	defaultResult := Score(current, baseline)
	if defaultResult.Verdict != "SAFE" {
		t.Fatalf("expected SAFE with default policy, got %s (score=%.2f)",
			defaultResult.Verdict, defaultResult.Score)
	}

	// With a tighter policy (1.1x multiplier), 1.2x now breaches → ROLLBACK.
	strict := DefaultPolicy()
	strict.ErrorRateMultiplier = 1.1
	strict.RollbackThreshold = 0.8 // anything below 0.8 is rollback

	strictResult := ScoreWithPolicy(current, baseline, strict)
	if strictResult.Verdict != "ROLLBACK" {
		t.Errorf("expected ROLLBACK with strict policy, got %s (score=%.2f, reasons=%v)",
			strictResult.Verdict, strictResult.Score, strictResult.Reasons)
	}
}
