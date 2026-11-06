package sqlitearchivist

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"videoarchive/internal/adapter/sqlitebase"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

type Store struct {
	db      *sql.DB
	release func() error
}

var _ archivistcontrol.Store = (*Store)(nil)

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

func (store *Store) Close() error { return store.release() }

func (store *Store) migrate(ctx context.Context) error {
	_, err := store.db.ExecContext(ctx, `
		PRAGMA busy_timeout = 5000;
		PRAGMA journal_mode = WAL;
		CREATE TABLE IF NOT EXISTS archivist_sessions (
			session_id TEXT PRIMARY KEY,
			archivist_host_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			payload BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS archivist_turns (
			turn_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			FOREIGN KEY(session_id) REFERENCES archivist_sessions(session_id)
		);
		CREATE INDEX IF NOT EXISTS archivist_turns_claim_order
			ON archivist_turns(state, created_at, turn_id);
		CREATE TABLE IF NOT EXISTS archivist_turn_entries (
			turn_id TEXT NOT NULL,
			cursor INTEGER NOT NULL,
			kind TEXT NOT NULL,
			created_at TEXT NOT NULL,
			summary TEXT NOT NULL,
			code TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(turn_id, cursor),
			FOREIGN KEY(turn_id) REFERENCES archivist_turns(turn_id)
		);
		CREATE TABLE IF NOT EXISTS archivist_tool_calls (
			tool_call_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			turn_id TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			FOREIGN KEY(session_id) REFERENCES archivist_sessions(session_id),
			FOREIGN KEY(turn_id) REFERENCES archivist_turns(turn_id)
		);
		CREATE TABLE IF NOT EXISTS archivist_action_cards (
			action_card_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			turn_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			FOREIGN KEY(session_id) REFERENCES archivist_sessions(session_id),
			FOREIGN KEY(turn_id) REFERENCES archivist_turns(turn_id)
		);
		CREATE TABLE IF NOT EXISTS archivist_action_card_rejections (
			action_card_id TEXT PRIMARY KEY,
			actor_id TEXT NOT NULL,
			reason TEXT NOT NULL,
			rejected_at TEXT NOT NULL,
			FOREIGN KEY(action_card_id) REFERENCES archivist_action_cards(action_card_id)
		);
		CREATE TABLE IF NOT EXISTS archivist_archive_run_bindings (
			archive_run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			turn_id TEXT NOT NULL UNIQUE,
			payload BLOB NOT NULL,
			FOREIGN KEY(session_id) REFERENCES archivist_sessions(session_id),
			FOREIGN KEY(turn_id) REFERENCES archivist_turns(turn_id)
		);
	`)
	if err != nil {
		return fmt.Errorf("initialize sqlite archivist runtime store: %w", err)
	}
	return nil
}

