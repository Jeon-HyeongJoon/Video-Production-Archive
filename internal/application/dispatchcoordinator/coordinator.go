package dispatchcoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/jobcontrol"
	"videoarchive/internal/application/workercapacity"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type ProjectionWriter interface {
	Replace(context.Context, archiveworkspace.JobView) error
}

type Coordinator struct {
	store        command.DispatchStore
	workers      *worker.Registry
	projection   ProjectionWriter
	now          func() time.Time
	heartbeatTTL time.Duration
	leaseTTL     time.Duration
	scheduler    resource.Scheduler
}

func New(store command.DispatchStore, workers *worker.Registry, projection ProjectionWriter, now func() time.Time, heartbeatTTL, leaseTTL time.Duration) (*Coordinator, error) {
	if store == nil || workers == nil || projection == nil || now == nil || heartbeatTTL <= 0 || leaseTTL <= 0 {
		return nil, errors.New("dispatch coordinator dependencies are incomplete")
	}
	return &Coordinator{
		store: store, workers: workers, projection: projection, now: now,
		heartbeatTTL: heartbeatTTL, leaseTTL: leaseTTL, scheduler: resource.DefaultScheduler(),
	}, nil
}

func (coordinator *Coordinator) DispatchAvailable(ctx context.Context) error {
	ignored := make(map[string]bool)
	for {
		outcome, candidateKey, err := coordinator.dispatchOnce(ctx, ignored)
		if err != nil {
			return err
		}
		switch outcome {
		case dispatchReserved:
			continue
		case dispatchBlocked:
			ignored[candidateKey] = true
			continue
		default:
			return nil
		}
	}
}

func (coordinator *Coordinator) DispatchOnce(ctx context.Context) (bool, error) {
	outcome, _, err := coordinator.dispatchOnce(ctx, nil)
	return outcome == dispatchReserved, err
}

type dispatchOutcome uint8

const (
	dispatchEmpty dispatchOutcome = iota
	dispatchBlocked
	dispatchReserved
)

