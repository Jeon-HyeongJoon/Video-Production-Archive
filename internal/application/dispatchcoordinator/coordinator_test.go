package dispatchcoordinator_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/dispatchcoordinator"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSP-004
func TestDispatchAdmissionWaitsWithResourceReasonAndResumesAfterCapacityReturns(t *testing.T) {
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	target := job.State{
		ID: "JOB-TARGET", Revision: 1, TemplateName: "resource-test", TargetTitle: "대기 후 재개",
		CurrentStepID: "render",
		Steps: []job.Step{{
			Workflow:      workflow.Step{ID: "render", State: workflow.Queued, MaxAttempts: 3},
			ResourceClass: vocabulary.ResourceHeavy, ExecutorKind: vocabulary.ExecutorWorker,
			RequiredProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender},
			LastEventAt:             now, ReadyAt: now.Add(-time.Minute), ExecutionPriority: resource.Background,
			EstimatedMemoryBytes: resource.DefaultHeavyMemoryBytes, EstimatedScratchBytes: resource.DefaultHeavyScratchBytes,
			RequiredStorageIDs: []string{"archive-main"},
		}},
	}
	targetState, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := command.NewMemoryStore(map[string]command.Snapshot{
		target.ID:    {TargetID: target.ID, Revision: target.Revision, State: targetState},
		"JOB-ACTIVE": {TargetID: "JOB-ACTIVE", Revision: 1, State: json.RawMessage(`{"seed":true}`)},
	})
	seedActiveReservation(t, store, now)

	registry := worker.NewRegistry([]worker.Definition{{
		ID: "WORKER-1", Enabled: true, CredentialRef: "worker-secret",
		ProcessingKinds:   map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true},
		AllowedStorageIDs: map[string]bool{"archive-main": true},
	}}, acceptingVerifier{}, time.Minute)
	metrics := worker.Metrics{
		SnapshotID: 1, ObservedAt: now, AverageCPU5m: 10, AverageGPU5m: 5, IdleStableFor: 10 * time.Minute,
		AvailableMemoryBytes: 13 << 30, ScratchFreeBytes: 100 << 30, AcceptingWork: true,
		BusyPhysicalDeviceIDs: map[string]bool{}, AccessibleStorageIDs: map[string]bool{"archive-main": true},
	}
	if err := registry.Connect("WORKER-1", "presented", now, metrics); err != nil {
		t.Fatal(err)
	}
	projection := &recordingProjection{}
	coordinator, err := dispatchcoordinator.New(store, registry, projection, func() time.Time { return now }, time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DispatchAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}

	waiting := loadJob(t, store, target.ID)
	step := waiting.Steps[0]
	if waiting.Revision != 2 || step.Workflow.State != workflow.Waiting || step.Workflow.WaitReason != vocabulary.WaitMemory ||
		step.Workflow.Attempt != 0 || step.Workflow.ExecutionLease != nil {
		t.Fatalf("resource admission did not persist WAITING/MEMORY without an attempt: %+v", waiting)
	}
	if len(projection.views) != 1 || projection.views[0].Summary.WaitReason != vocabulary.WaitMemory {
		t.Fatalf("resource wait was not projected: %+v", projection.views)
	}

	metrics.SnapshotID = 2
	metrics.ObservedAt = now.Add(time.Second)
	metrics.AvailableMemoryBytes = 20 << 30
	now = now.Add(time.Second)
	if err := registry.Heartbeat("WORKER-1", now, metrics); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DispatchAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}

	reserved := loadJob(t, store, target.ID)
	step = reserved.Steps[0]
	if reserved.Revision != 3 || step.Workflow.State != workflow.Reserved || step.Workflow.WaitReason != "" ||
		step.Workflow.Attempt != 0 || step.Workflow.ExecutionLease == nil || step.WorkerID != "WORKER-1" {
		t.Fatalf("recovered capacity did not reserve the waiting Step: %+v", reserved)
	}
	reservations, err := store.ListActiveReservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 2 {
		t.Fatalf("reservation was not committed with the resumed Step: %+v", reservations)
	}
}

type acceptingVerifier struct{}

func (acceptingVerifier) Verify(string, string, string) bool { return true }

type recordingProjection struct {
	views []archiveworkspace.JobView
}

func (projection *recordingProjection) Replace(_ context.Context, view archiveworkspace.JobView) error {
	projection.views = append(projection.views, view)
	return nil
}

