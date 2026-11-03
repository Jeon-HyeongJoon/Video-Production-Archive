package sqlitearchiveorganization

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/organization"
)

var ErrRecordConflict = errors.New("organization record conflicts with stored content")

type Store struct {
	db *sql.DB
}

func Open(databasePath string) (*Store, error) {
	db, err := sql.Open("sqlite", databasePath)
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

func (store *Store) Close() error { return store.db.Close() }

func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS archive_organization_runs (
            archive_run_id TEXT PRIMARY KEY,
            idempotency_key TEXT NOT NULL UNIQUE,
            input_digest TEXT NOT NULL,
            payload BLOB NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS organization_decisions (
            decision_id TEXT PRIMARY KEY,
            archive_run_id TEXT NOT NULL REFERENCES archive_organization_runs(archive_run_id),
            payload BLOB NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS organization_override_receipts (
            idempotency_key TEXT PRIMARY KEY,
            input_digest TEXT NOT NULL,
            payload BLOB NOT NULL
        )`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize organization store: %w", err)
		}
	}
	return nil
}

func (store *Store) FindReceipt(ctx context.Context, idempotencyKey string) (organization.ArchiveOrganizationReceipt, bool, error) {
	var payload []byte
	err := store.db.QueryRowContext(ctx, `SELECT payload FROM archive_organization_runs WHERE idempotency_key = ?`, strings.TrimSpace(idempotencyKey)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return organization.ArchiveOrganizationReceipt{}, false, nil
	}
	if err != nil {
		return organization.ArchiveOrganizationReceipt{}, false, err
	}
	var receipt organization.ArchiveOrganizationReceipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return organization.ArchiveOrganizationReceipt{}, false, err
	}
	return receipt, true, nil
}

func (store *Store) SaveReceipt(ctx context.Context, receipt organization.ArchiveOrganizationReceipt) error {
	if receipt.ID == "" || receipt.IdempotencyKey == "" || receipt.InputDigest == "" || receipt.SessionID == "" ||
		receipt.InputRevision == 0 || receipt.WorkspaceDriveID == "" || receipt.WorkerID == "" || len(receipt.Files) == 0 || len(receipt.Folders) == 0 || receipt.CompletedAt.IsZero() {
		return ErrRecordConflict
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM archive_organization_runs WHERE archive_run_id = ? OR idempotency_key = ?`, receipt.ID, receipt.IdempotencyKey).Scan(&existing)
	if err == nil {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return ErrRecordConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO archive_organization_runs(archive_run_id, idempotency_key, input_digest, payload) VALUES (?, ?, ?, ?)`,
		receipt.ID, receipt.IdempotencyKey, receipt.InputDigest, payload); err != nil {
		return err
	}
	for _, folder := range receipt.Folders {
		decisionPayload, err := json.Marshal(folder.Decision)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO organization_decisions(decision_id, archive_run_id, payload) VALUES (?, ?, ?)`,
			folder.Decision.ID, receipt.ID, decisionPayload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *Store) Receipts(ctx context.Context) ([]organization.ArchiveOrganizationReceipt, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM archive_organization_runs ORDER BY archive_run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []organization.ArchiveOrganizationReceipt
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var receipt organization.ArchiveOrganizationReceipt
		if err := json.Unmarshal(payload, &receipt); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

func (store *Store) LatestDecision(ctx context.Context, workspaceDriveID, folder string) (organization.OrganizationDecision, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM organization_decisions`)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	defer rows.Close()
	var latest organization.OrganizationDecision
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return organization.OrganizationDecision{}, err
		}
		var decision organization.OrganizationDecision
		if err := json.Unmarshal(payload, &decision); err != nil {
			return organization.OrganizationDecision{}, err
		}
		if decision.WorkspaceDriveID == workspaceDriveID && decision.Folder == folder && decision.Sequence > latest.Sequence {
			latest = decision
		}
	}
	if err := rows.Err(); err != nil {
		return organization.OrganizationDecision{}, err
	}
	if latest.ID == "" {
		return organization.OrganizationDecision{}, sql.ErrNoRows
	}
	return latest, nil
}

func (store *Store) LatestDecisionForArchiveRun(ctx context.Context, archiveRunID, folder string) (organization.OrganizationDecision, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM organization_decisions WHERE archive_run_id = ?`, strings.TrimSpace(archiveRunID))
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	defer rows.Close()
	var latest organization.OrganizationDecision
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return organization.OrganizationDecision{}, err
		}
		var decision organization.OrganizationDecision
		if err := json.Unmarshal(payload, &decision); err != nil {
			return organization.OrganizationDecision{}, err
		}
		if decision.Folder == folder && decision.Sequence > latest.Sequence {
			latest = decision
		}
	}
	if err := rows.Err(); err != nil {
		return organization.OrganizationDecision{}, err
	}
	if latest.ID == "" {
		return organization.OrganizationDecision{}, sql.ErrNoRows
	}
	return latest, nil
}

func (store *Store) FindOverrideReceipt(ctx context.Context, idempotencyKey string) (organization.OrganizationDecision, string, bool, error) {
	var digest string
	var payload []byte
	err := store.db.QueryRowContext(ctx, `SELECT input_digest, payload FROM organization_override_receipts WHERE idempotency_key = ?`, strings.TrimSpace(idempotencyKey)).Scan(&digest, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return organization.OrganizationDecision{}, "", false, nil
	}
	if err != nil {
		return organization.OrganizationDecision{}, "", false, err
	}
	var decision organization.OrganizationDecision
	if err := json.Unmarshal(payload, &decision); err != nil {
		return organization.OrganizationDecision{}, "", false, err
	}
	return decision, digest, true, nil
}

func (store *Store) AppendOverride(ctx context.Context, idempotencyKey, inputDigest, previousDecisionID string, decision organization.OrganizationDecision) error {
	if strings.TrimSpace(idempotencyKey) == "" || inputDigest == "" || previousDecisionID == "" || decision.ID == "" || decision.Sequence < 2 {
		return ErrRecordConflict
	}
	payload, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingDigest string
	var existingPayload []byte
	err = tx.QueryRowContext(ctx, `SELECT input_digest, payload FROM organization_override_receipts WHERE idempotency_key = ?`, idempotencyKey).Scan(&existingDigest, &existingPayload)
	if err == nil {
		if existingDigest == inputDigest && bytes.Equal(existingPayload, payload) {
			return nil
		}
		return ErrRecordConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var runID string
	if err := tx.QueryRowContext(ctx, `SELECT archive_run_id FROM organization_decisions WHERE decision_id = ?`, previousDecisionID).Scan(&runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO organization_decisions(decision_id, archive_run_id, payload) VALUES (?, ?, ?)`, decision.ID, runID, payload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO organization_override_receipts(idempotency_key, input_digest, payload) VALUES (?, ?, ?)`, idempotencyKey, inputDigest, payload); err != nil {
		return err
	}
	return tx.Commit()
}
