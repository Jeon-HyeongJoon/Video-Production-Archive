package sqliteevidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"videoarchive/internal/adapter/sqlitebase"
	"videoarchive/internal/application/capability"
)

type Store struct {
	db      *sql.DB
	release func() error
}

var _ capability.EvidenceSink = (*Store)(nil)

func Open(path string) (*Store, error) {
	db, release, err := sqlitebase.Acquire(path)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, release: release}
	if err := store.migrate(context.Background()); err != nil {
		release()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.release()
}

func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS capability_evidence (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			phase TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			UNIQUE(run_id, phase)
		)`,
		`CREATE INDEX IF NOT EXISTS capability_evidence_recent ON capability_evidence(phase,sequence)`,
		`CREATE INDEX IF NOT EXISTS capability_evidence_run_sequence
			ON capability_evidence(run_id, sequence)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite evidence store: %w", err)
		}
	}
	return nil
}

func (store *Store) Append(ctx context.Context, evidence capability.Evidence) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO capability_evidence(run_id, phase, observed_at, payload)
		VALUES (?, ?, ?, ?)
	`, evidence.RunID, evidence.Phase, evidence.ObservedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), payload)
	return err
}

func (store *Store) ByRun(ctx context.Context, runID string) ([]capability.Evidence, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT payload FROM capability_evidence WHERE run_id = ? ORDER BY sequence
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var evidence []capability.Evidence
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record capability.Evidence
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, err
		}
		evidence = append(evidence, record)
	}
	return evidence, rows.Err()
}

func (store *Store) RecentCapabilityRuns(ctx context.Context) ([]capability.Evidence, error) {
	// Include interrupted invocations at their last recorded phase.
	rows, err := store.db.QueryContext(ctx, `SELECT e.payload FROM capability_evidence e WHERE NOT EXISTS (
 SELECT 1 FROM capability_evidence n WHERE n.run_id=e.run_id AND n.sequence>e.sequence)
 ORDER BY e.sequence DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evidence := []capability.Evidence{}
	for rows.Next() {
		var payload []byte
		var item capability.Evidence
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, err
		}
		evidence = append(evidence, item)
	}
	return evidence, rows.Err()
}