func (coordinator *Coordinator) dispatchOnce(ctx context.Context, ignored map[string]bool) (dispatchOutcome, string, error) {
	now := coordinator.now().UTC()
	snapshots, err := coordinator.store.ListSnapshots()
	if err != nil {
		return dispatchEmpty, "", err
	}
	queue := dispatch.Queue{AgingAfter: dispatch.DefaultAgingAfter, JobLastDispatchSequence: make(map[string]uint64)}
	states := make(map[string]job.State, len(snapshots))
	stateSnapshots := make(map[string]command.Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		state, decodeErr := job.Decode(snapshot)
		if decodeErr != nil {
			continue
		}
		states[state.ID] = state
		stateSnapshots[state.ID] = snapshot
		queue.JobLastDispatchSequence[state.ID] = state.LastDispatchSequence
		steps := make(map[string]*workflow.Step, len(state.Steps))
		for index := range state.Steps {
			steps[state.Steps[index].Workflow.ID] = &state.Steps[index].Workflow
		}
		for _, step := range state.Steps {
			candidateKey := state.ID + "/" + step.Workflow.ID
			if ignored[candidateKey] || step.ExecutorKind != vocabulary.ExecutorWorker || !resourceAdmissionState(step.Workflow) ||
				!step.Workflow.Ready(steps) || step.ReadyAt.IsZero() {
				continue
			}
			queue.Candidates = append(queue.Candidates, dispatch.Candidate{
				JobID: state.ID, StepID: step.Workflow.ID, ReadyAt: step.ReadyAt,
				NextEligibleAt: step.NextEligibleAt, Priority: step.ExecutionPriority, ResourceClass: step.ResourceClass,
			})
		}
	}
	candidate, found := queue.Next(now)
	if !found {
		return dispatchEmpty, "", nil
	}
	candidateKey := candidate.JobID + "/" + candidate.StepID
	state := states[candidate.JobID]
	step := findStep(&state, candidate.StepID)
	if step == nil {
		return dispatchBlocked, candidateKey, nil
	}
	reservations, err := coordinator.store.ListActiveReservations()
	if err != nil {
		return dispatchEmpty, "", err
	}
	candidates := coordinator.workers.DispatchCandidates()
	quarantinedWorkers := make(map[string]bool)
	for _, reservation := range reservations {
		state, found := states[reservation.JobID]
		if !found {
			continue
		}
		stepID := strings.TrimPrefix(reservation.StepID, reservation.JobID+"/")
		reservedStep := findStep(&state, stepID)
		if reservedStep == nil || reservedStep.Workflow.ExecutionLease != nil {
			continue
		}
		switch reservedStep.Workflow.State {
		case workflow.Running, workflow.Verifying, workflow.Paused:
			quarantinedWorkers[reservation.WorkerID] = true
		}
	}
	var excluded []dispatch.Blocker
	filteredCandidates := candidates[:0]
	for _, candidate := range candidates {
		if quarantinedWorkers[candidate.Definition.ID] {
			excluded = append(excluded, dispatch.Blocker{WorkerID: candidate.Definition.ID, Code: "WORKER_QUARANTINED", SnapshotID: candidate.Metrics.SnapshotID})
		} else {
			filteredCandidates = append(filteredCandidates, candidate)
		}
	}
	candidates = filteredCandidates
	for index := range candidates {
		candidates[index].Metrics, _ = workercapacity.Account(candidates[index].Metrics, reservations, candidates[index].Definition.ID)
	}
	workerDecision := dispatch.SelectWorker(candidates, dispatch.WorkerRequest{
		Requirement: worker.Requirement{
			ProcessingKinds: append([]vocabulary.ProcessingKind(nil), step.RequiredProcessingKinds...),
			StorageIDs:      append([]string(nil), step.RequiredStorageIDs...),
		},
		Resources: resource.Request{
			Class: step.ResourceClass, Priority: step.ExecutionPriority,
			EstimatedMemoryGB: float64(step.EstimatedMemoryBytes) / float64(1<<30),
			ScratchRequiredGB: float64(step.EstimatedScratchBytes) / float64(1<<30),
			PhysicalDeviceIDs: append([]string(nil), step.PhysicalDeviceIDs...),
		},
	}, now, coordinator.heartbeatTTL, coordinator.scheduler)
	if workerDecision.WorkerID == "" {
		blockers := append(workerDecision.Blockers, excluded...)
		if len(blockers) == 0 {
			blockers = []dispatch.Blocker{{Code: "NO_CONFIGURED_WORKER"}}
		}
		sort.Slice(blockers, func(i, j int) bool { return blockers[i].WorkerID < blockers[j].WorkerID })
		if reason := resourceWaitReason(blockers); reason.Valid() {
			if err := coordinator.persistResourceWait(ctx, &state, stateSnapshots[state.ID], step, reason, now); err != nil {
				return dispatchBlocked, candidateKey, err
			}
		}
		err := coordinator.store.RecordDispatchObservation(ctx, dispatch.Observation{JobID: state.ID, JobRevision: state.Revision, StepID: step.Workflow.ID, ObservedAt: now, Blockers: blockers})
		if err != nil {
			return dispatchBlocked, candidateKey, err
		}
		return dispatchBlocked, candidateKey, nil
	}
	metrics, usable := coordinator.workers.DispatchSnapshot(workerDecision.WorkerID, now)
	if !usable || metrics.SnapshotID != workerDecision.SnapshotID {
		return dispatchBlocked, candidateKey, nil
	}
	dispatchStepID := state.ID + "/" + step.Workflow.ID
	fencingToken, err := coordinator.store.NextFencingToken(dispatchStepID)
	if err != nil {
		return dispatchEmpty, "", err
	}
	dispatchSequence, err := coordinator.store.NextDispatchSequence()
	if err != nil {
		return dispatchEmpty, "", err
	}
	if err := step.Workflow.Transition(workflow.Reserved); err != nil {
		return dispatchBlocked, candidateKey, err
	}
	if err := step.Workflow.AssignExecutionLease(workerDecision.WorkerID, now, coordinator.leaseTTL, fencingToken); err != nil {
		return dispatchBlocked, candidateKey, err
	}

	step.WorkerID = workerDecision.WorkerID
	step.LastEventAt = now
	state.CurrentStepID = step.Workflow.ID
	state.LastDispatchSequence = dispatchSequence
	state.Revision = stateSnapshots[state.ID].Revision + 1
	encodedState, err := json.Marshal(state)
	if err != nil {
		return dispatchEmpty, "", err
	}
	commandID := fmt.Sprintf("dispatch:%s:%s:%d", state.ID, step.Workflow.ID, dispatchSequence)
	idempotencyKey := commandID
	reason := "dispatch ready Step"
	event := command.Event{
		EventID: commandID + ":event", EventType: "StepReserved", CommandID: commandID,
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: state.ID, BeforeRevision: stateSnapshots[state.ID].Revision, AfterRevision: state.Revision, Reason: reason,
		Payload: map[string]any{"step_id": step.Workflow.ID, "worker_id": workerDecision.WorkerID, "snapshot_id": metrics.SnapshotID},
	}
	envelope := command.Envelope{
		CommandID: commandID, CommandType: "step.reserve",
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: state.ID, ExpectedRevision: stateSnapshots[state.ID].Revision,
		IdempotencyKey: idempotencyKey, Reason: reason,
	}
	expectedRevision := stateSnapshots[state.ID].Revision
	result := command.Result{CommandID: commandID, TargetID: state.ID, Revision: state.Revision, Value: state, Events: []command.Event{event}}
	reservationID := fmt.Sprintf("reservation:%s:%s:%d", state.ID, step.Workflow.ID, dispatchSequence)
	commit := command.DispatchCommit{
		Commit: command.Commit{
			ExpectedRevision: &expectedRevision,
			NextState:        &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encodedState},
			Record:           command.CommandRecord{Envelope: envelope, Accepted: true}, Events: []command.Event{event},
			Receipt: &command.Receipt{Digest: commandID, Result: result},
		},
		Reservation: dispatch.ResourceReservation{
			ID: reservationID, JobID: state.ID, StepID: dispatchStepID, WorkerID: workerDecision.WorkerID,
			SnapshotID: metrics.SnapshotID, ResourceClass: step.ResourceClass,
			MemoryBytes: step.EstimatedMemoryBytes, ScratchBytes: step.EstimatedScratchBytes,
			PhysicalDeviceIDs: append([]string(nil), step.PhysicalDeviceIDs...), CreatedAt: now,
		},
		Lease: *step.Workflow.ExecutionLease,
		Capacity: dispatch.Capacity{
			AvailableMemoryBytes: reserveSafeMemory(metrics.AvailableMemoryBytes), ScratchFreeBytes: metrics.ScratchFreeBytes,
			HeavyRunning: metrics.HeavyRunning, LightRunning: metrics.LightRunning,
			MaxHeavy: coordinator.scheduler.MaxHeavy, MaxLight: coordinator.scheduler.MaxLight,
		},
		DispatchSequence: dispatchSequence,
	}
	if err := coordinator.store.CommitDispatch(idempotencyKey, commit); err != nil {
		if errors.Is(err, dispatch.ErrCapacityUnavailable) || errors.Is(err, command.ErrRevisionConflict) ||
			errors.Is(err, dispatch.ErrExecutionLeaseHeld) || errors.Is(err, dispatch.ErrFencingTokenConflict) {
			return dispatchBlocked, candidateKey, nil
		}
		return dispatchEmpty, "", err
	}
	view, err := jobcontrol.Project(state)
	if err != nil {
		return dispatchReserved, candidateKey, err
	}
	if err := coordinator.projection.Replace(ctx, view); err != nil {
		return dispatchReserved, candidateKey, err
	}
	return dispatchReserved, candidateKey, nil
}

