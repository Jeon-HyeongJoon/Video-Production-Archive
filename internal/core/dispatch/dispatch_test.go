package dispatch

import (
	"encoding/json"
	"testing"
	"time"

	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

// Requirement: DSP-001
func TestPersistentDispatchQueueUsesAgingAndJobFairnessAcrossRestart(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	queue := Queue{
		AgingAfter: DefaultAgingAfter,
		Candidates: []Candidate{
			{JobID: "JOB-A", StepID: "step-newer", ReadyAt: now.Add(-20 * time.Minute), Priority: resource.Immediate, ResourceClass: vocabulary.ResourceHeavy},
			{JobID: "JOB-A", StepID: "step-oldest", ReadyAt: now.Add(-25 * time.Minute), Priority: resource.Immediate, ResourceClass: vocabulary.ResourceHeavy},
			{JobID: "JOB-B", StepID: "step-aged", ReadyAt: now.Add(-31 * time.Minute), Priority: resource.Background, ResourceClass: vocabulary.ResourceHeavy},
			{JobID: "JOB-C", StepID: "step-recent", ReadyAt: now.Add(-10 * time.Minute), Priority: resource.Background, ResourceClass: vocabulary.ResourceLight},
			{JobID: "JOB-D", StepID: "step-backoff", ReadyAt: now.Add(-time.Hour), NextEligibleAt: now.Add(time.Minute), Priority: resource.Immediate, ResourceClass: vocabulary.ResourceHeavy},
		},
		JobLastDispatchSequence: map[string]uint64{"JOB-A": 5, "JOB-B": 2, "JOB-C": 0, "JOB-D": 0},
	}
	want, ok := queue.Next(now)
	if !ok || want.JobID != "JOB-B" || want.StepID != "step-aged" {
		t.Fatalf("unexpected next candidate before restart: %+v ok=%v", want, ok)
	}
	payload, err := json.Marshal(queue)
	if err != nil {
		t.Fatal(err)
	}
	var restarted Queue
	if err := json.Unmarshal(payload, &restarted); err != nil {
		t.Fatal(err)
	}
	afterRestart, ok := restarted.Next(now)
	if !ok || afterRestart != want {
		t.Fatalf("restart changed queue order: before=%+v after=%+v", want, afterRestart)
	}
	if err := restarted.RecordDispatch("JOB-B", 6); err != nil {
		t.Fatal(err)
	}
	nextRound, ok := restarted.Next(now)
	if !ok || nextRound.JobID != "JOB-A" || nextRound.StepID != "step-oldest" {
		t.Fatalf("job fairness did not rotate the next head: %+v ok=%v", nextRound, ok)
	}
}

// Requirement: DSP-002
func TestResourceAwareWorkerSelectionUsesFreshStaticEligibleSnapshotAndRealBlockers(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	definition := func(id string) worker.Definition {
		return worker.Definition{
			ID: id, Enabled: true,
			ProcessingKinds:   map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true},
			AllowedStorageIDs: map[string]bool{"archive": true},
		}
	}
	metrics := func(id uint64) worker.Metrics {
		return worker.Metrics{
			SnapshotID: id, ObservedAt: now, AverageCPU5m: 10, AverageGPU5m: 5,
			IdleStableFor: 10 * time.Minute, AvailableMemoryBytes: 16 << 30, ScratchFreeBytes: 100 << 30,
			BusyPhysicalDeviceIDs: map[string]bool{}, AccessibleStorageIDs: map[string]bool{"archive": true},
			AcceptingWork: true,
		}
	}
	disabled := definition("worker-disabled")
	disabled.Enabled = false
	memoryBlocked := metrics(2)
	memoryBlocked.AvailableMemoryBytes = 9 << 30
	stale := metrics(3)
	stale.ObservedAt = now.Add(-2 * time.Minute)
	runnableA := metrics(4)
	runnableA.HeavyRunning = 0
	runnableZ := metrics(5)
	runnableZ.HeavyRunning = 0
	candidates := []worker.DispatchCandidate{
		{Definition: disabled, LastHeartbeat: now, Metrics: metrics(1)},
		{Definition: definition("worker-memory"), LastHeartbeat: now, Metrics: memoryBlocked},
		{Definition: definition("worker-stale"), LastHeartbeat: now, Metrics: stale},
		{Definition: definition("worker-z"), LastHeartbeat: now, Metrics: runnableZ},
		{Definition: definition("worker-a"), LastHeartbeat: now, Metrics: runnableA},
	}
	request := WorkerRequest{
		Requirement: worker.Requirement{
			ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"archive"},
		},
		Resources: resource.Request{
			Class: resource.Heavy, Priority: resource.Background, EstimatedMemoryGB: 4,
			ScratchRequiredGB: 20, ScratchSafetyGB: 5,
		},
	}
	decision := SelectWorker(candidates, request, now, time.Minute, resource.DefaultScheduler())
	if decision.WorkerID != "worker-a" || decision.SnapshotID != 4 {
		t.Fatalf("deterministic runnable Worker was not selected: %+v", decision)
	}
	blockers := make(map[string]Blocker, len(decision.Blockers))
	for _, blocker := range decision.Blockers {
		blockers[blocker.WorkerID] = blocker
	}
	if blockers["worker-disabled"].Code != "DISABLED" || blockers["worker-stale"].Code != "STALE_SNAPSHOT" ||
		blockers["worker-memory"].WaitReason != vocabulary.WaitMemory {
		t.Fatalf("selection returned inferred or missing blockers: %+v", decision.Blockers)
	}

	for index := range candidates {
		if candidates[index].Definition.ID == "worker-a" || candidates[index].Definition.ID == "worker-z" {
			candidates[index].Metrics.AvailableMemoryBytes = 9 << 30
		}
	}
	blocked := SelectWorker(candidates, request, now, time.Minute, resource.DefaultScheduler())
	if blocked.WorkerID != "" {
		t.Fatalf("resource-blocked Workers produced a selection: %+v", blocked)
	}
	for _, blocker := range blocked.Blockers {
		if (blocker.WorkerID == "worker-a" || blocker.WorkerID == "worker-z") && blocker.WaitReason != vocabulary.WaitMemory {
			t.Fatalf("actual memory blocker was replaced: %+v", blocker)
		}
	}
}
