package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

// parseNamespaces splits a comma-separated string into a deduplicated
// slice of namespace names. Empty entries are silently dropped, and
// duplicates produce a warning but are not fatal — the intent is clear,
// so we just deduplicate and move on.
func parseNamespaces(raw string) ([]string, error) {
	seen := map[string]bool{}
	var result []string

	for _, part := range strings.Split(raw, ",") {
		ns := strings.TrimSpace(part)
		if ns == "" {
			continue // silently skip empty entries (e.g. trailing comma)
		}
		if seen[ns] {
			log.Printf("WARNING: duplicate namespace %q ignored", ns)
			continue
		}
		seen[ns] = true
		result = append(result, ns)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("--watch-namespaces is required and must contain at least one non-empty namespace")
	}

	return result, nil
}

func main() {
	// --- CLI Flags ---
	watchNamespaces := flag.String("watch-namespaces", "", "Comma-separated list of namespaces to watch (required), e.g. \"prod,staging\"")
	labelSelector   := flag.String("label-selector", "deploy-guard/enabled=true", "Label selector for Deployments to watch within the namespaces")
	dryRun          := flag.Bool("dry-run", true, "If true, will not execute rollbacks (e.g. --dry-run=false)")
	interval        := flag.Int("interval", 30, "Polling interval in seconds")
	prometheusURL   := flag.String("prometheus-url", "http://localhost:30769", "Prometheus base URL")
	lokiURL         := flag.String("loki-url", "http://localhost:3100", "Loki base URL")
	slackWebhook    := flag.String("slack-webhook", "", "Slack webhook URL for notifications")
	baselinePath    := flag.String("baseline-path", "/tmp/deploy-guard-baselines", "Directory for baseline snapshots")
	auditDBPath     := flag.String("audit-db", "/tmp/deploy-guard-audit.db", "Path to SQLite audit database")

	flag.Parse()

	// --- Parse and validate namespaces ---
	namespaces, err := parseNamespaces(*watchNamespaces)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintf(os.Stderr, "Usage: deploy-guard --watch-namespaces=prod,staging [flags]\n")
		os.Exit(1)
	}

	// --- Startup ---
	log.Printf("Deploy Guard starting: watching namespaces %v with label selector %s", namespaces, *labelSelector)
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

	// TODO(phase2): Initialize per-namespace watchers with the parsed
	// namespaces and label selector. The watch loop, baseline capture,
	// scoring, and rollback logic will be wired up in the next step.
	log.Println("Flag parsing complete — exiting (watch loop not yet implemented)")
}
