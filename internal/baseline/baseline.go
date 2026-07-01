package baseline

import (
	"encoding/json"
	"fmt"
	"os"
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

type Store struct {
	filePath string
}

func NewStore(filePath string) *Store {
	return &Store{filePath: filePath}
}

func (s *Store) Save(snap *Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	if err := os.WriteFile(s.filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write snapshot: %w", err)
	}

	return nil
}

func (s *Store) Load() (*Snapshot, error) {
	data, err := os.ReadFile(s.filePath)
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

func (s *Store) Exists() bool {
	_, err := os.Stat(s.filePath)
	return !os.IsNotExist(err)
}

func (s *Store) Delete() error {
	return os.Remove(s.filePath)
}
