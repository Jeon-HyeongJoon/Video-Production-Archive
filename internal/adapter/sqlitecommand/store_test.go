package sqlitecommand

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: PER-001
func TestSQLiteCommitRollsBackEveryUnitOfWorkRecord(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	before := command.Snapshot{TargetID: "JOB-1", Revision: 3, State: json.RawMessage(`{"status":"before"}`)}
	if err := store.Seed(ctx, before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_domain_event_insert
		BEFORE INSERT ON domain_events
		BEGIN
			SELECT RAISE(ABORT, 'injected event insert failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	expectedRevision := uint64(3)
	event := command.Event{
		EventID: "EVT-1", EventType: "RenditionPublished", CommandID: "CMD-1",
		Actor: command.Actor{Type: command.HumanActor, ID: "operator-1"}, RequestedVia: "archive_workspace",
		TargetID: "JOB-1", BeforeRevision: 3, AfterRevision: 4, Reason: "create proxy",
	}
	commit := command.Commit{
		ExpectedRevision: &expectedRevision,
		NextState:        &command.Snapshot{TargetID: "JOB-1", Revision: 4, State: json.RawMessage(`{"status":"after"}`)},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: "CMD-1", CommandType: "render.create", Actor: command.Actor{Type: command.HumanActor, ID: "operator-1"},
			RequestedVia: "archive_workspace", TargetID: "JOB-1", ExpectedRevision: 3,
			IdempotencyKey: "JOB-1:render:v1", Reason: "create proxy",
		}, Accepted: true},
		Events:  []command.Event{event},
		Receipt: &command.Receipt{Digest: "digest-1", Result: command.Result{CommandID: "CMD-1", TargetID: "JOB-1", Revision: 4, Events: []command.Event{event}}},
	}
	if err := store.Commit("JOB-1:render:v1", commit); err == nil {
		t.Fatal("injected SQLite failure did not abort commit")
	}

	after, err := store.Load("JOB-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || string(after.State) != string(before.State) {
		t.Fatalf("state partially committed: before=%+v after=%+v", before, after)
	}
	for _, table := range []string{"command_records", "domain_events", "idempotency_receipts"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s contains %d partial records", table, count)
		}
	}
}