func (store *Store) PutToolCall(ctx context.Context, record archivistcontrol.ToolCallRecord) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingSessionID, existingTurnID string
	var existingPayload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT session_id, turn_id, payload FROM archivist_tool_calls WHERE tool_call_id = ?
	`, record.Call.ID).Scan(&existingSessionID, &existingTurnID, &existingPayload)
	if err == nil {
		var existing archivistcontrol.ToolCallProjection
		if json.Unmarshal(existingPayload, &existing) != nil || existingSessionID != record.SessionID || existingTurnID != record.TurnID ||
			existing.ID != record.Call.ID || existing.ToolName != record.Call.ToolName ||
			(existing.TargetID != "" && record.Call.TargetID != "" && existing.TargetID != record.Call.TargetID) {
			return command.ErrIdempotencyConflict
		}
		if (existing.State == "COMPLETED" || existing.State == "ACTION_CARD") && record.Call.State == "RUNNING" {
			return nil
		}
		if record.Call.TargetID == "" {
			record.Call.TargetID = existing.TargetID
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	payload, err := json.Marshal(record.Call)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO archivist_tool_calls(tool_call_id, session_id, turn_id, updated_at, payload)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tool_call_id) DO UPDATE SET updated_at = excluded.updated_at, payload = excluded.payload
	`, record.Call.ID, record.SessionID, record.TurnID, formatTime(record.Call.UpdatedAt), payload)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) ListToolCalls(ctx context.Context, turnID string) ([]archivistcontrol.ToolCallProjection, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM archivist_tool_calls WHERE turn_id = ? ORDER BY updated_at, tool_call_id`, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivistcontrol.ToolCallProjection
	for rows.Next() {
		var payload []byte
		var call archivistcontrol.ToolCallProjection
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &call); err != nil {
			return nil, err
		}
		result = append(result, call)
	}
	return result, rows.Err()
}

func (store *Store) ListTurnEntries(ctx context.Context, turnID string) ([]archivistruntime.TurnEntry, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT cursor, kind, summary, code, created_at
		FROM archivist_turn_entries WHERE turn_id = ? ORDER BY cursor
	`, strings.TrimSpace(turnID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivistruntime.TurnEntry
	for rows.Next() {
		var entry archivistruntime.TurnEntry
		var createdAt string
		if err := rows.Scan(&entry.Cursor, &entry.Kind, &entry.Summary, &entry.Code, &createdAt); err != nil {
			return nil, err
		}
		entry.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (store *Store) PutActionCard(ctx context.Context, record archivistcontrol.ActionCardRecord) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingSessionID, existingTurnID string
	var existingPayload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT session_id, turn_id, payload FROM archivist_action_cards WHERE action_card_id = ?
	`, record.Card.ID).Scan(&existingSessionID, &existingTurnID, &existingPayload)
	if err == nil {
		var existing archivistcontrol.ActionCardProjection
		if json.Unmarshal(existingPayload, &existing) != nil || existingSessionID != record.SessionID || existingTurnID != record.Card.ArchivistTurnID ||
			!sameActionCardProposal(existing, record.Card) {
			return command.ErrIdempotencyConflict
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	payload, err := json.Marshal(record.Card)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO archivist_action_cards(action_card_id, session_id, turn_id, payload) VALUES (?, ?, ?, ?)
	`, record.Card.ID, record.SessionID, record.Card.ArchivistTurnID, payload)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func sameActionCardProposal(left, right archivistcontrol.ActionCardProjection) bool {
	left.State = ""
	right.State = ""
	left.RejectedBy, right.RejectedBy = "", ""
	left.RejectedAt, right.RejectedAt = time.Time{}, time.Time{}
	left.RejectionReason, right.RejectionReason = "", ""
	leftPayload, leftErr := json.Marshal(left)
	rightPayload, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func (store *Store) ListActionCards(ctx context.Context, sessionID string) ([]archivistcontrol.ActionCardProjection, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM archivist_action_cards WHERE session_id = ? ORDER BY action_card_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivistcontrol.ActionCardProjection
	for rows.Next() {
		var payload []byte
		var card archivistcontrol.ActionCardProjection
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &card); err != nil {
			return nil, err
		}
		result = append(result, card)
	}
	return result, rows.Err()
}

func (store *Store) ActionCard(ctx context.Context, cardID string) (archivistcontrol.ActionCardRecord, error) {
	var sessionID string
	var payload []byte
	if err := store.db.QueryRowContext(ctx, `SELECT session_id, payload FROM archivist_action_cards WHERE action_card_id = ?`, cardID).Scan(&sessionID, &payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ActionCardRecord{}, archivistcontrol.ErrInvalidRequest
	} else if err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	var card archivistcontrol.ActionCardProjection
	if err := json.Unmarshal(payload, &card); err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	return archivistcontrol.ActionCardRecord{SessionID: sessionID, Card: card}, nil
}

func (store *Store) ClaimActionCard(ctx context.Context, cardID string) (archivistcontrol.ActionCardRecord, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	defer tx.Rollback()
	var sessionID string
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT session_id, payload FROM archivist_action_cards WHERE action_card_id = ?`, cardID).Scan(&sessionID, &payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ActionCardRecord{}, archivistcontrol.ErrInvalidRequest
	} else if err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	var card archivistcontrol.ActionCardProjection
	if json.Unmarshal(payload, &card) != nil || card.State != "PENDING" {
		return archivistcontrol.ActionCardRecord{}, archivistcontrol.ErrInvalidRequest
	}
	original := append([]byte(nil), payload...)
	card.State = "EXECUTING"
	payload, err = json.Marshal(card)
	if err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE archivist_action_cards SET payload = ? WHERE action_card_id = ? AND payload = ?
	`, payload, cardID, original)
	if err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return archivistcontrol.ActionCardRecord{}, archivistcontrol.ErrInvalidRequest
	}
	if err := tx.Commit(); err != nil {
		return archivistcontrol.ActionCardRecord{}, err
	}
	return archivistcontrol.ActionCardRecord{SessionID: sessionID, Card: card}, nil
}

