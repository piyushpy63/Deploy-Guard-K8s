package baseline

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestMultiDeploymentBaseline saves two baselines for different
// namespace/deployment pairs and verifies they are stored independently.
func TestMultiDeploymentBaseline(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	snapA := &Snapshot{
		DeploymentID: "app-a",
		Namespace:    "production",
		CapturedAt:   time.Now(),
		ErrorRate:    0.01,
		P95Latency:   100,
	}
	snapB := &Snapshot{
		DeploymentID: "app-b",
		Namespace:    "staging",
		CapturedAt:   time.Now(),
		ErrorRate:    0.05,
		P95Latency:   250,
	}

	// Save both
	if err := store.Save(snapA); err != nil {
		t.Fatalf("failed to save snapA: %v", err)
	}
	if err := store.Save(snapB); err != nil {
		t.Fatalf("failed to save snapB: %v", err)
	}

	// Both should exist
	if !store.Exists("production", "app-a") {
		t.Fatal("expected app-a baseline to exist")
	}
	if !store.Exists("staging", "app-b") {
		t.Fatal("expected app-b baseline to exist")
	}

	// Load each and verify they got the right data
	loadedA, err := store.Load("production", "app-a")
	if err != nil {
		t.Fatalf("failed to load app-a: %v", err)
	}
	if loadedA.ErrorRate != 0.01 {
		t.Errorf("app-a error rate: got %.4f, want 0.01", loadedA.ErrorRate)
	}

	loadedB, err := store.Load("staging", "app-b")
	if err != nil {
		t.Fatalf("failed to load app-b: %v", err)
	}
	if loadedB.ErrorRate != 0.05 {
		t.Errorf("app-b error rate: got %.4f, want 0.05", loadedB.ErrorRate)
	}
}

// TestDeleteOnlyTargetBaseline verifies that deleting one baseline
// does not affect others.
func TestDeleteOnlyTargetBaseline(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	snapA := &Snapshot{
		DeploymentID: "app-a",
		Namespace:    "default",
		CapturedAt:   time.Now(),
		ErrorRate:    0.02,
	}
	snapB := &Snapshot{
		DeploymentID: "app-b",
		Namespace:    "default",
		CapturedAt:   time.Now(),
		ErrorRate:    0.03,
	}

	if err := store.Save(snapA); err != nil {
		t.Fatalf("failed to save snapA: %v", err)
	}
	if err := store.Save(snapB); err != nil {
		t.Fatalf("failed to save snapB: %v", err)
	}

	// Delete app-a only
	if err := store.Delete("default", "app-a"); err != nil {
		t.Fatalf("failed to delete app-a: %v", err)
	}

	// app-a should be gone
	if store.Exists("default", "app-a") {
		t.Error("app-a should not exist after delete")
	}

	// app-b should still be there and loadable
	if !store.Exists("default", "app-b") {
		t.Fatal("app-b should still exist after deleting app-a")
	}
	loadedB, err := store.Load("default", "app-b")
	if err != nil {
		t.Fatalf("failed to load app-b after deleting app-a: %v", err)
	}
	if loadedB.ErrorRate != 0.03 {
		t.Errorf("app-b error rate: got %.4f, want 0.03", loadedB.ErrorRate)
	}
}

// TestLoadNonExistent verifies that loading a baseline that was never
// saved returns (nil, nil), not an error.
func TestLoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	snap, err := store.Load("nope", "does-not-exist")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if snap != nil {
		t.Fatalf("expected nil snapshot, got: %+v", snap)
	}
}

// TestNewStoreCreatesDirectory verifies that NewStore creates the
// directory if it doesn't exist.
func TestNewStoreCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := dir + "/sub/baselines"

	_ = NewStore(nested)

	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("path should be a directory")
	}
}

// TestAtomicSaveConcurrentLoad spawns a writer goroutine calling Save()
// in a loop and a reader goroutine calling Load() concurrently. The test
// asserts that Load() never returns a json.Unmarshal error — it should
// always see either the old complete file or the new complete file, never
// a partially-written one.
//
// Run with: go test -race ./internal/baseline/ -run TestAtomicSaveConcurrentLoad
func TestAtomicSaveConcurrentLoad(t *testing.T) {
	const iterations = 500

	dir := t.TempDir()
	store := NewStore(dir)

	// Seed an initial snapshot so Load() never gets (nil, nil).
	initial := &Snapshot{
		DeploymentID: "app-stress",
		Namespace:    "test",
		CapturedAt:   time.Now(),
		ErrorRate:    0.0,
	}
	if err := store.Save(initial); err != nil {
		t.Fatalf("failed to save initial snapshot: %v", err)
	}

	var wg sync.WaitGroup

	// Writer goroutine: saves snapshots with incrementing ErrorRate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			snap := &Snapshot{
				DeploymentID: "app-stress",
				Namespace:    "test",
				CapturedAt:   time.Now(),
				ErrorRate:    float64(i) / float64(iterations),
				P95Latency:   float64(i),
			}
			if err := store.Save(snap); err != nil {
				t.Errorf("Save failed on iteration %d: %v", i, err)
				return
			}
		}
	}()

	// Reader goroutine: loads the same baseline concurrently.
	loadErrors := make(chan string, iterations)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			snap, err := store.Load("test", "app-stress")
			if err != nil {
				loadErrors <- fmt.Sprintf("Load iteration %d: %v", i, err)
				return
			}
			if snap == nil {
				loadErrors <- fmt.Sprintf("Load iteration %d: got nil snapshot", i)
				return
			}
			// Sanity: ErrorRate should be a value we actually wrote.
			if snap.ErrorRate < 0 || snap.ErrorRate > 1.0 {
				loadErrors <- fmt.Sprintf(
					"Load iteration %d: ErrorRate %.4f out of expected range [0, 1]",
					i, snap.ErrorRate,
				)
			}
		}
	}()

	wg.Wait()
	close(loadErrors)

	for msg := range loadErrors {
		t.Error(msg)
	}
}