// Requirement: DSP-003
func TestSQLiteDispatchAtomicallyCommitsOneReservationStateLeaseAndFencingToken(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/dispatch.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, jobID := range []string{"JOB-FAIL", "JOB-A", "JOB-B"} {
		if err := store.Seed(ctx, command.Snapshot{TargetID: jobID, Revision: 1, State: json.RawMessage(`{"step_state":"QUEUED"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_selected_dispatch
		BEFORE INSERT ON execution_leases
		WHEN NEW.reservation_id = 'RES-FAIL'
		BEGIN
			SELECT RAISE(ABORT, 'injected dispatch failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	failedCommit := dispatchCommit("JOB-FAIL", "STEP-FAIL", "RES-FAIL", 77, 1, now)
	if err := store.CommitDispatch("JOB-FAIL:dispatch:1", failedCommit); err == nil {
		t.Fatal("injected dispatch failure was accepted")
	}
	assertDispatchAbsent(t, store, "JOB-FAIL", "STEP-FAIL", "RES-FAIL", 1)
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_selected_dispatch`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		jobID string
		err   error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, jobID := range []string{"JOB-A", "JOB-B"} {
		jobID := jobID
		wait.Add(1)
		go func() {
			defer wait.Done()
			stepID := "STEP-" + jobID[len("JOB-"):]
			reservationID := "RES-" + jobID[len("JOB-"):]
			results <- result{jobID: jobID, err: store.CommitDispatch(jobID+":dispatch:1", dispatchCommit(jobID, stepID, reservationID, 77, 1, now))}
		}()
	}
	wait.Wait()
	close(results)
	var winner, loser string
	for result := range results {
		if result.err == nil {
			winner = result.jobID
		} else {
			loser = result.jobID
		}
	}
	if winner == "" || loser == "" {
		t.Fatalf("same last slot did not produce one winner and one loser: winner=%q loser=%q", winner, loser)
	}
	winnerSuffix := winner[len("JOB-"):]
	loserSuffix := loser[len("JOB-"):]
	assertDispatchPresent(t, store, winner, "STEP-"+winnerSuffix, "RES-"+winnerSuffix, 2)
	assertDispatchAbsent(t, store, loser, "STEP-"+loserSuffix, "RES-"+loserSuffix, 1)

	retry := dispatchCommit(loser, "STEP-"+loserSuffix, "RES-"+loserSuffix+"-RETRY", 77, 2, now.Add(time.Second))
	if err := store.CommitDispatch(loser+":dispatch:2", retry); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("confirmed reservation was not deducted from the same snapshot: %v", err)
	}
	assertDispatchAbsent(t, store, loser, "STEP-"+loserSuffix, "RES-"+loserSuffix+"-RETRY", 1)
}

// Requirement: WFL-004
func TestExpiredRunningLeaseReturnsStepToQueue(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/recovery.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	jobID := "JOB-RUNNING-RECOVERY"
	stepID := "inspect"
	compositeStepID := jobID + "/" + stepID
	queued := job.State{
		ID: jobID, Revision: 1, TemplateName: "archive-intake-inspection", TargetTitle: "selected folder",
		ControlMode: workflow.AssistedMode, CurrentStepID: stepID,
		Steps: []job.Step{{Workflow: workflow.Step{ID: stepID, State: workflow.Queued, Attempt: 1, MaxAttempts: 3}}},
	}
	queuedBytes, err := json.Marshal(queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Seed(ctx, command.Snapshot{TargetID: jobID, Revision: 1, State: queuedBytes}); err != nil {
		t.Fatal(err)
	}

	lease := workflow.ExecutionLease{
		OwnerWorkerID: "worker-windows", AcquiredAt: now.Add(-2 * time.Minute), HeartbeatAt: now.Add(-2 * time.Minute),
		ExpiresAt: now.Add(-time.Minute), FencingToken: 1,
	}
	running := queued
	running.Revision = 2
	running.Steps[0].Workflow.State = workflow.Running
	running.Steps[0].Workflow.ExecutionLease = &lease
	running.Steps[0].Workflow.LastFencingToken = 1
	running.Steps[0].WorkerID = "worker-windows"
	runningBytes, err := json.Marshal(running)
	if err != nil {
		t.Fatal(err)
	}
	commit := dispatchCommit(jobID, compositeStepID, "RES-RUNNING-RECOVERY", 91, 1, now.Add(-2*time.Minute))
	commit.NextState = &command.Snapshot{TargetID: jobID, Revision: 2, State: runningBytes}
	commit.Lease = lease
	if err := store.CommitDispatch("running-recovery:dispatch", commit); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.RecoverExpiredReservations(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Revision != 3 || recovered[0].Steps[0].Workflow.State != workflow.Queued ||
		recovered[0].Steps[0].Workflow.ExecutionLease != nil || recovered[0].Steps[0].Workflow.Attempt != 1 || recovered[0].Steps[0].WorkerID != "" {
		t.Fatalf("expired RUNNING lease was not recovered atomically: %+v", recovered)
	}
	var activeReservations, activeLeases int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE reservation_id = ? AND released_at IS NULL`, commit.Reservation.ID).Scan(&activeReservations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM execution_leases WHERE step_id = ?`, compositeStepID).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if activeReservations != 0 || activeLeases != 0 {
		t.Fatalf("expired execution rows remain active: reservations=%d leases=%d", activeReservations, activeLeases)
	}
}

func dispatchCommit(jobID, stepID, reservationID string, snapshotID, dispatchSequence uint64, now time.Time) command.DispatchCommit {
	expectedRevision := uint64(1)
	event := command.Event{
		EventID: reservationID + ":event", EventType: "StepReserved", CommandID: reservationID + ":command",
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: jobID, BeforeRevision: 1, AfterRevision: 2, Reason: "dispatch ready Step",
		Payload: map[string]any{"step_id": stepID, "worker_id": "worker-windows"},
	}
	record := command.CommandRecord{Envelope: command.Envelope{
		CommandID: reservationID + ":command", CommandType: "step.reserve",
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: jobID, ExpectedRevision: 1, IdempotencyKey: jobID + ":dispatch", Reason: "dispatch ready Step",
	}, Accepted: true}
	base := command.Commit{
		ExpectedRevision: &expectedRevision,
		NextState:        &command.Snapshot{TargetID: jobID, Revision: 2, State: json.RawMessage(`{"step_state":"RESERVED"}`)},
		Record:           record, Events: []command.Event{event},
		Receipt: &command.Receipt{Digest: reservationID + ":digest", Result: command.Result{
			CommandID: record.Envelope.CommandID, TargetID: jobID, Revision: 2, Events: []command.Event{event},
		}},
	}
	return command.DispatchCommit{
		Commit: base,
		Reservation: dispatch.ResourceReservation{
			ID: reservationID, JobID: jobID, StepID: stepID, WorkerID: "worker-windows", SnapshotID: snapshotID,
			ResourceClass: vocabulary.ResourceHeavy, MemoryBytes: 4 << 30, ScratchBytes: 10 << 30,
			PhysicalDeviceIDs: []string{"disk-" + stepID}, CreatedAt: now,
		},
		Lease: workflow.ExecutionLease{
			OwnerWorkerID: "worker-windows", AcquiredAt: now, HeartbeatAt: now,
			ExpiresAt: now.Add(time.Minute), FencingToken: 1,
		},
		Capacity: dispatch.Capacity{
			AvailableMemoryBytes: 4 << 30, ScratchFreeBytes: 10 << 30,
			MaxHeavy: 1, MaxLight: 2,
		},
		DispatchSequence: dispatchSequence,
	}
}

func assertDispatchAbsent(t *testing.T, store *Store, jobID, stepID, reservationID string, revision uint64) {
	t.Helper()
	snapshot, err := store.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != revision || string(snapshot.State) != `{"step_state":"QUEUED"}` {
		t.Fatalf("failed dispatch changed state: %+v", snapshot)
	}
	for table, lookup := range map[string][2]string{
		"resource_reservations": {"reservation_id", reservationID},
		"execution_leases":      {"step_id", stepID},
		"step_fencing_tokens":   {"step_id", stepID},
	} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+lookup[0]+" = ?", lookup[1]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d partial rows", table, count)
		}
	}
}

func assertDispatchPresent(t *testing.T, store *Store, jobID, stepID, reservationID string, revision uint64) {
	t.Helper()
	snapshot, err := store.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != revision || string(snapshot.State) != `{"step_state":"RESERVED"}` {
		t.Fatalf("successful dispatch state is incomplete: %+v", snapshot)
	}
	var reservationCount, leaseCount, tokenCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM resource_reservations WHERE reservation_id = ?`, reservationID).Scan(&reservationCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM execution_leases WHERE step_id = ? AND reservation_id = ? AND fencing_token = 1`, stepID, reservationID).Scan(&leaseCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM step_fencing_tokens WHERE step_id = ? AND last_token = 1`, stepID).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if reservationCount != 1 || leaseCount != 1 || tokenCount != 1 {
		t.Fatalf("successful dispatch rows reservation=%d lease=%d token=%d", reservationCount, leaseCount, tokenCount)
	}
}
