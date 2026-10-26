package sqlitecommand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/command"
)

type Store struct {
	db *sql.DB
}

var _ command.Store = (*Store)(nil)

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS aggregate_states (
			target_id TEXT PRIMARY KEY,
			revision INTEGER NOT NULL,
			state BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS command_records (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			command_id TEXT NOT NULL,
			target_id TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS domain_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			target_id TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS idempotency_receipts (
			idempotency_key TEXT PRIMARY KEY,
			digest TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite command store: %w", err)
		}
	}
	return nil
}

func (store *Store) Seed(ctx context.Context, snapshot command.Snapshot) error {
	if snapshot.TargetID == "" || snapshot.State == nil {
		return command.ErrInvalidChange
	}
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO aggregate_states(target_id, revision, state)
		VALUES (?, ?, ?)
		ON CONFLICT(target_id) DO UPDATE SET revision = excluded.revision, state = excluded.state
	`, snapshot.TargetID, snapshot.Revision, []byte(snapshot.State))
	return err
}

func (store *Store) Load(targetID string) (command.Snapshot, error) {
	var revision int64
	var state []byte
	err := store.db.QueryRow(`SELECT revision, state FROM aggregate_states WHERE target_id = ?`, targetID).Scan(&revision, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return command.Snapshot{}, command.ErrTargetNotFound
	}
	if err != nil {
		return command.Snapshot{}, err
	}
	return command.Snapshot{TargetID: targetID, Revision: uint64(revision), State: json.RawMessage(state)}, nil
}

func (store *Store) FindReceipt(idempotencyKey string) (command.Receipt, bool, error) {
	var payload []byte
	err := store.db.QueryRow(`SELECT payload FROM idempotency_receipts WHERE idempotency_key = ?`, idempotencyKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return command.Receipt{}, false, nil
	}
	if err != nil {
		return command.Receipt{}, false, err
	}
	var receipt command.Receipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return command.Receipt{}, false, err
	}
	return receipt, true, nil
}

func (store *Store) Commit(idempotencyKey string, commit command.Commit) error {
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if commit.NextState != nil {
		if commit.ExpectedRevision == nil {
			return command.ErrInvalidChange
		}
		if commit.CreateTarget {
			if *commit.ExpectedRevision != 0 || commit.NextState.Revision != 1 {
				return command.ErrInvalidChange
			}
			var exists int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM aggregate_states WHERE target_id = ?`, commit.NextState.TargetID).Scan(&exists); err != nil {
				return err
			}
			if exists != 0 {
				return command.ErrRevisionConflict
			}
			if _, err := tx.Exec(`
				INSERT INTO aggregate_states(target_id, revision, state) VALUES (?, ?, ?)
			`, commit.NextState.TargetID, commit.NextState.Revision, []byte(commit.NextState.State)); err != nil {
				return err
			}
		} else {
			result, err := tx.Exec(`
				UPDATE aggregate_states
				SET revision = ?, state = ?
				WHERE target_id = ? AND revision = ?
			`, commit.NextState.Revision, []byte(commit.NextState.State), commit.NextState.TargetID, *commit.ExpectedRevision)
			if err != nil {
				return err
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if updated != 1 {
				var exists int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM aggregate_states WHERE target_id = ?`, commit.NextState.TargetID).Scan(&exists); err != nil {
					return err
				}
				if exists == 0 {
					return command.ErrTargetNotFound
				}
				return command.ErrRevisionConflict
			}
		}
	}

	recordPayload, err := json.Marshal(commit.Record)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO command_records(command_id, target_id, payload) VALUES (?, ?, ?)
	`, commit.Record.Envelope.CommandID, commit.Record.Envelope.TargetID, recordPayload); err != nil {
		return err
	}
	for _, event := range commit.Events {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO domain_events(event_id, target_id, payload) VALUES (?, ?, ?)
		`, event.EventID, event.TargetID, payload); err != nil {
			return err
		}
	}
	if commit.Receipt != nil {
		payload, err := json.Marshal(commit.Receipt)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO idempotency_receipts(idempotency_key, digest, payload) VALUES (?, ?, ?)
		`, idempotencyKey, commit.Receipt.Digest, payload); err != nil {
			return command.ErrIdempotencyConflict
		}
	}
	return tx.Commit()
}
