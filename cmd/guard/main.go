package main

import (
	"flag"
	"log"
	"strings"
	"time"

	"github.com/piyushpy63/deploy-guard/internal/audit"
	"github.com/piyushpy63/deploy-guard/internal/baseline"
	"github.com/piyushpy63/deploy-guard/internal/metrics"
	"github.com/piyushpy63/deploy-guard/internal/notify"
	"github.com/piyushpy63/deploy-guard/internal/rollback"
	"github.com/piyushpy63/deploy-guard/internal/scorer"
)

func main() {
	// --- CLI Flags ---
	namespace      := flag.String("namespace", "demo", "Kubernetes namespace to watch")
	deployment     := flag.String("deployment", "sample-app", "Deployment name to watch")
	dryRun         := flag.Bool("dry-run", true, "If true, will not execute rollbacks")
	interval       := flag.Int("interval", 30, "Polling interval in seconds")
	prometheusURL  := flag.String("prometheus-url", "http://localhost:30769", "Prometheus base URL")
	lokiURL        := flag.String("loki-url", "http://localhost:3100", "Loki base URL")
	slackWebhook   := flag.String("slack-webhook", "", "Slack webhook URL for notifications")
	baselinePath   := flag.String("baseline-path", "/tmp/deploy-guard-baseline.json", "Path to baseline JSON file")
	auditDBPath    := flag.String("audit-db", "/tmp/deploy-guard-audit.db", "Path to SQLite audit database")

	flag.Parse()

	// --- Startup ---
	log.Println("Deploy Guard starting...")
	log.Printf("Namespace:      %s", *namespace)
	log.Printf("Deployment:     %s", *deployment)
	log.Printf("Dry-run:        %v", *dryRun)
	log.Printf("Interval:       %ds", *interval)
	log.Printf("Prometheus:     %s", *prometheusURL)
	log.Printf("Loki:           %s", *lokiURL)
	log.Printf("Baseline path:  %s", *baselinePath)
	log.Printf("Audit DB:       %s", *auditDBPath)

	if *slackWebhook == "" {
		log.Println("Slack:          disabled (no webhook provided)")
	} else {
		log.Println("Slack:          enabled")
	}

	// --- Init clients ---
	prom     := metrics.NewPrometheusClient(*prometheusURL)
	loki     := metrics.NewLokiClient(*lokiURL)
	store    := baseline.NewStore(*baselinePath)
	executor := rollback.NewExecutor(*dryRun)

	var slack *notify.SlackNotifier
	if *slackWebhook != "" {
		slack = notify.NewSlackNotifier(*slackWebhook)
	}

	// --- Audit log ---
	auditLog, err := audit.NewLog(*auditDBPath)
	if err != nil {
		log.Fatalf("Failed to create audit log: %v", err)
	}
	defer auditLog.Close()
	log.Printf("Audit log ready at %s", *auditDBPath)

	// --- Capture baseline ---
	if !store.Exists() {
		log.Println("No baseline found — capturing now...")

		promResult, err := prom.GetMetrics(*namespace)
		if err != nil {
			log.Fatalf("Failed to get prometheus metrics: %v", err)
		}

		lokiResult, err := loki.GetLogMetrics(*namespace)
		if err != nil {
			log.Fatalf("Failed to get loki metrics: %v", err)
		}

		snap := &baseline.Snapshot{
			DeploymentID: *deployment + "-baseline",
			Namespace:    *namespace,
			CapturedAt:   time.Now(),
			ErrorRate:    promResult.ErrorRate,
			P95Latency:   promResult.P95Latency,
			PodRestarts:  promResult.PodRestarts,
			OOMKills:     promResult.OOMKills,
			LogErrors:    lokiResult.ErrorCount,
			LogWarns:     lokiResult.WarnCount,
		}

		if err := store.Save(snap); err != nil {
			log.Fatalf("Failed to save baseline: %v", err)
		}
		log.Println("Baseline captured!")
	} else {
		snap, _ := store.Load()
		log.Printf("Baseline loaded from %s", snap.CapturedAt.Format(time.RFC3339))
	}

	// --- Polling loop ---
	lastVerdict := ""

	for {
		log.Println("--- Polling cycle ---")

		snap, err := store.Load()
		if err != nil {
			log.Printf("ERROR loading baseline: %v", err)
			time.Sleep(time.Duration(*interval) * time.Second)
			continue
		}

		promResult, err := prom.GetMetrics(*namespace)
		if err != nil {
			log.Printf("ERROR querying prometheus: %v", err)
			time.Sleep(time.Duration(*interval) * time.Second)
			continue
		}

		lokiResult, err := loki.GetLogMetrics(*namespace)
		if err != nil {
			log.Printf("ERROR querying loki: %v", err)
			time.Sleep(time.Duration(*interval) * time.Second)
			continue
		}

		current := &scorer.Metrics{
			ErrorRate:   promResult.ErrorRate,
			P95Latency:  promResult.P95Latency,
			PodRestarts: promResult.PodRestarts,
			OOMKills:    promResult.OOMKills,
			LogErrors:   lokiResult.ErrorCount,
		}

		base := &scorer.Metrics{
			ErrorRate:   snap.ErrorRate,
			P95Latency:  snap.P95Latency,
			PodRestarts: snap.PodRestarts,
			OOMKills:    snap.OOMKills,
			LogErrors:   snap.LogErrors,
		}

		result := scorer.Score(current, base)
		reasons := strings.Join(result.Reasons, " | ")

		log.Printf("Score:   %.2f", result.Score)
		log.Printf("Verdict: %s", result.Verdict)
		log.Printf("Reasons: %s", reasons)

		rollbackDone := false

		switch result.Verdict {
		case "ROLLBACK":
			log.Printf("🚨 ROLLBACK triggered for %s/%s", *namespace, *deployment)
			rbResult, err := executor.Rollback(*namespace, *deployment)
			if err != nil {
				log.Printf("ERROR during rollback: %v", err)
			} else {
				log.Printf("Rollback result: %s", rbResult.Message)
				rollbackDone = true
			}

			if !*dryRun {
				store.Delete()
				log.Println("Baseline reset after rollback")
			}

		case "WARN":
			log.Printf("⚠️  WARN — watching closely")

		case "SAFE":
			log.Printf("✅ SAFE — deployment looks healthy")
		}

		// Send Slack only when verdict changes
		if slack != nil && result.Verdict != lastVerdict {
			log.Printf("Verdict changed %s → %s, sending Slack...", lastVerdict, result.Verdict)
			err := slack.Send(*namespace, *deployment, result.Verdict, result.Score, reasons, *dryRun)
			if err != nil {
				log.Printf("ERROR sending Slack: %v", err)
			} else {
				log.Printf("Slack notification sent!")
			}
		}
		lastVerdict = result.Verdict

		// Write audit log
		auditEntry := &audit.Entry{
			Timestamp:    time.Now(),
			DeploymentID: snap.DeploymentID,
			Namespace:    *namespace,
			Score:        result.Score,
			Verdict:      result.Verdict,
			Reasons:      reasons,
			DryRun:       *dryRun,
			RollbackDone: rollbackDone,
		}

		if err := auditLog.Write(auditEntry); err != nil {
			log.Printf("ERROR writing audit log: %v", err)
		}

		auditLog.Print(5)

		time.Sleep(time.Duration(*interval) * time.Second)
	}
}
