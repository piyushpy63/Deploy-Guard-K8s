package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Snapshot struct {
	DeploymentID string    `json:"deployment_id"`
	Namespace    string    `json:"namespace"`
	CapturedAt   time.Time `json:"captured_at"`
	ErrorRate    float64   `json:"error_rate"`
	P95Latency   float64   `json:"p95_latency"`
	PodRestarts  float64   `json:"pod_restarts"`
	OOMKills     float64   `json:"oom_kills"`
	LogErrors    float64   `json:"log_errors"`
	LogWarns     float64   `json:"log_warns"`
}

// Store persists baseline snapshots as individual JSON files inside a
// directory, keyed by namespace and deployment name. Each file is named
// {namespace}_{deployment}.json so multiple deployments can be watched
// concurrently without clobbering each other.
type Store struct {
	dirPath string
}

// NewStore creates a Store backed by the given directory. The directory
// is created (with parents) if it does not already exist.
func NewStore(dirPath string) *Store {
	_ = os.MkdirAll(dirPath, 0755)
	return &Store{dirPath: dirPath}
}

// baselineFile returns the full path to the JSON file for a given
// namespace/deployment pair.
func (s *Store) baselineFile(namespace, deployment string) string {
	return filepath.Join(s.dirPath, namespace+"_"+deployment+".json")
}

// Save writes a snapshot to disk atomically. The filename is derived from
// the Namespace and DeploymentID fields already present in the Snapshot.
//
// Atomicity: data is first written to a temp file in the same directory,
// then os.Rename() swaps it into place. Rename is atomic on POSIX within
// the same filesystem, so concurrent Load() calls will only ever see the
// old complete file or the new complete file, never a partial write.
func (s *Store) Save(snap *Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	// Create temp file in dirPath (not os.TempDir) so the rename in the
	// next step stays on the same filesystem — required for atomic rename.
	tmp, err := os.CreateTemp(s.dirPath, "baseline-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up the temp file on any failure path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomic swap: rename within the same directory/filesystem.
	finalPath := s.baselineFile(snap.Namespace, snap.DeploymentID)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", finalPath, err)
	}

	success = true
	return nil
}

// Load reads the baseline snapshot for the given namespace/deployment.
// Returns (nil, nil) if no baseline has been captured yet.
func (s *Store) Load(namespace, deployment string) (*Snapshot, error) {
	data, err := os.ReadFile(s.baselineFile(namespace, deployment))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no baseline yet, that's ok
		}
		return nil, fmt.Errorf("failed to read snapshot: %w", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("failed to parse snapshot: %w", err)
	}

	return &snap, nil
}

// Exists reports whether a baseline snapshot exists for the given
// namespace/deployment pair.
func (s *Store) Exists(namespace, deployment string) bool {
	_, err := os.Stat(s.baselineFile(namespace, deployment))
	return !os.IsNotExist(err)
}

// Delete removes the baseline snapshot for the given namespace/deployment.
func (s *Store) Delete(namespace, deployment string) error {
	return os.Remove(s.baselineFile(namespace, deployment))
}
