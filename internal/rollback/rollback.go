package rollback

import (
	"fmt"
	"log"
	"os/exec"
	"time"
)

type Executor struct {
	DryRun bool
}

type Result struct {
	Success     bool
	Message     string
	ExecutedAt  time.Time
	DryRun      bool
}

func NewExecutor(dryRun bool) *Executor {
	return &Executor{DryRun: dryRun}
}

func (e *Executor) Rollback(namespace, deployment string) (*Result, error) {
	result := &Result{
		ExecutedAt: time.Now(),
		DryRun:     e.DryRun,
	}

	cmd := fmt.Sprintf(
		"kubectl rollout undo deployment/%s -n %s",
		deployment, namespace,
	)

	if e.DryRun {
		result.Success = true
		result.Message = fmt.Sprintf("[DRY-RUN] Would execute: %s", cmd)
		log.Println(result.Message)
		return result, nil
	}

	// Actually execute rollback
	log.Printf("Executing rollback: %s", cmd)
	out, err := exec.Command(
		"kubectl", "rollout", "undo",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", namespace,
	).CombinedOutput()

	if err != nil {
		result.Success = false
		result.Message = fmt.Sprintf("rollback failed: %s", string(out))
		return result, fmt.Errorf("kubectl rollout undo failed: %w", err)
	}

	result.Success = true
	result.Message = fmt.Sprintf("Rollback successful: %s", string(out))
	log.Println(result.Message)

	return result, nil
}

func (e *Executor) WaitForRollout(namespace, deployment string) error {
	log.Printf("Waiting for rollback to complete: %s/%s", namespace, deployment)

	out, err := exec.Command(
		"kubectl", "rollout", "status",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", namespace,
		"--timeout=60s",
	).CombinedOutput()

	if err != nil {
		return fmt.Errorf("rollout status failed: %s — %w", string(out), err)
	}

	log.Printf("Rollback complete: %s", string(out))
	return nil
}
