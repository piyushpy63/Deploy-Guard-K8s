package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/piyushpy63/deploy-guard/internal/audit"
	"github.com/piyushpy63/deploy-guard/internal/baseline"
	"github.com/piyushpy63/deploy-guard/internal/metrics"
	"github.com/piyushpy63/deploy-guard/internal/rollback"
)

// mockMetricsServer returns a mock HTTP server that simulates Prometheus and Loki responses.
// For namespace "ns-a", the baseline capture queries (first 2 calls) return healthy 0.0 metrics,
// and all subsequent polling queries return metrics that trigger a ROLLBACK (high error rate & OOM kills).
// For namespaces "ns-b" and "ns-c", it always returns healthy metrics (SAFE).
func mockMetricsServer() *httptest.Server {
	var mu sync.Mutex
	calls := make(map[string]int)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		ns := "ns-b"
		if strings.Contains(query, `namespace="ns-a"`) || strings.Contains(query, `{namespace="ns-a"}`) {
			ns = "ns-a"
		} else if strings.Contains(query, `namespace="ns-c"`) || strings.Contains(query, `{namespace="ns-c"}`) {
			ns = "ns-c"
		}

		mu.Lock()
		calls[query]++
		count := calls[query]
		mu.Unlock()

		var val float64
		// If it's ns-a and we are past the baseline capture (count > 1 for this query string)
		if ns == "ns-a" && count > 1 {
			if strings.Contains(query, "rate(http_requests_total") {
				val = 1.0 // 100% error rate -> -0.4 penalty
			} else if strings.Contains(query, "kube_pod_container_status_last_terminated_reason") {
				val = 1.0 // OOM Kills -> -0.5 penalty (total = 0.1 score -> ROLLBACK)
			}
		}

		// Respond with standard Prometheus/Loki JSON vector format
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [
					{
						"value": [123456, "%f"]
					}
				]
			}
		}`, val)
	}))
}

func makeDummyDeployment(name, ns, image string) (*appsv1.Deployment, *appsv1.Deployment) {
	oldDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image + ":v0"}},
				},
			},
		},
	}
	newDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image + ":v1"}},
				},
			},
		},
	}
	return oldDeploy, newDeploy
}

// TestWatchLifecycleConcurrency simulates rollout-detected events firing for 3
// different deployments concurrently.
// - It asserts all 3 get independent map entries and independent watcher goroutines.
// - It simulates ns-a/app-a triggering a ROLLBACK, and asserts its map entry is
//   cleanly removed while ns-b/app-b and ns-c/app-c remain active and unaffected.
// - Runs under the Go race detector to ensure all map/mutex lock access patterns
//   are thread-safe.
func TestWatchLifecycleConcurrency(t *testing.T) {
	// Clean up maps at test start.
	watchesMu.Lock()
	activeWatches = make(map[string]context.CancelFunc)
	activeTemplates = make(map[string]string)
	watchesMu.Unlock()

	server := mockMetricsServer()
	defer server.Close()

	prom := metrics.NewPrometheusClient(server.URL)
	loki := metrics.NewLokiClient(server.URL)
	store := baseline.NewStore(t.TempDir())
	executor := rollback.NewExecutor(true) // dry-run = true

	auditLog, err := audit.NewLog(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("failed to create audit log: %v", err)
	}
	defer auditLog.Close()

	// Deployments configuration
	deploys := []struct {
		name, ns string
	}{
		{"app-a", "ns-a"}, // Rollback target
		{"app-b", "ns-b"}, // Safe target
		{"app-c", "ns-c"}, // Safe target
	}

	var wg sync.WaitGroup
	// Fire rollout events concurrently
	for _, d := range deploys {
		wg.Add(1)
		go func(name, ns string) {
			defer wg.Done()
			oldD, newD := makeDummyDeployment(name, ns, "my-app")
			handleUpdate(oldD, newD, ns, prom, loki, store, executor, nil, auditLog, true, 1, 10*time.Minute)
		}(d.name, d.ns)
	}
	wg.Wait()

	// Assert all 3 watches were registered in the map
	watchesMu.Lock()
	count := len(activeWatches)
	watchesMu.Unlock()
	if count != 3 {
		t.Errorf("expected 3 active watches, got %d", count)
	}

	// Poll until ns-a/app-a is cleanly removed from the active map (rollback triggered).
	// With interval=1s, it should trigger rollback on the first tick.
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	success := false
Loop:
	for {
		select {
		case <-timeout:
			break Loop
		case <-ticker.C:
			watchesMu.Lock()
			_, aExists := activeWatches["ns-a/app-a"]
			bExists := activeWatches["ns-b/app-b"] != nil
			cExists := activeWatches["ns-c/app-c"] != nil
			watchesMu.Unlock()

			// Check if ns-a/app-a is cleanly removed and ns-b/ns-c are unaffected
			if !aExists && bExists && cExists {
				success = true
				break Loop
			}
		}
	}

	if !success {
		watchesMu.Lock()
		defer watchesMu.Unlock()
		t.Fatalf("concurrency test timeout or incorrect map state. Map: %v", activeWatches)
	}

	// Clean up remaining watches
	watchesMu.Lock()
	for k, cancel := range activeWatches {
		cancel()
		delete(activeWatches, k)
		delete(activeTemplates, k)
	}
	watchesMu.Unlock()
}

func TestWatchDurationTimeout(t *testing.T) {
	// Clean up maps at test start.
	watchesMu.Lock()
	activeWatches = make(map[string]context.CancelFunc)
	activeTemplates = make(map[string]string)
	watchesMu.Unlock()

	server := mockMetricsServer()
	defer server.Close()

	prom := metrics.NewPrometheusClient(server.URL)
	loki := metrics.NewLokiClient(server.URL)
	store := baseline.NewStore(t.TempDir())
	executor := rollback.NewExecutor(true)

	auditLog, err := audit.NewLog(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("failed to create audit log: %v", err)
	}
	defer auditLog.Close()

	// Use a short watch duration of 2 seconds and interval of 1 second
	watchDuration := 2 * time.Second
	oldD, newD := makeDummyDeployment("app-b", "ns-b", "my-app")
	handleUpdate(oldD, newD, "ns-b", prom, loki, store, executor, nil, auditLog, true, 1, watchDuration)

	// Assert it was registered in activeWatches
	watchesMu.Lock()
	_, exists := activeWatches["ns-b/app-b"]
	watchesMu.Unlock()
	if !exists {
		t.Fatalf("expected watch to be registered in map")
	}

	// Wait for watchDuration to expire (should be > 2 seconds)
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	success := false
Loop:
	for {
		select {
		case <-timeout:
			break Loop
		case <-ticker.C:
			watchesMu.Lock()
			_, exists = activeWatches["ns-b/app-b"]
			watchesMu.Unlock()
			if !exists {
				success = true
				break Loop
			}
		}
	}

	if !success {
		t.Fatalf("watch did not terminate after watch duration exceeded")
	}
}