func (store *Store) SetActionCardState(ctx context.Context, cardID, state string) error {
	state = strings.TrimSpace(state)
	if state != "COMPLETED" {
		return archivistcontrol.ErrInvalidRequest
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID string
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT session_id, payload FROM archivist_action_cards WHERE action_card_id = ?`, cardID).Scan(&sessionID, &payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ErrInvalidRequest
	} else if err != nil {
		return err
	}
	var card archivistcontrol.ActionCardProjection
	if json.Unmarshal(payload, &card) != nil || card.State != "EXECUTING" {
		return archivistcontrol.ErrInvalidRequest
	}
	record := archivistcontrol.ActionCardRecord{SessionID: sessionID, Card: card}
	record.Card.State = state
	payload, err = json.Marshal(record.Card)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE archivist_action_cards SET payload = ? WHERE action_card_id = ?`, payload, cardID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return archivistcontrol.ErrInvalidRequest
	}
	return tx.Commit()
}

func (store *Store) ReleaseActionCard(ctx context.Context, cardID string) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM archivist_action_cards WHERE action_card_id = ?`, cardID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ErrInvalidRequest
	} else if err != nil {
		return err
	}
	var card archivistcontrol.ActionCardProjection
	if json.Unmarshal(payload, &card) != nil || card.State != "EXECUTING" {
		return archivistcontrol.ErrInvalidRequest
	}
	card.State = "PENDING"
	updatedPayload, err := json.Marshal(card)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE archivist_action_cards SET payload = ? WHERE action_card_id = ? AND payload = ?
	`, updatedPayload, cardID, payload)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return archivistcontrol.ErrInvalidRequest
	}
	return tx.Commit()
}

func (store *Store) RejectActionCard(ctx context.Context, rejection archivistcontrol.ActionCardRejection) error {
	if strings.TrimSpace(rejection.ActionCardID) == "" || strings.TrimSpace(rejection.ActorID) == "" ||
		strings.TrimSpace(rejection.Reason) == "" || rejection.RejectedAt.IsZero() {
		return archivistcontrol.ErrInvalidRequest
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM archivist_action_cards WHERE action_card_id = ?`, rejection.ActionCardID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ErrInvalidRequest
	} else if err != nil {
		return err
	}
	var card archivistcontrol.ActionCardProjection
	if json.Unmarshal(payload, &card) != nil || card.State != "PENDING" {
		return archivistcontrol.ErrInvalidRequest
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO archivist_action_card_rejections(action_card_id, actor_id, reason, rejected_at)
		VALUES (?, ?, ?, ?)
	`, rejection.ActionCardID, rejection.ActorID, rejection.Reason, formatTime(rejection.RejectedAt)); err != nil {
		return err
	}
	card.State = "REJECTED"
	card.RejectedBy = rejection.ActorID
	card.RejectedAt = rejection.RejectedAt.UTC()
	card.RejectionReason = rejection.Reason
	payload, err = json.Marshal(card)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE archivist_action_cards SET payload = ? WHERE action_card_id = ?`, payload, rejection.ActionCardID); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) CreateSession(ctx context.Context, record archivistcontrol.SessionRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO archivist_sessions(session_id, archivist_host_id, created_at, payload) VALUES (?, ?, ?, ?)
	`, record.Session.ID, record.Session.ArchivistHostID, formatTime(record.CreatedAt), payload)
	return err
}

func (store *Store) Session(ctx context.Context, sessionID string) (archivistcontrol.SessionRecord, error) {
	row := store.db.QueryRowContext(ctx, `SELECT payload FROM archivist_sessions WHERE session_id = ?`, sessionID)
	var payload []byte
	if err := row.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.SessionRecord{}, archivistcontrol.ErrSessionNotFound
	} else if err != nil {
		return archivistcontrol.SessionRecord{}, err
	}
	return decodeSession(payload)
}