func resourceAdmissionState(step workflow.Step) bool {
	if step.State == workflow.Queued {
		return true
	}
	if step.State != workflow.Waiting {
		return false
	}
	switch step.WaitReason {
	case vocabulary.WaitIdle, vocabulary.WaitMemory, vocabulary.WaitScratch, vocabulary.WaitSlot,
		vocabulary.WaitDisk, vocabulary.WaitPriority:
		return true
	default:
		return false
	}
}

func resourceWaitReason(blockers []dispatch.Blocker) vocabulary.WaitReason {
	for _, blocker := range blockers {
		if blocker.Code == "RESOURCE" && blocker.WaitReason.Valid() {
			return blocker.WaitReason
		}
	}
	return ""
}

func (coordinator *Coordinator) persistResourceWait(
	ctx context.Context,
	state *job.State,
	snapshot command.Snapshot,
	step *job.Step,
	reason vocabulary.WaitReason,
	now time.Time,
) error {
	if step.Workflow.State == workflow.Waiting && step.Workflow.WaitReason == reason {
		return nil
	}
	if step.Workflow.State == workflow.Waiting {
		if err := step.Workflow.Transition(workflow.Queued); err != nil {
			return err
		}
	}
	if err := step.Workflow.Wait(reason); err != nil {
		return err
	}
	step.WorkerID = ""
	step.LastEventAt = now
	state.CurrentStepID = step.Workflow.ID
	state.Revision = snapshot.Revision + 1
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	commandID := fmt.Sprintf("resource-wait:%s:%s:%d", state.ID, step.Workflow.ID, state.Revision)
	recordReason := "defer queued Step for Worker resource capacity"
	event := command.Event{
		EventID: commandID + ":event", EventType: "StepWaiting", CommandID: commandID,
		Actor: command.Actor{Type: command.SystemActor, ID: "dispatch-coordinator"}, RequestedVia: "dispatch_coordinator",
		TargetID: state.ID, BeforeRevision: snapshot.Revision, AfterRevision: state.Revision, Reason: recordReason,
		Payload: map[string]any{"step_id": step.Workflow.ID, "wait_reason": reason},
	}
	expected := snapshot.Revision
	if err := coordinator.store.Commit(commandID, command.Commit{
		ExpectedRevision: &expected,
		NextState:        &command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: encoded},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: commandID, CommandType: "step.wait_for_resource",
			Actor: event.Actor, RequestedVia: event.RequestedVia, TargetID: state.ID,
			ExpectedRevision: snapshot.Revision, IdempotencyKey: commandID, Reason: recordReason,
		}, Accepted: true},
		Events: []command.Event{event},
	}); err != nil {
		if errors.Is(err, command.ErrRevisionConflict) {
			return nil
		}
		return err
	}
	view, err := jobcontrol.Project(*state)
	if err != nil {
		return err
	}
	return coordinator.projection.Replace(ctx, view)
}

func findStep(state *job.State, stepID string) *job.Step {
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == stepID {
			return &state.Steps[index]
		}
	}
	return nil
}

func reserveSafeMemory(available uint64) uint64 {
	const safety = uint64(6 << 30)
	if available <= safety {
		return 0
	}
	return available - safety
}
