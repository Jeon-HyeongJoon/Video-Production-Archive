package sqlitearchiveworkspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"videoarchive/internal/application/archiveworkspace"
)

type Store struct {
	db *sql.DB
}

var _ archiveworkspace.Reader = (*Store)(nil)

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
	_, err := store.db.ExecContext(ctx, `
		PRAGMA busy_timeout = 5000;
		PRAGMA journal_mode = WAL;
		CREATE TABLE IF NOT EXISTS archive_workspace_job_projections (
			job_id TEXT PRIMARY KEY,
			revision INTEGER NOT NULL,
			needs_intervention INTEGER NOT NULL,
			last_event_at TEXT NOT NULL,
			payload BLOB NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("initialize sqlite archive workspace store: %w", err)
	}
	return nil
}

// Replace stores a rebuildable read projection. It is not part of the
// ArchiveWorkspace query interface and does not write to the authoritative ledger.
func (store *Store) Replace(ctx context.Context, view archiveworkspace.JobView) error {
	if view.Summary.JobID == "" || view.Summary.LastEventAt.IsZero() {
		return archiveworkspace.ErrInvalidProjection
	}
	payload, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO archive_workspace_job_projections(job_id, revision, needs_intervention, last_event_at, payload)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
			revision = excluded.revision,
			needs_intervention = excluded.needs_intervention,
			last_event_at = excluded.last_event_at,
			payload = excluded.payload
	`, view.Summary.JobID, view.Summary.Revision, view.Summary.NeedsHumanIntervention,
		view.Summary.LastEventAt.UTC().Format(time.RFC3339Nano), payload)
	return err
}

func (store *Store) List(ctx context.Context) ([]archiveworkspace.JobView, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT payload FROM archive_workspace_job_projections
		ORDER BY needs_intervention DESC, last_event_at DESC, job_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []archiveworkspace.JobView
	for rows.Next() {
		view, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (store *Store) Get(ctx context.Context, jobID string) (archiveworkspace.JobView, error) {
	row := store.db.QueryRowContext(ctx, `SELECT payload FROM archive_workspace_job_projections WHERE job_id = ?`, jobID)
	view, err := scanView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return archiveworkspace.JobView{}, archiveworkspace.ErrJobNotFound
	}
	return view, err
}

type scanner interface {
	Scan(...any) error
}

func scanView(row scanner) (archiveworkspace.JobView, error) {
	var payload []byte
	if err := row.Scan(&payload); err != nil {
		return archiveworkspace.JobView{}, err
	}
	var view archiveworkspace.JobView
	if err := json.Unmarshal(payload, &view); err != nil {
		return archiveworkspace.JobView{}, err
	}
	return view, nil
}
