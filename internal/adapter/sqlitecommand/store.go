package sqlitecommand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"videoarchive/internal/application/workercontrol"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type Store struct {
	db *sql.DB
}

var _ command.Store = (*Store)(nil)

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
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
		`PRAGMA busy_timeout = 5000`,
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
		`CREATE TABLE IF NOT EXISTS resource_reservations (
			reservation_id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL,
			step_id TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			snapshot_id INTEGER NOT NULL,
			resource_class TEXT NOT NULL,
			memory_bytes INTEGER NOT NULL,
			scratch_bytes INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			released_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS resource_reservations_worker_snapshot
			ON resource_reservations(worker_id, snapshot_id, released_at)`,
		`CREATE TABLE IF NOT EXISTS resource_reservation_devices (
			physical_device_id TEXT PRIMARY KEY,
			reservation_id TEXT NOT NULL REFERENCES resource_reservations(reservation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS execution_leases (
			step_id TEXT PRIMARY KEY,
			reservation_id TEXT NOT NULL UNIQUE REFERENCES resource_reservations(reservation_id),
			owner_worker_id TEXT NOT NULL,
			acquired_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			heartbeat_at TEXT NOT NULL,
			fencing_token INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS step_fencing_tokens (
			step_id TEXT PRIMARY KEY,
			last_token INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS dispatch_sequence_counter (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			last_sequence INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO dispatch_sequence_counter(singleton, last_sequence) VALUES (1, 0)`,
		`CREATE TABLE IF NOT EXISTS dispatch_job_sequences (
			job_id TEXT PRIMARY KEY,
			last_sequence INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS worker_cancellation_requests (
			cancellation_id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL,
			step_id TEXT NOT NULL,
			command_id TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			reservation_id TEXT NOT NULL REFERENCES resource_reservations(reservation_id),
			fencing_token INTEGER NOT NULL,
			expected_revision INTEGER NOT NULL,
			requested_at TEXT NOT NULL,
			reason TEXT NOT NULL,
			UNIQUE(command_id, step_id)
		)`,
		`CREATE INDEX IF NOT EXISTS worker_cancellation_requests_target
			ON worker_cancellation_requests(worker_id, job_id, step_id, fencing_token)`,
		`CREATE TABLE IF NOT EXISTS worker_cancellation_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			cancellation_id TEXT NOT NULL REFERENCES worker_cancellation_requests(cancellation_id),
			event_type TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			UNIQUE(cancellation_id, event_type)
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

func (store *Store) ListSnapshots() ([]command.Snapshot, error) {
	rows, err := store.db.Query(`SELECT target_id, revision, state FROM aggregate_states ORDER BY target_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []command.Snapshot
	for rows.Next() {
		var snapshot command.Snapshot
		var state []byte
		if err := rows.Scan(&snapshot.TargetID, &snapshot.Revision, &state); err != nil {
			return nil, err
		}
		snapshot.State = json.RawMessage(state)
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (store *Store) ListActiveReservations() ([]dispatch.ResourceReservation, error) {
	rows, err := store.db.Query(`
		SELECT reservation_id, job_id, step_id, worker_id, snapshot_id, resource_class,
			memory_bytes, scratch_bytes, created_at
		FROM resource_reservations
		WHERE released_at IS NULL
		ORDER BY reservation_id
	`)
	if err != nil {
		return nil, err
	}
	type storedReservation struct {
		value     dispatch.ResourceReservation
		createdAt string
	}
	var stored []storedReservation
	for rows.Next() {
		var item storedReservation
		var snapshotID, memoryBytes, scratchBytes int64
		var resourceClass string
		if err := rows.Scan(
			&item.value.ID, &item.value.JobID, &item.value.StepID, &item.value.WorkerID,
			&snapshotID, &resourceClass, &memoryBytes, &scratchBytes, &item.createdAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		if snapshotID <= 0 || memoryBytes < 0 || scratchBytes < 0 {
			rows.Close()
			return nil, dispatch.ErrInvalidReservation
		}
		item.value.SnapshotID = uint64(snapshotID)
		item.value.ResourceClass = vocabulary.ResourceClass(resourceClass)
		item.value.MemoryBytes = uint64(memoryBytes)
		item.value.ScratchBytes = uint64(scratchBytes)
		item.value.CreatedAt, err = time.Parse(time.RFC3339Nano, item.createdAt)
		if err != nil || item.value.Validate() != nil {
			rows.Close()
			return nil, dispatch.ErrInvalidReservation
		}
		stored = append(stored, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]dispatch.ResourceReservation, 0, len(stored))
	for _, item := range stored {
		deviceRows, queryErr := store.db.Query(`
			SELECT physical_device_id FROM resource_reservation_devices
			WHERE reservation_id = ? ORDER BY physical_device_id
		`, item.value.ID)
		if queryErr != nil {
			return nil, queryErr
		}
		for deviceRows.Next() {
			var deviceID string
			if scanErr := deviceRows.Scan(&deviceID); scanErr != nil {
				deviceRows.Close()
				return nil, scanErr
			}
			item.value.PhysicalDeviceIDs = append(item.value.PhysicalDeviceIDs, deviceID)
		}
		if queryErr := deviceRows.Err(); queryErr != nil {
			deviceRows.Close()
			return nil, queryErr
		}
		if queryErr := deviceRows.Close(); queryErr != nil {
			return nil, queryErr
		}
		result = append(result, item.value)
	}
	return result, nil
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
	if err := applyCommit(tx, idempotencyKey, commit); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) NextFencingToken(stepID string) (uint64, error) {
	if stepID == "" {
		return 0, command.ErrInvalidChange
	}
	var token uint64
	err := store.db.QueryRow(`SELECT last_token FROM step_fencing_tokens WHERE step_id = ?`, stepID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return token + 1, nil
}

func (store *Store) NextDispatchSequence() (uint64, error) {
	var sequence uint64
	if err := store.db.QueryRow(`SELECT last_sequence FROM dispatch_sequence_counter WHERE singleton = 1`).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence + 1, nil
}

func (store *Store) CommitDispatch(idempotencyKey string, commit command.DispatchCommit) error {
	if err := commit.Validate(idempotencyKey); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := verifyDispatchSequence(tx, commit.DispatchSequence); err != nil {
		return err
	}
	if err := verifyFencingToken(tx, commit.Reservation.StepID, commit.Lease.FencingToken); err != nil {
		return err
	}
	if err := verifyLeaseAvailable(tx, commit.Reservation.StepID, commit.Reservation.CreatedAt); err != nil {
		return err
	}
	if err := verifyCapacity(tx, commit); err != nil {
		return err
	}
	if err := insertDispatchRecords(tx, commit); err != nil {
		return err
	}
	if err := applyCommit(tx, idempotencyKey, commit.Commit); err != nil {
		return err
	}
	return tx.Commit()
}

func applyCommit(tx *sql.Tx, idempotencyKey string, commit command.Commit) error {

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
		if commit.Record.Envelope.CommandType == job.CancelCommand {
			cancelledState, err := job.Decode(*commit.NextState)
			if err != nil {
				return err
			}
			if err := applyJobCancellation(tx, cancelledState, commit.Record.Envelope); err != nil {
				return err
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
	return nil
}

func applyJobCancellation(tx *sql.Tx, state job.State, envelope command.Envelope) error {
	rows, err := tx.Query(`
		SELECT r.reservation_id, r.step_id, r.worker_id, l.fencing_token
		FROM resource_reservations r
		LEFT JOIN execution_leases l ON l.reservation_id = r.reservation_id
		WHERE r.job_id = ? AND r.released_at IS NULL
		ORDER BY r.reservation_id
	`, state.ID)
	if err != nil {
		return err
	}
	type activeReservation struct {
		reservationID string
		stepID        string
		workerID      string
		fencingToken  sql.NullInt64
	}
	var reservations []activeReservation
	for rows.Next() {
		var reservation activeReservation
		if err := rows.Scan(&reservation.reservationID, &reservation.stepID, &reservation.workerID, &reservation.fencingToken); err != nil {
			rows.Close()
			return err
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, reservation := range reservations {
		step := stateStep(&state, reservation.stepID)
		if step == nil {
			return command.ErrInvalidChange
		}
		switch step.Workflow.State {
		case workflow.Cancelled:
			if err := releaseReservation(tx, reservation.reservationID, step.LastEventAt); err != nil {
				return err
			}
		case workflow.Running, workflow.Verifying, workflow.Paused:
			if !reservation.fencingToken.Valid || reservation.fencingToken.Int64 <= 0 || step.Workflow.ExecutionLease != nil {
				return command.ErrInvalidChange
			}
			cancellationID := envelope.CommandID + ":" + step.Workflow.ID
			if _, err := tx.Exec(`
				INSERT INTO worker_cancellation_requests(
					cancellation_id, job_id, step_id, command_id, worker_id, reservation_id,
					fencing_token, expected_revision, requested_at, reason
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, cancellationID, state.ID, step.Workflow.ID, envelope.CommandID, reservation.workerID,
				reservation.reservationID, reservation.fencingToken.Int64, envelope.ExpectedRevision,
				formatTime(step.LastEventAt), envelope.Reason); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM execution_leases WHERE reservation_id = ?`, reservation.reservationID); err != nil {
				return err
			}
		}
	}
	return nil
}

func releaseReservation(tx *sql.Tx, reservationID string, releasedAt time.Time) error {
	if releasedAt.IsZero() {
		return command.ErrInvalidChange
	}
	if _, err := tx.Exec(`DELETE FROM resource_reservation_devices WHERE reservation_id = ?`, reservationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM execution_leases WHERE reservation_id = ?`, reservationID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE resource_reservations SET released_at = ? WHERE reservation_id = ? AND released_at IS NULL`, formatTime(releasedAt), reservationID)
	return err
}

func verifyDispatchSequence(tx *sql.Tx, sequence uint64) error {
	var last uint64
	if err := tx.QueryRow(`SELECT last_sequence FROM dispatch_sequence_counter WHERE singleton = 1`).Scan(&last); err != nil {
		return err
	}
	if sequence != last+1 {
		return command.ErrRevisionConflict
	}
	return nil
}

func verifyFencingToken(tx *sql.Tx, stepID string, token uint64) error {
	var last uint64
	err := tx.QueryRow(`SELECT last_token FROM step_fencing_tokens WHERE step_id = ?`, stepID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		last = 0
	} else if err != nil {
		return err
	}
	if token != last+1 {
		return dispatch.ErrFencingTokenConflict
	}
	return nil
}

func verifyLeaseAvailable(tx *sql.Tx, stepID string, now time.Time) error {
	var expiresAtRaw string
	err := tx.QueryRow(`SELECT expires_at FROM execution_leases WHERE step_id = ?`, stepID).Scan(&expiresAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtRaw)
	if err != nil {
		return err
	}
	if now.Before(expiresAt) {
		return dispatch.ErrExecutionLeaseHeld
	}
	return nil
}

func verifyCapacity(tx *sql.Tx, commit command.DispatchCommit) error {
	var memoryReserved, scratchReserved uint64
	var slotReservations int
	err := tx.QueryRow(`
		SELECT COALESCE(SUM(memory_bytes), 0), COALESCE(SUM(scratch_bytes), 0),
		       COALESCE(SUM(CASE WHEN resource_class = ? THEN 1 ELSE 0 END), 0)
		FROM resource_reservations
		WHERE worker_id = ? AND released_at IS NULL
	`, string(commit.Reservation.ResourceClass), commit.Reservation.WorkerID).
		Scan(&memoryReserved, &scratchReserved, &slotReservations)
	if err != nil {
		return err
	}
	if memoryReserved > commit.Capacity.AvailableMemoryBytes ||
		commit.Reservation.MemoryBytes > commit.Capacity.AvailableMemoryBytes-memoryReserved ||
		scratchReserved > commit.Capacity.ScratchFreeBytes ||
		commit.Reservation.ScratchBytes > commit.Capacity.ScratchFreeBytes-scratchReserved {
		return dispatch.ErrCapacityUnavailable
	}
	switch commit.Reservation.ResourceClass {
	case vocabulary.ResourceHeavy:
		if commit.Capacity.HeavyRunning+slotReservations >= commit.Capacity.MaxHeavy {
			return dispatch.ErrCapacityUnavailable
		}
	case vocabulary.ResourceLight:
		if commit.Capacity.LightRunning+slotReservations >= commit.Capacity.MaxLight {
			return dispatch.ErrCapacityUnavailable
		}
	}
	return nil
}

func insertDispatchRecords(tx *sql.Tx, commit command.DispatchCommit) error {
	reservation := commit.Reservation
	snapshotID, err := sqliteInteger(reservation.SnapshotID)
	if err != nil {
		return err
	}
	memoryBytes, err := sqliteInteger(reservation.MemoryBytes)
	if err != nil {
		return err
	}
	scratchBytes, err := sqliteInteger(reservation.ScratchBytes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO resource_reservations(
			reservation_id, job_id, step_id, worker_id, snapshot_id, resource_class,
			memory_bytes, scratch_bytes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, reservation.ID, reservation.JobID, reservation.StepID, reservation.WorkerID, snapshotID,
		string(reservation.ResourceClass), memoryBytes, scratchBytes, formatTime(reservation.CreatedAt)); err != nil {
		return dispatch.ErrCapacityUnavailable
	}
	for _, deviceID := range reservation.PhysicalDeviceIDs {
		if _, err := tx.Exec(`
			INSERT INTO resource_reservation_devices(physical_device_id, reservation_id) VALUES (?, ?)
		`, deviceID, reservation.ID); err != nil {
			return dispatch.ErrPhysicalDeviceReserved
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO execution_leases(
			step_id, reservation_id, owner_worker_id, acquired_at, expires_at, heartbeat_at, fencing_token
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(step_id) DO UPDATE SET
			reservation_id = excluded.reservation_id,
			owner_worker_id = excluded.owner_worker_id,
			acquired_at = excluded.acquired_at,
			expires_at = excluded.expires_at,
			heartbeat_at = excluded.heartbeat_at,
			fencing_token = excluded.fencing_token
	`, reservation.StepID, reservation.ID, commit.Lease.OwnerWorkerID, formatTime(commit.Lease.AcquiredAt),
		formatTime(commit.Lease.ExpiresAt), formatTime(commit.Lease.HeartbeatAt), commit.Lease.FencingToken); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO step_fencing_tokens(step_id, last_token) VALUES (?, ?)
		ON CONFLICT(step_id) DO UPDATE SET last_token = excluded.last_token
	`, reservation.StepID, commit.Lease.FencingToken); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE dispatch_sequence_counter SET last_sequence = ? WHERE singleton = 1`, commit.DispatchSequence); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO dispatch_job_sequences(job_id, last_sequence) VALUES (?, ?)
		ON CONFLICT(job_id) DO UPDATE SET last_sequence = excluded.last_sequence
	`, reservation.JobID, commit.DispatchSequence); err != nil {
		return err
	}
	return nil
}

func sqliteInteger(value uint64) (int64, error) {
	if value > math.MaxInt64 {
		return 0, command.ErrInvalidChange
	}
	return int64(value), nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func (store *Store) ClaimWorkerStep(ctx context.Context, workerID string, now time.Time) (workercontrol.Assignment, job.State, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT r.job_id, r.step_id, l.fencing_token, l.expires_at
		FROM resource_reservations r
		JOIN execution_leases l ON l.reservation_id = r.reservation_id
		WHERE r.worker_id = ? AND r.released_at IS NULL
		ORDER BY r.created_at, r.reservation_id
	`, workerID)
	if err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	var jobID, compositeStepID, expiresRaw string
	var fencingToken uint64
	for rows.Next() {
		if err := rows.Scan(&jobID, &compositeStepID, &fencingToken, &expiresRaw); err != nil {
			rows.Close()
			return workercontrol.Assignment{}, job.State{}, err
		}
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresRaw)
		if parseErr != nil {
			rows.Close()
			return workercontrol.Assignment{}, job.State{}, parseErr
		}
		if now.Before(expiresAt) {
			break
		}
		jobID = ""
	}
	if err := rows.Close(); err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	if jobID == "" {
		return workercontrol.Assignment{}, job.State{}, workercontrol.ErrNoWork
	}
	var revision uint64
	var encoded []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision, state FROM aggregate_states WHERE target_id = ?`, jobID).Scan(&revision, &encoded); err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	state, err := job.Decode(command.Snapshot{TargetID: jobID, Revision: revision, State: encoded})
	if err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	step := stateStep(&state, compositeStepID)
	if step == nil || step.Workflow.ExecutionLease == nil || step.Workflow.ExecutionLease.OwnerWorkerID != workerID ||
		step.Workflow.ExecutionLease.FencingToken != fencingToken {
		return workercontrol.Assignment{}, job.State{}, workflow.ErrStaleFencingToken
	}
	if step.ExecutionSpec == nil || step.ExecutionSpec.Validate() != nil || step.ExecutionSpec.JobID != state.ID ||
		step.ExecutionSpec.StepID != step.Workflow.ID || step.ExecutionSpec.TemplateName != state.TemplateName {
		return workercontrol.Assignment{}, job.State{}, workercontrol.ErrInvalidAssignment
	}
	expiresAt, _ := time.Parse(time.RFC3339Nano, expiresRaw)
	assignment := workercontrol.Assignment{
		JobID: state.ID, StepID: step.Workflow.ID, FencingToken: fencingToken, LeaseExpiresAt: expiresAt,
		ResourceClass: step.ResourceClass, RequiredProcessingKinds: append([]vocabulary.ProcessingKind(nil), step.RequiredProcessingKinds...),
		ExecutionSpec: *step.ExecutionSpec,
	}
	if step.Workflow.State == workflow.Running {
		return assignment, state, nil
	}
	if step.Workflow.State != workflow.Reserved || step.Workflow.Transition(workflow.Running) != nil {
		return workercontrol.Assignment{}, job.State{}, workercontrol.ErrNoWork
	}
	step.LastEventAt = now
	state.CurrentStepID = step.Workflow.ID
	state.Revision = revision + 1
	encoded, err = json.Marshal(state)
	if err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	commandID := fmt.Sprintf("worker-claim:%s:%s:%d", state.ID, step.Workflow.ID, fencingToken)
	reason := "Worker claimed reserved Step"
	event := command.Event{
		EventID: commandID + ":event", EventType: "StepStarted", CommandID: commandID,
		Actor: command.Actor{Type: command.WorkerActor, ID: workerID}, RequestedVia: "worker_api",
		TargetID: state.ID, BeforeRevision: revision, AfterRevision: state.Revision, Reason: reason,
		Payload: map[string]any{"step_id": step.Workflow.ID, "fencing_token": fencingToken},
	}
	expected := revision
	if err := applyCommit(tx, "", command.Commit{
		ExpectedRevision: &expected, NextState: &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: commandID, CommandType: "step.start", Actor: event.Actor, RequestedVia: event.RequestedVia,
			TargetID: state.ID, ExpectedRevision: revision, IdempotencyKey: commandID, Reason: reason,
		}, Accepted: true}, Events: []command.Event{event},
	}); err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return workercontrol.Assignment{}, job.State{}, err
	}
	return assignment, state, nil
}

func (store *Store) SettleWorkerStep(ctx context.Context, report workercontrol.Result, now time.Time) (job.State, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return job.State{}, err
	}
	defer tx.Rollback()
	compositeStepID := report.JobID + "/" + report.StepID
	var reservationID, owner string
	var token uint64
	if err := tx.QueryRowContext(ctx, `
		SELECT l.reservation_id, l.owner_worker_id, l.fencing_token
		FROM execution_leases l JOIN resource_reservations r ON r.reservation_id = l.reservation_id
		WHERE l.step_id = ? AND r.released_at IS NULL
	`, compositeStepID).Scan(&reservationID, &owner, &token); errors.Is(err, sql.ErrNoRows) {
		return job.State{}, workflow.ErrStaleFencingToken
	} else if err != nil {
		return job.State{}, err
	}
	if owner != report.WorkerID || token != report.FencingToken {
		return job.State{}, workflow.ErrStaleFencingToken
	}
	var revision uint64
	var encoded []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision, state FROM aggregate_states WHERE target_id = ?`, report.JobID).Scan(&revision, &encoded); err != nil {
		return job.State{}, err
	}
	state, err := job.Decode(command.Snapshot{TargetID: report.JobID, Revision: revision, State: encoded})
	if err != nil {
		return job.State{}, err
	}
	step := stateStep(&state, compositeStepID)
	if step == nil || step.Workflow.AcceptWorkerResult(report.WorkerID, report.FencingToken) != nil || step.Workflow.State != workflow.Running {
		return job.State{}, workflow.ErrStaleFencingToken
	}
	if step.ExecutionSpec == nil || step.ExecutionSpec.Validate() != nil || step.OutputRecord != nil {
		return job.State{}, workercontrol.ErrInvalidAssignment
	}
	eventType := "StepCompleted"
	if report.Success {
		if report.OutputRecord == nil || report.OutputRecord.SpecDigest != step.ExecutionSpec.Digest ||
			report.OutputRecord.FencingToken != report.FencingToken || report.OutputRecord.WorkerID != report.WorkerID {
			return job.State{}, workercontrol.ErrInvalidAssignment
		}
		outputRecord, err := execution.NewStepOutputRecordWithInspections(
			*step.ExecutionSpec, report.FencingToken, report.WorkerID, now,
			report.OutputRecord.Outputs, report.OutputRecord.VerificationReferences, report.OutputRecord.InspectedMedia,
		)
		if err != nil {
			return job.State{}, workercontrol.ErrInvalidAssignment
		}
		if step.Workflow.Transition(workflow.Verifying) != nil || step.Workflow.Transition(workflow.Completed) != nil {
			return job.State{}, workflow.ErrIllegalTransition
		}
		step.OutputRecord = &outputRecord
		step.Progress = 1
		step.WaitTargetStorageID = ""
		if err := materializeDependentExecutionSpecs(&state); err != nil {
			return job.State{}, err
		}
	} else {
		if report.OutputRecord != nil {
			return job.State{}, workercontrol.ErrInvalidAssignment
		}
		deviceWait := report.FailureClass == workflow.FailureResource && report.WaitReason == vocabulary.WaitDevice
		if deviceWait != (strings.TrimSpace(report.WaitTargetStorageID) != "") {
			return job.State{}, workercontrol.ErrInvalidAssignment
		}
		if err := step.Workflow.RecordFailure(workflow.Failure{Class: report.FailureClass, WaitReason: report.WaitReason}); err != nil {
			return job.State{}, err
		}
		step.WaitTargetStorageID = strings.TrimSpace(report.WaitTargetStorageID)
		step.ErrorClass = report.ErrorClass
		eventType = "StepFailed"
		if step.Workflow.State == workflow.Queued || step.Workflow.State == workflow.Waiting {
			eventType = "StepRetryScheduled"
		}
	}
	step.Workflow.ExecutionLease = nil
	step.LogReference = report.LogReference
	step.LastEventAt = now
	activateNextReadyStep(&state, now)
	state.Revision = revision + 1
	encoded, err = json.Marshal(state)
	if err != nil {
		return job.State{}, err
	}
	commandID := fmt.Sprintf("worker-result:%s:%s:%d:%d", state.ID, step.Workflow.ID, report.FencingToken, state.Revision)
	reason := "Worker settled Step execution"
	event := command.Event{
		EventID: commandID + ":event", EventType: eventType, CommandID: commandID,
		Actor: command.Actor{Type: command.WorkerActor, ID: report.WorkerID}, RequestedVia: "worker_api",
		TargetID: state.ID, BeforeRevision: revision, AfterRevision: state.Revision, Reason: reason,
		Payload: map[string]any{
			"step_id": step.Workflow.ID, "fencing_token": report.FencingToken, "success": report.Success,
			"output_ids": stepOutputIDs(step.OutputRecord),
		},
	}
	expected := revision
	if err := applyCommit(tx, "", command.Commit{
		ExpectedRevision: &expected, NextState: &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: commandID, CommandType: "step.settle", Actor: event.Actor, RequestedVia: event.RequestedVia,
			TargetID: state.ID, ExpectedRevision: revision, IdempotencyKey: commandID, Reason: reason,
		}, Accepted: true}, Events: []command.Event{event},
	}); err != nil {
		return job.State{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_reservation_devices WHERE reservation_id = ?`, reservationID); err != nil {
		return job.State{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_leases WHERE reservation_id = ?`, reservationID); err != nil {
		return job.State{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE resource_reservations SET released_at = ? WHERE reservation_id = ? AND released_at IS NULL`, formatTime(now), reservationID); err != nil {
		return job.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return job.State{}, err
	}
	return state, nil
}

func (store *Store) PendingWorkerCancellation(
	ctx context.Context,
	workerID, jobID, stepID string,
	fencingToken uint64,
	now time.Time,
) (workercontrol.Cancellation, error) {
	if workerID == "" || jobID == "" || stepID == "" || fencingToken == 0 {
		return workercontrol.Cancellation{}, workercontrol.ErrNoCancellation
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return workercontrol.Cancellation{}, err
	}
	defer tx.Rollback()
	var cancellation workercontrol.Cancellation
	var requestedAt string
	err = tx.QueryRowContext(ctx, `
		SELECT cancellation_id, worker_id, job_id, step_id, fencing_token, requested_at
		FROM worker_cancellation_requests
		WHERE worker_id = ? AND job_id = ? AND step_id = ? AND fencing_token = ?
	`, workerID, jobID, stepID, fencingToken).Scan(
		&cancellation.ID, &cancellation.WorkerID, &cancellation.JobID, &cancellation.StepID,
		&cancellation.FencingToken, &requestedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return workercontrol.Cancellation{}, workercontrol.ErrNoCancellation
	}
	if err != nil {
		return workercontrol.Cancellation{}, err
	}
	cancellation.RequestedAt, err = time.Parse(time.RFC3339Nano, requestedAt)
	if err != nil {
		return workercontrol.Cancellation{}, err
	}
	var settled int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM worker_cancellation_events
		WHERE cancellation_id = ? AND event_type = 'CancellationSettled'
	`, cancellation.ID).Scan(&settled); err != nil {
		return workercontrol.Cancellation{}, err
	}
	if settled != 0 {
		return workercontrol.Cancellation{}, workercontrol.ErrNoCancellation
	}
	payload, err := json.Marshal(map[string]any{"worker_id": workerID, "fencing_token": fencingToken})
	if err != nil {
		return workercontrol.Cancellation{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO worker_cancellation_events(cancellation_id, event_type, observed_at, payload)
		VALUES (?, 'StopDispatched', ?, ?)
	`, cancellation.ID, formatTime(now), payload); err != nil {
		return workercontrol.Cancellation{}, err
	}
	if err := tx.Commit(); err != nil {
		return workercontrol.Cancellation{}, err
	}
	return cancellation, nil
}

func (store *Store) SettleWorkerCancellation(ctx context.Context, report workercontrol.CancellationResult, now time.Time) (job.State, error) {
	if report.CancellationID == "" || report.WorkerID == "" || report.JobID == "" || report.StepID == "" || report.FencingToken == 0 {
		return job.State{}, workercontrol.ErrNoCancellation
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return job.State{}, err
	}
	defer tx.Rollback()
	var workerID, jobID, stepID, reservationID string
	var fencingToken uint64
	if err := tx.QueryRowContext(ctx, `
		SELECT worker_id, job_id, step_id, reservation_id, fencing_token
		FROM worker_cancellation_requests WHERE cancellation_id = ?
	`, report.CancellationID).Scan(&workerID, &jobID, &stepID, &reservationID, &fencingToken); errors.Is(err, sql.ErrNoRows) {
		return job.State{}, workercontrol.ErrNoCancellation
	} else if err != nil {
		return job.State{}, err
	}
	if workerID != report.WorkerID || jobID != report.JobID || stepID != report.StepID || fencingToken != report.FencingToken {
		return job.State{}, workflow.ErrStaleFencingToken
	}
	var settled int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM worker_cancellation_events
		WHERE cancellation_id = ? AND event_type = 'CancellationSettled'
	`, report.CancellationID).Scan(&settled); err != nil {
		return job.State{}, err
	}
	if settled != 0 {
		return job.State{}, workflow.ErrStaleFencingToken
	}
	if !report.ProcessStopped || !report.CleanupConfirmed || len(report.ExternalEffectReferences) != 0 {
		payload, marshalErr := json.Marshal(map[string]any{
			"process_stopped": report.ProcessStopped, "cleanup_confirmed": report.CleanupConfirmed,
			"external_effect_references": report.ExternalEffectReferences, "log_reference": report.LogReference,
		})
		if marshalErr != nil {
			return job.State{}, marshalErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO worker_cancellation_events(cancellation_id, event_type, observed_at, payload)
			VALUES (?, 'ExternalEffectUncertain', ?, ?)
		`, report.CancellationID, formatTime(now), payload); err != nil {
			return job.State{}, err
		}
		if err := tx.Commit(); err != nil {
			return job.State{}, err
		}
		snapshot, err := store.Load(report.JobID)
		if err != nil {
			return job.State{}, err
		}
		return job.Decode(snapshot)
	}

	var revision uint64
	var encoded []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision, state FROM aggregate_states WHERE target_id = ?`, report.JobID).Scan(&revision, &encoded); err != nil {
		return job.State{}, err
	}
	state, err := job.Decode(command.Snapshot{TargetID: report.JobID, Revision: revision, State: encoded})
	if err != nil {
		return job.State{}, err
	}
	step := stateStep(&state, state.ID+"/"+report.StepID)
	if step == nil || (step.Workflow.State != workflow.Running && step.Workflow.State != workflow.Verifying && step.Workflow.State != workflow.Paused) ||
		step.Workflow.ExecutionLease != nil {
		return job.State{}, workflow.ErrStaleFencingToken
	}
	if err := step.Workflow.Transition(workflow.Cancelled); err != nil {
		return job.State{}, err
	}
	step.LastEventAt = now
	step.LogReference = report.LogReference
	for index := range state.Steps {
		candidate := &state.Steps[index]
		switch candidate.Workflow.State {
		case workflow.Queued, workflow.Waiting, workflow.Reserved:
			if err := candidate.Workflow.Transition(workflow.Cancelled); err != nil {
				return job.State{}, err
			}
			candidate.Workflow.ExecutionLease = nil
			candidate.LastEventAt = now
		}
	}
	state.CurrentStepID = step.Workflow.ID
	state.Revision = revision + 1
	encoded, err = json.Marshal(state)
	if err != nil {
		return job.State{}, err
	}
	commandID := fmt.Sprintf("worker-cancellation:%s:%s:%d:%d", state.ID, step.Workflow.ID, report.FencingToken, state.Revision)
	reason := "Worker confirmed process stop and temporary output cleanup"
	event := command.Event{
		EventID: commandID + ":event", EventType: "StepCancellationSettled", CommandID: commandID,
		Actor: command.Actor{Type: command.WorkerActor, ID: report.WorkerID}, RequestedVia: "worker_api",
		TargetID: state.ID, BeforeRevision: revision, AfterRevision: state.Revision, Reason: reason,
		Payload: map[string]any{"step_id": step.Workflow.ID, "cancellation_id": report.CancellationID, "fencing_token": report.FencingToken},
	}
	expected := revision
	if err := applyCommit(tx, "", command.Commit{
		ExpectedRevision: &expected, NextState: &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: commandID, CommandType: "step.settle_cancellation", Actor: event.Actor, RequestedVia: event.RequestedVia,
			TargetID: state.ID, ExpectedRevision: revision, IdempotencyKey: commandID, Reason: reason,
		}, Accepted: true}, Events: []command.Event{event},
	}); err != nil {
		return job.State{}, err
	}
	if err := releaseReservation(tx, reservationID, now); err != nil {
		return job.State{}, err
	}
	for _, candidate := range state.Steps {
		if candidate.Workflow.State != workflow.Cancelled {
			continue
		}
		var candidateReservation string
		err := tx.QueryRowContext(ctx, `
			SELECT reservation_id FROM resource_reservations
			WHERE job_id = ? AND step_id = ? AND released_at IS NULL
		`, state.ID, state.ID+"/"+candidate.Workflow.ID).Scan(&candidateReservation)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return job.State{}, err
		}
		if err := releaseReservation(tx, candidateReservation, now); err != nil {
			return job.State{}, err
		}
	}
	payload, err := json.Marshal(map[string]any{
		"process_stopped": true, "cleanup_confirmed": true, "log_reference": report.LogReference,
	})
	if err != nil {
		return job.State{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO worker_cancellation_events(cancellation_id, event_type, observed_at, payload)
		VALUES (?, 'CancellationSettled', ?, ?)
	`, report.CancellationID, formatTime(now), payload); err != nil {
		return job.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return job.State{}, err
	}
	return state, nil
}

func stateStep(state *job.State, compositeStepID string) *job.Step {
	for index := range state.Steps {
		if state.ID+"/"+state.Steps[index].Workflow.ID == compositeStepID {
			return &state.Steps[index]
		}
	}
	return nil
}

func stepOutputIDs(record *execution.StepOutputRecord) []string {
	if record == nil {
		return nil
	}
	ids := make([]string, 0, len(record.Outputs))
	for _, output := range record.Outputs {
		ids = append(ids, output.ID)
	}
	return ids
}

func materializeDependentExecutionSpecs(state *job.State) error {
	if state == nil || !(strings.HasPrefix(state.TemplateName, "archive-intake-") || state.TemplateName == "archive-transcription") {
		return nil
	}
	workflowSteps := make(map[string]*workflow.Step, len(state.Steps))
	completed := make([]execution.CompletedStep, 0, len(state.Steps))
	for index := range state.Steps {
		step := &state.Steps[index]
		workflowSteps[step.Workflow.ID] = &step.Workflow
		if step.Workflow.State == workflow.Completed && step.ExecutionSpec != nil && step.OutputRecord != nil {
			completed = append(completed, execution.CompletedStep{
				StepID: step.Workflow.ID, Spec: *step.ExecutionSpec, OutputRecord: *step.OutputRecord,
			})
		}
	}
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.ExecutorKind != vocabulary.ExecutorWorker || step.Workflow.State != workflow.Queued || step.ExecutionSpec != nil ||
			!step.Workflow.Ready(workflowSteps) {
			continue
		}
		spec, err := execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{
			JobID: state.ID, TemplateName: state.TemplateName, StepID: step.Workflow.ID, Completed: completed,
		})
		if err != nil {
			return err
		}
		step.ExecutionSpec = &spec
	}
	return nil
}

