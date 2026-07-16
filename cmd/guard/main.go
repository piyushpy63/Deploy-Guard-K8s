package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/piyushpy63/deploy-guard/internal/audit"
	"github.com/piyushpy63/deploy-guard/internal/baseline"
	"github.com/piyushpy63/deploy-guard/internal/metrics"
	"github.com/piyushpy63/deploy-guard/internal/notify"
	"github.com/piyushpy63/deploy-guard/internal/rollback"
	"github.com/piyushpy63/deploy-guard/internal/scorer"
)

var (
	activeWatches   = make(map[string]context.CancelFunc)
	activeTemplates = make(map[string]string)
	watchesMu       sync.Mutex
)

// getTemplateHash computes a SHA256 hash of the PodTemplateSpec to uniquely
// identify the deployment's template state.
func getTemplateHash(deploy *appsv1.Deployment) string {
	data, _ := json.Marshal(deploy.Spec.Template)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// parseNamespaces splits a comma-separated string into a deduplicated
// slice of namespace names. Empty entries are silently dropped, and
// duplicates produce a warning but are not fatal.
func parseNamespaces(raw string) ([]string, error) {
	seen := map[string]bool{}
	var result []string

	for _, part := range strings.Split(raw, ",") {
		ns := strings.TrimSpace(part)
		if ns == "" {
			continue
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

// buildKubeClient creates a Kubernetes clientset, trying in-cluster config
// first, then falling back to the kubeconfig file.
func buildKubeClient(kubeconfigPath string) (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster Kubernetes config")
		return kubernetes.NewForConfig(config)
	}

	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	}
	config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig from %s: %w", kubeconfigPath, err)
	}

	log.Printf("Using kubeconfig: %s", kubeconfigPath)
	return kubernetes.NewForConfig(config)
}

func isRealRollout(oldDeploy, newDeploy *appsv1.Deployment) bool {
	return !reflect.DeepEqual(oldDeploy.Spec.Template, newDeploy.Spec.Template)
}

// handleUpdate processes the old and new deployment objects, detects real
// rollouts, manages watcher goroutines using the thread-safe map, captures
// baseline metrics, and starts a polling loop. Exposing this function makes
// the core watch lifecycle cleanly unit-testable.
func handleUpdate(
	oldObj, newObj interface{},
	namespace string,
	prom *metrics.PrometheusClient,
	loki *metrics.LokiClient,
	store *baseline.Store,
	executor *rollback.Executor,
	slack *notify.SlackNotifier,
	auditLog *audit.Log,
	dryRun bool,
	interval int,
) {
	oldDeploy, ok1 := oldObj.(*appsv1.Deployment)
	newDeploy, ok2 := newObj.(*appsv1.Deployment)
	if !ok1 || !ok2 {
		return
	}

	if isRealRollout(oldDeploy, newDeploy) {
		key := fmt.Sprintf("%s/%s", namespace, newDeploy.Name)
		hash := getTemplateHash(newDeploy)

		watchesMu.Lock()
		cancel, exists := activeWatches[key]
		if exists {
			activeHash := activeTemplates[key]
			if activeHash == hash {
				log.Printf("rollout already being watched, ignoring duplicate event: namespace=%s deployment=%s",
					namespace, newDeploy.Name)
				watchesMu.Unlock()
				return
			}

			log.Printf("superseding in-progress watch for namespace=%s deployment=%s due to new rollout",
				namespace, newDeploy.Name)
			cancel()
			delete(activeWatches, key)
			delete(activeTemplates, key)
		}

		ctx, cancelFunc := context.WithCancel(context.Background())
		activeWatches[key] = cancelFunc
		activeTemplates[key] = hash
		watchesMu.Unlock()

		// Spawn the real watcher goroutine
		go func(ctx context.Context, ns, name string, cancelFunc context.CancelFunc) {
			log.Printf("Started watch goroutine for %s/%s", ns, name)
			defer func() {
				log.Printf("Stopped watch goroutine for %s/%s", ns, name)
				watchesMu.Lock()
				// Only clean up from map if our own cancel func is still registered
				if activeTemplates[key] == hash {
					delete(activeWatches, key)
					delete(activeTemplates, key)
				}
				watchesMu.Unlock()
			}()

			// 1. Capture baseline metrics
			log.Printf("Capturing baseline metrics for %s/%s...", ns, name)
			promResult, err := prom.GetMetrics(ns)
			if err != nil {
				log.Printf("ERROR capturing Prometheus baseline for %s/%s: %v", ns, name, err)
				return
			}

			lokiResult, err := loki.GetLogMetrics(ns)
			if err != nil {
				log.Printf("ERROR capturing Loki baseline for %s/%s: %v", ns, name, err)
				return
			}

			snap := &baseline.Snapshot{
				DeploymentID: name + "-baseline",
				Namespace:    ns,
				CapturedAt:   time.Now(),
				ErrorRate:    promResult.ErrorRate,
				P95Latency:   promResult.P95Latency,
				PodRestarts:  promResult.PodRestarts,
				OOMKills:     promResult.OOMKills,
				LogErrors:    lokiResult.ErrorCount,
				LogWarns:     lokiResult.WarnCount,
			}

			if err := store.Save(snap); err != nil {
				log.Printf("ERROR saving baseline snapshot for %s/%s: %v", ns, name, err)
				return
			}
			log.Printf("Successfully captured baseline snapshot for %s/%s", ns, name)

			lastVerdict := ""
			pollTicker := time.NewTicker(time.Duration(interval) * time.Second)
			defer pollTicker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-pollTicker.C:
					log.Printf("--- Polling cycle for %s/%s ---", ns, name)

					// Query current metrics
					currProm, err := prom.GetMetrics(ns)
					if err != nil {
						log.Printf("ERROR querying Prometheus metrics for %s/%s: %v", ns, name, err)
						continue
					}

					currLoki, err := loki.GetLogMetrics(ns)
					if err != nil {
						log.Printf("ERROR querying Loki metrics for %s/%s: %v", ns, name, err)
						continue
					}

					current := &scorer.Metrics{
						ErrorRate:   currProm.ErrorRate,
						P95Latency:  currProm.P95Latency,
						PodRestarts: currProm.PodRestarts,
						OOMKills:    currProm.OOMKills,
						LogErrors:   currLoki.ErrorCount,
					}

					baseMetrics := &scorer.Metrics{
						ErrorRate:   snap.ErrorRate,
						P95Latency:  snap.P95Latency,
						PodRestarts: snap.PodRestarts,
						OOMKills:    snap.OOMKills,
						LogErrors:   snap.LogErrors,
					}

					result := scorer.Score(current, baseMetrics)
					reasons := strings.Join(result.Reasons, " | ")

					log.Printf("[%s/%s] Score:   %.2f", ns, name, result.Score)
					log.Printf("[%s/%s] Verdict: %s", ns, name, result.Verdict)
					log.Printf("[%s/%s] Reasons: %s", ns, name, reasons)

					// Send Slack alert on state change
					if slack != nil && result.Verdict != lastVerdict {
						log.Printf("[%s/%s] Verdict changed %q -> %q, sending Slack notification...", ns, name, lastVerdict, result.Verdict)
						if err := slack.Send(ns, name, result.Verdict, result.Score, reasons, dryRun); err != nil {
							log.Printf("ERROR sending Slack notification: %v", err)
						}
					}
					lastVerdict = result.Verdict

					switch result.Verdict {
					case "ROLLBACK":
						log.Printf("🚨 ROLLBACK triggered for %s/%s", ns, name)
						rollbackDone := false
						rbResult, err := executor.Rollback(ns, name)
						if err != nil {
							log.Printf("ERROR executing rollback: %v", err)
						} else {
							log.Printf("Rollback result: %s", rbResult.Message)
							rollbackDone = true
						}

						// Write to audit log
						entry := &audit.Entry{
							Timestamp:    time.Now(),
							DeploymentID: snap.DeploymentID,
							Namespace:    ns,
							Score:        result.Score,
							Verdict:      result.Verdict,
							Reasons:      reasons,
							DryRun:       dryRun,
							RollbackDone: rollbackDone,
						}
						if err := auditLog.Write(entry); err != nil {
							log.Printf("ERROR writing to audit log: %v", err)
						}

						// Remove the stale baseline file
						if err := store.Delete(ns, name); err != nil {
							log.Printf("ERROR deleting baseline snapshot: %v", err)
						}

						// Cancel self (terminates goroutine & triggers cleanup)
						cancelFunc()
						return

					case "WARN":
						log.Printf("⚠️  WARN — watching %s/%s closely", ns, name)

					case "SAFE":
						log.Printf("✅ SAFE — %s/%s deployment looks healthy", ns, name)
					}
				}
			}
		}(ctx, namespace, newDeploy.Name, cancelFunc)
	}
}

