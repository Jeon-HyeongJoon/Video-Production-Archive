package sqlitearchiveintake

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/archiveintake"
)

var (
	ErrRecordNotFound = errors.New("archive intake record not found")
	ErrRecordConflict = errors.New("archive intake record conflicts with stored content")
)

type Repository struct {
	db *sql.DB
}

func Open(databasePath string) (*Repository, error) {
	db, err := sql.Open("sqlite", databasePath)
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

func (repository *Repository) Close() error { return repository.db.Close() }

func (repository *Repository) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS archive_intake_candidates (
            volume_id TEXT PRIMARY KEY,
            revision INTEGER NOT NULL,
            payload BLOB NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS archive_intake_sessions (
            session_id TEXT PRIMARY KEY,
            volume_id TEXT NOT NULL,
            payload BLOB NOT NULL
        )`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize archive intake repository: %w", err)
		}
	}
	return nil
}

func (repository *Repository) SaveCandidate(ctx context.Context, candidate archiveintake.Candidate) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	var revision uint64
	var existing []byte
	err = repository.db.QueryRowContext(ctx, `SELECT revision, payload FROM archive_intake_candidates WHERE volume_id = ?`, candidate.VolumeID).Scan(&revision, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = repository.db.ExecContext(ctx, `INSERT INTO archive_intake_candidates(volume_id, revision, payload) VALUES (?, ?, ?)`, candidate.VolumeID, candidate.Revision, payload)
		return err
	}
	if err != nil {
		return err
	}
	if candidate.Revision < revision {
		return archiveintake.ErrStaleCandidate
	}
	if candidate.Revision == revision {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return ErrRecordConflict
	}
	_, err = repository.db.ExecContext(ctx, `UPDATE archive_intake_candidates SET revision = ?, payload = ? WHERE volume_id = ?`, candidate.Revision, payload, candidate.VolumeID)
	return err
}

func (repository *Repository) Candidate(ctx context.Context, volumeID string) (archiveintake.Candidate, error) {
	var payload []byte
	err := repository.db.QueryRowContext(ctx, `SELECT payload FROM archive_intake_candidates WHERE volume_id = ?`, strings.TrimSpace(volumeID)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return archiveintake.Candidate{}, ErrRecordNotFound
	}
	if err != nil {
		return archiveintake.Candidate{}, err
	}
	var candidate archiveintake.Candidate
	if json.Unmarshal(payload, &candidate) != nil || candidate.Validate() != nil {
		return archiveintake.Candidate{}, ErrRecordConflict
	}
	return candidate, nil
}

func (repository *Repository) ListCandidates(ctx context.Context) ([]archiveintake.Candidate, error) {
	rows, err := repository.db.QueryContext(ctx, `SELECT payload FROM archive_intake_candidates ORDER BY volume_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archiveintake.Candidate
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var candidate archiveintake.Candidate
		if json.Unmarshal(payload, &candidate) != nil || candidate.Validate() != nil {
			return nil, ErrRecordConflict
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func (repository *Repository) SaveSession(ctx context.Context, session archiveintake.Session) error {
	if session.ID == "" || session.Revision == 0 || session.VolumeID == "" || session.WorkspaceDriveID != session.VolumeID ||
		session.PhysicalDeviceID == "" || session.WorkerID == "" ||
		session.StorageID == "" || session.SelectedBy == "" || session.SelectedAt.IsZero() {
		return archiveintake.ErrInvalidSession
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	var existing []byte
	err = repository.db.QueryRowContext(ctx, `SELECT payload FROM archive_intake_sessions WHERE session_id = ?`, session.ID).Scan(&existing)
	if err == nil {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return ErrRecordConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = repository.db.ExecContext(ctx, `INSERT INTO archive_intake_sessions(session_id, volume_id, payload) VALUES (?, ?, ?)`, session.ID, session.VolumeID, payload)
	return err
}

func (repository *Repository) Session(ctx context.Context, sessionID string) (archiveintake.Session, error) {
	var payload []byte
	err := repository.db.QueryRowContext(ctx, `SELECT payload FROM archive_intake_sessions WHERE session_id = ?`, strings.TrimSpace(sessionID)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return archiveintake.Session{}, ErrRecordNotFound
	}
	if err != nil {
		return archiveintake.Session{}, err
	}
	var session archiveintake.Session
	if json.Unmarshal(payload, &session) != nil || repository.normalizeSession(ctx, &session) != nil {
		return archiveintake.Session{}, ErrRecordConflict
	}
	return session, nil
}

func (repository *Repository) ListSessions(ctx context.Context) ([]archiveintake.Session, error) {
	rows, err := repository.db.QueryContext(ctx, `SELECT payload FROM archive_intake_sessions ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []archiveintake.Session
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var session archiveintake.Session
		if err := json.Unmarshal(payload, &session); err != nil {
			return nil, err
		}
		if err := repository.normalizeSession(ctx, &session); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// normalizeSession keeps pre-logical-drive v1 records readable. Their payload
// stored the PhysicalDevice ID in workspace_drive_id and had no separate
// physical_device_id. The candidate identity is immutable at the selected
// revision, so it is the authoritative migration source.
func (repository *Repository) normalizeSession(ctx context.Context, session *archiveintake.Session) error {
	if session == nil || session.ID == "" || session.Revision == 0 || session.CandidateRevision == 0 ||
		session.VolumeID == "" || session.WorkerID == "" || session.StorageID == "" ||
		session.SelectedBy == "" || session.SelectedAt.IsZero() {
		return ErrRecordConflict
	}
	if session.WorkspaceDriveID == session.VolumeID && session.PhysicalDeviceID != "" {
		return nil
	}
	candidate, err := repository.Candidate(ctx, session.VolumeID)
	if err != nil || candidate.Revision != session.CandidateRevision || candidate.WorkerID != session.WorkerID ||
		candidate.StorageID != session.StorageID {
		return ErrRecordConflict
	}
	if session.WorkspaceDriveID != "" && session.WorkspaceDriveID != candidate.PhysicalDeviceID && session.WorkspaceDriveID != session.VolumeID {
		return ErrRecordConflict
	}
	session.WorkspaceDriveID = candidate.VolumeID
	session.PhysicalDeviceID = candidate.PhysicalDeviceID
	return nil
}
