package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
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

// buildKubeClient creates a Kubernetes clientset, trying in-cluster config
// first (for running inside a pod), then falling back to the kubeconfig
// file (for local development).
func buildKubeClient(kubeconfigPath string) (kubernetes.Interface, error) {
	// Try in-cluster config first (running inside a pod).
	config, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster Kubernetes config")
		return kubernetes.NewForConfig(config)
	}

	// Fall back to kubeconfig file.
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

// isRealRollout compares the PodTemplateSpec of the old and new Deployment
// to detect a real rollout (template change), as opposed to a replica count
// change or a status-only update.
//
// Uses reflect.DeepEqual rather than JSON-hash comparison because:
//   - DeepEqual is the standard approach in the Kubernetes ecosystem (used
//     internally by controller-runtime, kube-controller-manager, etc.)
//   - No serialization overhead or risk of non-deterministic field ordering
//   - Correctly handles unexported fields and nil-vs-empty slice differences
//     in the same way the apiserver does
func isRealRollout(oldDeploy, newDeploy *appsv1.Deployment) bool {
	return !reflect.DeepEqual(oldDeploy.Spec.Template, newDeploy.Spec.Template)
}

func main() {
	// --- CLI Flags ---
	watchNamespaces := flag.String("watch-namespaces", "", "Comma-separated list of namespaces to watch (required), e.g. \"prod,staging\"")
	labelSelector   := flag.String("label-selector", "deploy-guard/enabled=true", "Label selector for Deployments to watch within the namespaces")
	kubeconfig      := flag.String("kubeconfig", "", "Path to kubeconfig file (defaults to ~/.kube/config; ignored when running in-cluster)")
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

	// --- Startup log ---
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

	// Suppress unused-variable warnings for flags not yet wired up.
	_ = dryRun
	_ = interval
	_ = prometheusURL
	_ = lokiURL
	_ = slackWebhook
	_ = baselinePath
	_ = auditDBPath

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
		log.Printf("Received signal %s — shutting down", sig)
		close(stopCh)
	}()

	// --- Informers: one factory per namespace ---
	//
	// client-go's SharedInformerFactory accepts a WithNamespace option that
	// scopes the List/Watch to a single namespace. For multiple namespaces
	// we create one factory per namespace rather than a single cluster-wide
	// informer because:
	//   - Minimal RBAC: only needs get/list/watch in the target namespaces,
	//     not cluster-wide Deployment access.
	//   - Precise: no need to filter out irrelevant namespaces in the event
	//     handler — the informer only sees what we asked for.
	//   - Standard pattern: this is how multi-namespace controllers are
	//     built in the Kubernetes ecosystem.
	//
	// The label selector is injected via WithTweakListOptions so the
	// apiserver does the filtering server-side (not client-side).

	selector := *labelSelector
	factories := make([]informers.SharedInformerFactory, 0, len(namespaces))

	for _, ns := range namespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(
			clientset,
			60*time.Second, // resync period
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = selector
			}),
		)

		deployInformer := factory.Apps().V1().Deployments().Informer()

		// Capture ns for the closure.
		namespace := ns

		deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldDeploy, ok1 := oldObj.(*appsv1.Deployment)
				newDeploy, ok2 := newObj.(*appsv1.Deployment)
				if !ok1 || !ok2 {
					return
				}

				if isRealRollout(oldDeploy, newDeploy) {
					log.Printf("rollout detected: namespace=%s deployment=%s",
						namespace, newDeploy.Name)
				}
			},
		})

		factories = append(factories, factory)
		log.Printf("Informer registered for namespace=%s selector=%q", ns, selector)
	}

	// --- Start all informer factories ---
	for _, f := range factories {
		f.Start(stopCh)
	}

	// Wait for initial cache sync.
	for _, f := range factories {
		f.WaitForCacheSync(stopCh)
	}
	log.Println("All informer caches synced — watching for rollouts")

	// Block until signal.
	<-stopCh
	log.Println("Deploy Guard stopped")
}