func (store *Store) ListSessions(ctx context.Context) ([]archivistcontrol.SessionRecord, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT payload FROM archivist_sessions ORDER BY created_at DESC, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivistcontrol.SessionRecord
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		record, err := decodeSession(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) CreateTurn(ctx context.Context, record archivistcontrol.TurnRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO archivist_turns(turn_id, session_id, state, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?)
	`, record.Turn.ID, record.Turn.SessionID, record.Turn.State, formatTime(record.CreatedAt), formatTime(record.UpdatedAt), payload)
	return err
}

func (store *Store) ListTurns(ctx context.Context, sessionID string) ([]archivistcontrol.TurnRecord, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT payload FROM archivist_turns WHERE session_id = ? ORDER BY created_at, turn_id
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivistcontrol.TurnRecord
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		record, err := decodeTurn(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) Turn(ctx context.Context, turnID string) (archivistcontrol.TurnRecord, error) {
	var payload []byte
	if err := store.db.QueryRowContext(ctx, `SELECT payload FROM archivist_turns WHERE turn_id = ?`, turnID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.TurnRecord{}, archivistcontrol.ErrTurnNotFound
	} else if err != nil {
		return archivistcontrol.TurnRecord{}, err
	}
	return decodeTurn(payload)
}

func (store *Store) BindArchiveRun(ctx context.Context, binding archivistcontrol.ArchiveRunBinding) error {
	normalized, err := archivistruntime.NewDriveSelection(binding.Selection)
	if err != nil || binding.SessionID == "" || binding.TurnID == "" || normalized.ArchiveRunID == "" || normalized.ArchiveRunRevision == 0 {
		return archivistcontrol.ErrInvalidRequest
	}
	binding.Selection = normalized
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM archivist_archive_run_bindings WHERE archive_run_id = ?`, normalized.ArchiveRunID).Scan(&existing)
	if err == nil {
		if !bytes.Equal(existing, payload) {
			return command.ErrIdempotencyConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var turnPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM archivist_turns WHERE turn_id = ? AND session_id = ?`, binding.TurnID, binding.SessionID).Scan(&turnPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return archivistcontrol.ErrTurnNotFound
		}
		return err
	}
	turn, err := decodeTurn(turnPayload)
	if err != nil || turn.Turn.DriveSelection == nil {
		return archivistcontrol.ErrInvalidRequest
	}
	turn.Turn.DriveSelection = &normalized
	workContext, err := archivistruntime.NewArchiveRunWorkContext(normalized.ArchiveRunID, binding.TurnID, archivistruntime.WorkArchiveRunStarted)
	if err != nil {
		return archivistcontrol.ErrInvalidRequest
	}
	turn.Turn, err = archivistruntime.AttachArchiveRunWorkContext(turn.Turn, workContext)
	if err != nil {
		return archivistcontrol.ErrInvalidRequest
	}
	updatedTurn, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE archivist_turns SET payload = ? WHERE turn_id = ?`, updatedTurn, binding.TurnID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO archivist_archive_run_bindings(archive_run_id, session_id, turn_id, payload) VALUES (?, ?, ?, ?)
	`, normalized.ArchiveRunID, binding.SessionID, binding.TurnID, payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) ArchiveRunBinding(ctx context.Context, archiveRunID string) (archivistcontrol.ArchiveRunBinding, error) {
	var payload []byte
	if err := store.db.QueryRowContext(ctx, `SELECT payload FROM archivist_archive_run_bindings WHERE archive_run_id = ?`, strings.TrimSpace(archiveRunID)).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.ArchiveRunBinding{}, archivistcontrol.ErrArchiveRunNotBound
	} else if err != nil {
		return archivistcontrol.ArchiveRunBinding{}, err
	}
	var binding archivistcontrol.ArchiveRunBinding
	if json.Unmarshal(payload, &binding) != nil || binding.Selection.ArchiveRunID != strings.TrimSpace(archiveRunID) {
		return archivistcontrol.ArchiveRunBinding{}, command.ErrInvalidChange
	}
	return binding, nil
}

func (store *Store) ClaimTurn(ctx context.Context, archivistHostID string, now time.Time, ttl time.Duration) (archivistcontrol.ClaimedTurn, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT t.payload, s.payload
		FROM archivist_turns t JOIN archivist_sessions s ON s.session_id = t.session_id
		WHERE s.archivist_host_id = ? AND t.state IN (?, ?, ?, ?, ?)
		ORDER BY t.created_at, t.turn_id
	`, archivistHostID, vocabulary.ArchivistTurnQueued, vocabulary.ArchivistTurnLeased, vocabulary.ArchivistTurnSubmitted,
		vocabulary.ArchivistTurnRunning, vocabulary.ArchivistTurnWaiting)
	if err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	var selectedTurn archivistcontrol.TurnRecord
	var selectedSession archivistcontrol.SessionRecord
	for rows.Next() {
		var turnPayload, sessionPayload []byte
		if err := rows.Scan(&turnPayload, &sessionPayload); err != nil {
			rows.Close()
			return archivistcontrol.ClaimedTurn{}, err
		}
		candidate, err := decodeTurn(turnPayload)
		if err != nil {
			rows.Close()
			return archivistcontrol.ClaimedTurn{}, err
		}
		if candidate.Turn.Lease != nil && now.Before(candidate.Turn.Lease.ExpiresAt) {
			continue
		}
		selectedTurn = candidate
		selectedSession, err = decodeSession(sessionPayload)
		if err != nil {
			rows.Close()
			return archivistcontrol.ClaimedTurn{}, err
		}
		break
	}
	if err := rows.Close(); err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	if selectedTurn.Turn.ID == "" {
		return archivistcontrol.ClaimedTurn{}, archivistcontrol.ErrTurnNotFound
	}
	claimed, lease, err := archivistruntime.ClaimTurn(selectedTurn.Turn, archivistHostID, now, ttl)
	if err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	selectedTurn.Turn = claimed
	selectedTurn.UpdatedAt = now
	if err := updateTurn(ctx, tx, selectedTurn); err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return archivistcontrol.ClaimedTurn{}, err
	}
	return archivistcontrol.ClaimedTurn{Session: selectedSession.Session, Turn: claimed, Prompt: selectedTurn.Prompt, Lease: lease}, nil
}