func main() {
	// --- CLI Flags ---
	watchNamespaces := flag.String("watch-namespaces", "", "Comma-separated list of namespaces to watch (required)")
	labelSelector   := flag.String("label-selector", "deploy-guard/enabled=true", "Label selector for Deployments to watch")
	kubeconfig      := flag.String("kubeconfig", "", "Path to kubeconfig file")
	dryRun          := flag.Bool("dry-run", true, "If true, will not execute rollbacks")
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

	// --- Startup log ---
	log.Printf("Deploy Guard starting: watching namespaces %v with label selector %s", namespaces, *labelSelector)
	log.Printf("Dry-run:        %v", *dryRun)
	log.Printf("Interval:       %ds", *interval)
	log.Printf("Prometheus:     %s", *prometheusURL)
	log.Printf("Loki:           %s", *lokiURL)
	log.Printf("Baseline path:  %s", *baselinePath)
	log.Printf("Audit DB:       %s", *auditDBPath)

	if *slackWebhook == "" {
		log.Println("Slack:          disabled")
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

	// --- Kubernetes client ---
	clientset, err := buildKubeClient(*kubeconfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// --- Stop channel: wired to SIGINT/SIGTERM for clean shutdown ---
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %s — shutting down cleanly", sig)

		watchesMu.Lock()
		for key, cancel := range activeWatches {
			log.Printf("Cancelling active watch for key %s", key)
			cancel()
		}
		watchesMu.Unlock()

		close(stopCh)
	}()

	// --- Informers: one factory per namespace ---
	selector := *labelSelector
	factories := make([]informers.SharedInformerFactory, 0, len(namespaces))

	for _, ns := range namespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(
			clientset,
			60*time.Second,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = selector
			}),
		)

		deployInformer := factory.Apps().V1().Deployments().Informer()
		namespace := ns

		deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(oldObj, newObj interface{}) {
				handleUpdate(oldObj, newObj, namespace, prom, loki, store, executor, slack, auditLog, *dryRun, *interval)
			},
		})

		factories = append(factories, factory)
		log.Printf("Informer registered for namespace=%s selector=%q", ns, selector)
	}

	for _, f := range factories {
		f.Start(stopCh)
	}

	for _, f := range factories {
		f.WaitForCacheSync(stopCh)
	}
	log.Println("All informer caches synced — watching for rollouts")

	<-stopCh
	log.Println("Deploy Guard stopped")
}
