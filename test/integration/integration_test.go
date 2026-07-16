package integration

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	_ "modernc.org/sqlite"
)

// TestE2ERolloutIntegration runs a full end-to-end integration test against the
// actual k3s cluster. It compiles the deploy-guard binary, launches it as a
// subprocess, creates a temporary test deployment, triggers a rollout spec change,
// injects failure metrics via a mock Prometheus/Loki server, and asserts that
// deploy-guard successfully rolls back the deployment and records the audit log.
func TestE2ERolloutIntegration(t *testing.T) {
	// 1. Compile the deploy-guard binary.
	binPath := filepath.Join(t.TempDir(), "deploy-guard-test")
	cmdBuild := exec.Command("go", "build", "-o", binPath, "../../cmd/guard/main.go")
	cmdBuild.Dir = "."
	if output, err := cmdBuild.CombinedOutput(); err != nil {
		t.Fatalf("failed to compile deploy-guard binary: %v, output: %s", err, string(output))
	}

	// 2. Set up clientset to talk to k3s.
	home, _ := os.UserHomeDir()
	kubeconfig := filepath.Join(home, ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("failed to load kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create kubernetes client: %v", err)
	}

	// Ensure demo namespace exists.
	nsClient := clientset.CoreV1().Namespaces()
	if _, err := nsClient.Get(context.TODO(), "demo", metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace 'demo' does not exist in k3s cluster: %v", err)
	}

	// 3. Create a temporary test deployment.
	deployName := "deploy-guard-integration-test"
	deployClient := clientset.AppsV1().Deployments("demo")

	// Delete any leftover test deployment.
	_ = deployClient.Delete(context.TODO(), deployName, metav1.DeleteOptions{})

	testDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: "demo",
			Labels: map[string]string{
				"app":                  deployName,
				"deploy-guard/enabled": "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": deployName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": deployName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "web",
							Image: "nginx:1.25.0",
							Env: []corev1.EnvVar{
								{Name: "TEST_ENV", Value: "initial-value"},
							},
						},
					},
				},
			},
		},
	}

	_, err = deployClient.Create(context.TODO(), testDeploy, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create test deployment: %v", err)
	}
	defer func() {
		// Clean up the deployment at the end of the test.
		_ = deployClient.Delete(context.TODO(), deployName, metav1.DeleteOptions{})
	}()

	// 4. Spin up mock Prometheus/Loki metrics server to inject failure states.
	var mu sync.Mutex
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		mu.Lock()
		calls[query]++
		count := calls[query]
		mu.Unlock()

		var val float64
		// Return 0.0 for baseline queries (count == 1).
		// Return failure metrics on subsequent polls (count > 1) to trigger rollback.
		if count > 1 {
			if strings.Contains(query, "rate(http_requests_total") {
				val = 1.5 // High error rate -> -0.4 penalty
			} else if strings.Contains(query, "kube_pod_container_status_last_terminated_reason") {
				val = 1.0 // OOM Kills -> -0.5 penalty (total = 0.1 score -> ROLLBACK)
			}
		}

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
	defer server.Close()

	// 5. Setup temp dirs for baselines and SQLite DB.
	tmpDir := t.TempDir()
	baselinePath := filepath.Join(tmpDir, "baselines")
	auditDBPath := filepath.Join(tmpDir, "audit.db")

	// 6. Launch the deploy-guard binary as a subprocess.
	// We run it with --dry-run=false so it actually executes the rollback.
	// We set the polling interval to 2 seconds for quick testing.
	cmdGuard := exec.Command(binPath,
		"--watch-namespaces=demo",
		"--label-selector=app="+deployName,
		"--prometheus-url="+server.URL,
		"--loki-url="+server.URL,
		"--baseline-path="+baselinePath,
		"--audit-db="+auditDBPath,
		"--interval=2",
		"--dry-run=false",
	)

	// Start deploy-guard in the background.
	if err := cmdGuard.Start(); err != nil {
		t.Fatalf("failed to start deploy-guard subprocess: %v", err)
	}
	defer func() {
		if cmdGuard.Process != nil {
			_ = cmdGuard.Process.Signal(syscall.SIGTERM)
			_ = cmdGuard.Wait()
		}
	}()

	// Wait for informer cache to sync
	time.Sleep(3 * time.Second)

	// 7. Trigger a spec rollout update.
	// Retrieve the latest version from the cluster first to avoid resourceVersion conflicts.
	latestDeploy, err := deployClient.Get(context.TODO(), deployName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get latest deployment: %v", err)
	}
	latestDeploy.Spec.Template.Spec.Containers[0].Env[0].Value = "updated-value"
	_, err = deployClient.Update(context.TODO(), latestDeploy, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update deployment (trigger rollout): %v", err)
	}

	// 8. Poll and verify:
	// - The deployment should revert to "initial-value".
	// - The SQLite database should have an entry with ROLLBACK.
	timeout := time.After(20 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	success := false
Loop:
	for {
		select {
		case <-timeout:
			break Loop
		case <-ticker.C:
			// Check if the deployment env was reverted
			curr, err := deployClient.Get(context.TODO(), deployName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			val := curr.Spec.Template.Spec.Containers[0].Env[0].Value
			if val == "initial-value" {
				// Reverted successfully! Verify the SQLite audit database.
				db, err := sql.Open("sqlite", auditDBPath)
				if err != nil {
					continue
				}
				var count int
				var verdict string
				err = db.QueryRow("SELECT COUNT(*), verdict FROM audit_log").Scan(&count, &verdict)
				db.Close()
				if err == nil && count > 0 && verdict == "ROLLBACK" {
					success = true
					break Loop
				}
			}
		}
	}

	if !success {
		t.Fatal("E2E integration test failed: deployment was not rolled back or audit log entry was not written within timeout")
	}
}

func int32Ptr(i int32) *int32 { return &i }