func activateNextReadyStep(state *job.State, now time.Time) {
	steps := make(map[string]*workflow.Step, len(state.Steps))
	for index := range state.Steps {
		steps[state.Steps[index].Workflow.ID] = &state.Steps[index].Workflow
	}
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.Workflow.State == workflow.Queued && step.Workflow.Ready(steps) {
			if step.ReadyAt.IsZero() {
				step.ReadyAt = now
			}
			state.CurrentStepID = step.Workflow.ID
			return
		}
	}
}

func (store *Store) RecoverExpiredReservations(ctx context.Context, now time.Time) ([]job.State, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT r.job_id, r.step_id, r.reservation_id
		FROM resource_reservations r JOIN execution_leases l ON l.reservation_id = r.reservation_id
		WHERE r.released_at IS NULL AND l.expires_at <= ?
		ORDER BY r.created_at, r.reservation_id
	`, formatTime(now))
	if err != nil {
		return nil, err
	}
	type expired struct{ jobID, stepID, reservationID string }
	var values []expired
	for rows.Next() {
		var value expired
		if err := rows.Scan(&value.jobID, &value.stepID, &value.reservationID); err != nil {
			rows.Close()
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var recovered []job.State
	for _, value := range values {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		var revision uint64
		var encoded []byte
		if err := tx.QueryRowContext(ctx, `SELECT revision, state FROM aggregate_states WHERE target_id = ?`, value.jobID).Scan(&revision, &encoded); err != nil {
			tx.Rollback()
			return nil, err
		}
		state, err := job.Decode(command.Snapshot{TargetID: value.jobID, Revision: revision, State: encoded})
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		step := stateStep(&state, value.stepID)
		if step == nil {
			tx.Rollback()
			continue
		}
		eventType := "StepReservationExpired"
		commandType := "step.recover_reservation"
		commandIDPrefix := "recover-reservation"
		reason := "expired unstarted Worker reservation"
		switch step.Workflow.State {
		case workflow.Reserved:
			if err := step.Workflow.Transition(workflow.Queued); err != nil {
				tx.Rollback()
				return nil, err
			}
		case workflow.Running:
			if !step.Workflow.RecoverExpiredLease(now) {
				tx.Rollback()
				continue
			}
			eventType = "StepExecutionLeaseExpired"
			commandType = "step.recover_execution_lease"
			commandIDPrefix = "recover-execution-lease"
			reason = "expired running Worker execution lease"
		default:
			tx.Rollback()
			continue
		}
		step.WorkerID = ""
		step.LastEventAt = now
		state.Revision = revision + 1
		encoded, err = json.Marshal(state)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		commandID := fmt.Sprintf("%s:%s:%s:%d", commandIDPrefix, state.ID, step.Workflow.ID, state.Revision)
		event := command.Event{
			EventID: commandID + ":event", EventType: eventType, CommandID: commandID,
			Actor: command.Actor{Type: command.SystemActor, ID: "worker-recovery"}, RequestedVia: "worker_recovery",
			TargetID: state.ID, BeforeRevision: revision, AfterRevision: state.Revision, Reason: reason,
			Payload: map[string]any{"step_id": step.Workflow.ID},
		}
		expected := revision
		if err := applyCommit(tx, "", command.Commit{
			ExpectedRevision: &expected, NextState: &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded},
			Record: command.CommandRecord{Envelope: command.Envelope{
				CommandID: commandID, CommandType: commandType, Actor: event.Actor, RequestedVia: event.RequestedVia,
				TargetID: state.ID, ExpectedRevision: revision, IdempotencyKey: commandID, Reason: reason,
			}, Accepted: true}, Events: []command.Event{event},
		}); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_reservation_devices WHERE reservation_id = ?`, value.reservationID); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM execution_leases WHERE reservation_id = ?`, value.reservationID); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE resource_reservations SET released_at = ? WHERE reservation_id = ?`, formatTime(now), value.reservationID); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		recovered = append(recovered, state)
	}
	return recovered, nil
}