func (store *Store) PrepareDelivery(ctx context.Context, turnID, archivistHostID string, fencingToken uint64, now time.Time, requestID string) (archivistcontrol.TurnRecord, bool, error) {
	return store.mutateTurn(ctx, turnID, now, func(turn archivistruntime.Turn) (archivistruntime.Turn, bool, error) {
		return archivistruntime.PrepareTurnDelivery(turn, archivistHostID, fencingToken, now, requestID)
	})
}

func (store *Store) ApplyRuntimeEvent(ctx context.Context, turnID, archivistHostID string, fencingToken uint64, now time.Time, event archivistruntime.RuntimeEvent) (archivistcontrol.TurnRecord, bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM archivist_turns WHERE turn_id = ?`, turnID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.TurnRecord{}, false, archivistcontrol.ErrTurnNotFound
	} else if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	record, err := decodeTurn(payload)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	turn, changed, err := archivistruntime.ReduceRuntimeEvent(record.Turn, archivistHostID, fencingToken, now, event)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	if !changed {
		return record, false, nil
	}
	entry, hasEntry, err := archivistruntime.TurnEntryFromRuntimeEvent(event, now)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	if hasEntry {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archivist_turn_entries(turn_id, cursor, kind, created_at, summary, code)
			VALUES (?, ?, ?, ?, ?, ?)
		`, turnID, entry.Cursor, entry.Kind, formatTime(entry.CreatedAt), entry.Summary, entry.Code); err != nil {
			return archivistcontrol.TurnRecord{}, false, err
		}
	}
	record.Turn = turn
	record.UpdatedAt = now
	if err := updateTurn(ctx, tx, record); err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	return record, true, nil
}

func (store *Store) RenewTurnLease(ctx context.Context, turnID, archivistHostID string, fencingToken uint64, now time.Time, ttl time.Duration) (archivistcontrol.TurnRecord, error) {
	record, _, err := store.mutateTurn(ctx, turnID, now, func(turn archivistruntime.Turn) (archivistruntime.Turn, bool, error) {
		renewed, renewErr := archivistruntime.RenewTurnLease(turn, archivistHostID, fencingToken, now, ttl)
		return renewed, renewErr == nil, renewErr
	})
	return record, err
}

func (store *Store) mutateTurn(ctx context.Context, turnID string, now time.Time, mutate func(archivistruntime.Turn) (archivistruntime.Turn, bool, error)) (archivistcontrol.TurnRecord, bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	defer tx.Rollback()
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM archivist_turns WHERE turn_id = ?`, turnID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return archivistcontrol.TurnRecord{}, false, archivistcontrol.ErrTurnNotFound
	} else if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	record, err := decodeTurn(payload)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	turn, changed, err := mutate(record.Turn)
	if err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	if !changed {
		return record, false, nil
	}
	record.Turn = turn
	record.UpdatedAt = now
	if err := updateTurn(ctx, tx, record); err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return archivistcontrol.TurnRecord{}, false, err
	}
	return record, true, nil
}

func updateTurn(ctx context.Context, tx *sql.Tx, record archivistcontrol.TurnRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE archivist_turns SET state = ?, updated_at = ?, payload = ? WHERE turn_id = ?
	`, record.Turn.State, formatTime(record.UpdatedAt), payload, record.Turn.ID)
	return err
}

func decodeSession(payload []byte) (archivistcontrol.SessionRecord, error) {
	var record archivistcontrol.SessionRecord
	err := json.Unmarshal(payload, &record)
	return record, err
}

func decodeTurn(payload []byte) (archivistcontrol.TurnRecord, error) {
	var record archivistcontrol.TurnRecord
	err := json.Unmarshal(payload, &record)
	return record, err
}

func formatTime(value time.Time) string { return sqlitebase.FormatTime(value) }