func loadJob(t *testing.T, store *command.MemoryStore, jobID string) job.State {
	t.Helper()
	snapshot, err := store.Load(jobID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func seedActiveReservation(t *testing.T, store *command.MemoryStore, now time.Time) {
	t.Helper()
	expected := uint64(1)
	event := command.Event{
		EventID: "seed:event", EventType: "StepReserved", CommandID: "seed:command",
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: "JOB-ACTIVE", BeforeRevision: 1, AfterRevision: 2, Reason: "seed active reservation",
	}
	err := store.CommitDispatch("seed:reservation", command.DispatchCommit{
		Commit: command.Commit{
			ExpectedRevision: &expected,
			NextState:        &command.Snapshot{TargetID: "JOB-ACTIVE", Revision: 2, State: json.RawMessage(`{"seed":true}`)},
			Record:           command.CommandRecord{Envelope: command.Envelope{CommandID: "seed:command"}, Accepted: true},
			Events:           []command.Event{event},
		},
		Reservation: dispatch.ResourceReservation{
			ID: "RES-ACTIVE", JobID: "JOB-ACTIVE", StepID: "JOB-ACTIVE/light", WorkerID: "WORKER-1",
			SnapshotID: 1, ResourceClass: vocabulary.ResourceLight, MemoryBytes: 4 << 30, ScratchBytes: 1 << 30,
			CreatedAt: now,
		},
		Lease: workflow.ExecutionLease{
			OwnerWorkerID: "WORKER-1", AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute), FencingToken: 1,
		},
		Capacity: dispatch.Capacity{
			AvailableMemoryBytes: 20 << 30, ScratchFreeBytes: 100 << 30, MaxHeavy: 1, MaxLight: 2,
		},
		DispatchSequence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Requirement: DSP-005
func TestDispatchPreservesStaticBlockerUntilEligibilityChanges(t *testing.T) {
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	target := job.State{
		ID: "JOB-TARGET", Revision: 1, TemplateName: "resource-test", TargetTitle: "대기 후 재개",
		CurrentStepID: "render",
		Steps: []job.Step{{
			Workflow:      workflow.Step{ID: "render", State: workflow.Queued, MaxAttempts: 3},
			ResourceClass: vocabulary.ResourceHeavy, ExecutorKind: vocabulary.ExecutorWorker,
			RequiredProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender},
			LastEventAt:             now, ReadyAt: now.Add(-time.Minute), ExecutionPriority: resource.Background,
			EstimatedMemoryBytes: resource.DefaultHeavyMemoryBytes, EstimatedScratchBytes: resource.DefaultHeavyScratchBytes,
			RequiredStorageIDs: []string{"archive-main"},
		}},
	}
	targetState, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := command.NewMemoryStore(map[string]command.Snapshot{
		target.ID:    {TargetID: target.ID, Revision: target.Revision, State: targetState},
		"JOB-ACTIVE": {TargetID: "JOB-ACTIVE", Revision: 1, State: json.RawMessage(`{"seed":true}`)},
	})
	seedActiveReservation(t, store, now)

	registry := worker.NewRegistry([]worker.Definition{{
		ID: "WORKER-1", Enabled: true, CredentialRef: "worker-secret",
		ProcessingKinds:   map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true},
		AllowedStorageIDs: map[string]bool{"archive-main": true},
	}}, acceptingVerifier{}, time.Minute)
	metrics := worker.Metrics{
		SnapshotID: 1, ObservedAt: now, AverageCPU5m: 10, AverageGPU5m: 5, IdleStableFor: 10 * time.Minute,
		AvailableMemoryBytes: 13 << 30, ScratchFreeBytes: 100 << 30, AcceptingWork: true,
		BusyPhysicalDeviceIDs: map[string]bool{}, AccessibleStorageIDs: map[string]bool{},
	}
	if err := registry.Connect("WORKER-1", "presented", now, metrics); err != nil {
		t.Fatal(err)
	}
	projection := &recordingProjection{}
	coordinator, err := dispatchcoordinator.New(store, registry, projection, func() time.Time { return now }, time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := coordinator.DispatchAvailable(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	blocked := loadJob(t, store, target.ID)
	step := blocked.Steps[0]
	items, err := store.DispatchObservations(context.Background(), target.ID, blocked.Revision)
	if err != nil || blocked.Revision != 1 || step.Workflow.State != workflow.Queued || len(items) != 1 || len(items[0].Blockers) != 1 || !items[0].ObservedAt.Equal(now) {
		t.Fatalf("missing independent dispatch evidence: %+v %v", items, err)
	}
	b := items[0].Blockers[0]
	if b.Code != "STORAGE_UNAVAILABLE" || b.SnapshotID != 1 || b.WorkerID != "WORKER-1" {
		t.Fatalf("missing admission basis %+v", b)
	}
	now = now.Add(time.Second)
	metrics.SnapshotID = 2
	metrics.ObservedAt = now
	metrics.AvailableMemoryBytes = 20 << 30
	metrics.AccessibleStorageIDs = map[string]bool{"archive-main": true}
	if err := registry.Heartbeat("WORKER-1", now, metrics); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DispatchAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	step = loadJob(t, store, target.ID).Steps[0]
	if step.Workflow.State != workflow.Reserved {
		t.Fatalf("obsolete blockers retained: %+v", step)
	}
}
