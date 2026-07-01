package audit

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Entry struct {
	ID           int64
	Timestamp    time.Time
	DeploymentID string
	Namespace    string
	Score        float64
	Verdict      string
	Reasons      string
	DryRun       bool
	RollbackDone bool
}

type Log struct {
	db *sql.DB
}

func NewLog(dbPath string) (*Log, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	// Create table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp     DATETIME NOT NULL,
			deployment_id TEXT NOT NULL,
			namespace     TEXT NOT NULL,
			score         REAL NOT NULL,
			verdict       TEXT NOT NULL,
			reasons       TEXT NOT NULL,
			dry_run       BOOLEAN NOT NULL,
			rollback_done BOOLEAN NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	return &Log{db: db}, nil
}

func (l *Log) Write(entry *Entry) error {
	_, err := l.db.Exec(`
		INSERT INTO audit_log 
		(timestamp, deployment_id, namespace, score, verdict, reasons, dry_run, rollback_done)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Timestamp.UTC(),
		entry.DeploymentID,
		entry.Namespace,
		entry.Score,
		entry.Verdict,
		entry.Reasons,
		entry.DryRun,
		entry.RollbackDone,
	)
	if err != nil {
		return fmt.Errorf("failed to write audit entry: %w", err)
	}
	return nil
}

func (l *Log) Recent(limit int) ([]*Entry, error) {
	rows, err := l.db.Query(`
		SELECT id, timestamp, deployment_id, namespace, score, verdict, reasons, dry_run, rollback_done
		FROM audit_log
		ORDER BY timestamp DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query audit log: %w", err)
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e := &Entry{}
		err := rows.Scan(
			&e.ID,
			&e.Timestamp,
			&e.DeploymentID,
			&e.Namespace,
			&e.Score,
			&e.Verdict,
			&e.Reasons,
			&e.DryRun,
			&e.RollbackDone,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (l *Log) Close() {
	l.db.Close()
}

func (l *Log) Print(limit int) error {
	entries, err := l.Recent(limit)
	if err != nil {
		return err
	}

	fmt.Printf("\n%-4s %-25s %-15s %-6s %-10s %s\n",
		"ID", "TIMESTAMP", "DEPLOYMENT", "SCORE", "VERDICT", "REASONS")
	fmt.Println(strings.Repeat("-", 90))

	for _, e := range entries {
		fmt.Printf("%-4d %-25s %-15s %-6.2f %-10s %s\n",
			e.ID,
			e.Timestamp.Format("2006-01-02 15:04:05"),
			e.DeploymentID,
			e.Score,
			e.Verdict,
			e.Reasons,
		)
	}
	return nil
}
