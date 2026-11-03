package artifactpublish

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/workflow"
)

var ErrPublishReceiptNotFound = errors.New("publish receipt not found")

type Receipt struct {
	StagingID     string           `json:"staging_id"`
	StepID        string           `json:"step_id"`
	PublishedPath string           `json:"published_path"`
	Checksum      string           `json:"checksum"`
	Artifact      archive.Artifact `json:"artifact"`
}

type Processor interface {
	Run(context.Context, string) error
}

type Repository struct {
	db *sql.DB
}

func OpenRepository(path string) (*Repository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	repository := &Repository{db: db}
	if err := repository.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Close() error {
	return repository.db.Close()
}

func (repository *Repository) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS publish_receipts (
			staging_id TEXT PRIMARY KEY,
			step_id TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS publish_steps (
			step_id TEXT PRIMARY KEY,
			state TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS published_artifacts (
			artifact_id TEXT PRIMARY KEY,
			payload BLOB NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize publish repository: %w", err)
		}
	}
	return nil
}

func (repository *Repository) SaveInterruptedPublish(ctx context.Context, receipt Receipt) error {
	if receipt.StagingID == "" || receipt.StepID == "" || receipt.PublishedPath == "" || receipt.Checksum == "" ||
		receipt.Artifact.Path != receipt.PublishedPath || receipt.Artifact.Checksum != receipt.Checksum || receipt.Artifact.ValidateDerived() != nil {
		return archive.ErrInvalidProvenance
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO publish_receipts(staging_id, step_id, payload) VALUES (?, ?, ?)
	`, receipt.StagingID, receipt.StepID, payload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO publish_steps(step_id, state) VALUES (?, ?)
	`, receipt.StepID, workflow.Running); err != nil {
		return err
	}
	return tx.Commit()
}

func (repository *Repository) receipt(ctx context.Context, stagingID string) (Receipt, error) {
	var payload []byte
	if err := repository.db.QueryRowContext(ctx, `SELECT payload FROM publish_receipts WHERE staging_id = ?`, stagingID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrPublishReceiptNotFound
	} else if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (repository *Repository) finalize(ctx context.Context, receipt Receipt) error {
	payload, err := json.Marshal(receipt.Artifact)
	if err != nil {
		return err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO published_artifacts(artifact_id, payload) VALUES (?, ?)
		ON CONFLICT(artifact_id) DO UPDATE SET payload = excluded.payload
	`, receipt.Artifact.ID, payload); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE publish_steps SET state = ? WHERE step_id = ?`, workflow.Verifying, receipt.StepID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return workflow.ErrIllegalTransition
	}
	return tx.Commit()
}

func (repository *Repository) Artifact(ctx context.Context, artifactID string) (archive.Artifact, bool, error) {
	var payload []byte
	if err := repository.db.QueryRowContext(ctx, `SELECT payload FROM published_artifacts WHERE artifact_id = ?`, artifactID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archive.Artifact{}, false, nil
	} else if err != nil {
		return archive.Artifact{}, false, err
	}
	var artifact archive.Artifact
	if err := json.Unmarshal(payload, &artifact); err != nil {
		return archive.Artifact{}, false, err
	}
	return artifact, true, nil
}

func (repository *Repository) StepState(ctx context.Context, stepID string) (workflow.StepState, error) {
	var state string
	if err := repository.db.QueryRowContext(ctx, `SELECT state FROM publish_steps WHERE step_id = ?`, stepID).Scan(&state); err != nil {
		return "", err
	}
	return workflow.StepState(state), nil
}

type Reconciler struct {
	repository *Repository
	processor  Processor
}

func NewReconciler(repository *Repository, processor Processor) *Reconciler {
	return &Reconciler{repository: repository, processor: processor}
}

func (reconciler *Reconciler) Reconcile(ctx context.Context, stagingID string) error {
	receipt, err := reconciler.repository.receipt(ctx, stagingID)
	if err != nil {
		return err
	}
	valid, err := publishedFileMatches(receipt)
	if err != nil {
		return err
	}
	if !valid {
		return reconciler.processor.Run(ctx, stagingID)
	}
	return reconciler.repository.finalize(ctx, receipt)
}

func publishedFileMatches(receipt Receipt) (bool, error) {
	file, err := os.Open(receipt.PublishedPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return false, err
	}
	return size == receipt.Artifact.Size && hex.EncodeToString(hash.Sum(nil)) == receipt.Checksum, nil
}
