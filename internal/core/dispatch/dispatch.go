package dispatch

import (
	"errors"
	"sort"
	"time"

	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

var (
	ErrInvalidQueue           = errors.New("dispatch queue is invalid")
	ErrInvalidReservation     = errors.New("resource reservation is invalid")
	ErrCapacityUnavailable    = errors.New("dispatch capacity is unavailable")
	ErrPhysicalDeviceReserved = errors.New("physical device is already reserved")
	ErrExecutionLeaseHeld     = errors.New("step already has an active execution lease")
	ErrFencingTokenConflict   = errors.New("fencing token is not the next token")
)

const DefaultAgingAfter = 30 * time.Minute

type Candidate struct {
	JobID          string                     `json:"job_id"`
	StepID         string                     `json:"step_id"`
	ReadyAt        time.Time                  `json:"ready_at"`
	NextEligibleAt time.Time                  `json:"next_eligible_at,omitempty"`
	Priority       resource.ExecutionPriority `json:"priority"`
	ResourceClass  vocabulary.ResourceClass   `json:"resource_class"`
}

type Queue struct {
	AgingAfter              time.Duration     `json:"aging_after"`
	Candidates              []Candidate       `json:"candidates"`
	JobLastDispatchSequence map[string]uint64 `json:"job_last_dispatch_sequence"`
}

func (queue Queue) Next(now time.Time) (Candidate, bool) {
	agingAfter := queue.AgingAfter
	if agingAfter <= 0 {
		agingAfter = DefaultAgingAfter
	}
	heads := make(map[string]Candidate)
	for _, candidate := range queue.Candidates {
		if candidate.JobID == "" || candidate.StepID == "" || candidate.ReadyAt.IsZero() ||
			(!candidate.NextEligibleAt.IsZero() && now.Before(candidate.NextEligibleAt)) {
			continue
		}
		current, exists := heads[candidate.JobID]
		if !exists || candidate.ReadyAt.Before(current.ReadyAt) ||
			(candidate.ReadyAt.Equal(current.ReadyAt) && candidate.StepID < current.StepID) {
			heads[candidate.JobID] = candidate
		}
	}
	ordered := make([]Candidate, 0, len(heads))
	for _, candidate := range heads {
		ordered = append(ordered, candidate)
	}
	if len(ordered) == 0 {
		return Candidate{}, false
	}
	sort.Slice(ordered, func(i, j int) bool {
		leftHigh := effectiveHigh(ordered[i], now, agingAfter)
		rightHigh := effectiveHigh(ordered[j], now, agingAfter)
		if leftHigh != rightHigh {
			return leftHigh
		}
		leftSequence := queue.JobLastDispatchSequence[ordered[i].JobID]
		rightSequence := queue.JobLastDispatchSequence[ordered[j].JobID]
		if leftSequence != rightSequence {
			return leftSequence < rightSequence
		}
		if !ordered[i].ReadyAt.Equal(ordered[j].ReadyAt) {
			return ordered[i].ReadyAt.Before(ordered[j].ReadyAt)
		}
		if ordered[i].JobID != ordered[j].JobID {
			return ordered[i].JobID < ordered[j].JobID
		}
		return ordered[i].StepID < ordered[j].StepID
	})
	return ordered[0], true
}

func (queue *Queue) RecordDispatch(jobID string, sequence uint64) error {
	if queue == nil || jobID == "" || sequence == 0 {
		return ErrInvalidQueue
	}
	if queue.JobLastDispatchSequence == nil {
		queue.JobLastDispatchSequence = make(map[string]uint64)
	}
	if sequence <= queue.JobLastDispatchSequence[jobID] {
		return ErrInvalidQueue
	}
	queue.JobLastDispatchSequence[jobID] = sequence
	return nil
}

func effectiveHigh(candidate Candidate, now time.Time, agingAfter time.Duration) bool {
	return candidate.Priority == resource.Immediate ||
		(candidate.Priority == resource.Background && !now.Before(candidate.ReadyAt.Add(agingAfter)))
}

type WorkerRequest struct {
	Requirement worker.Requirement
	Resources   resource.Request
}

type Observation struct {
	JobID       string    `json:"job_id"`
	JobRevision uint64    `json:"job_revision"`
	StepID      string    `json:"step_id"`
	ObservedAt  time.Time `json:"observed_at"`
	Blockers    []Blocker `json:"blockers"`
}

func (o Observation) SameDecision(other Observation) bool {
	if o.JobID != other.JobID || o.JobRevision != other.JobRevision || o.StepID != other.StepID || len(o.Blockers) != len(other.Blockers) {
		return false
	}
	for i, a := range o.Blockers {
		b := other.Blockers[i]
		if a.WorkerID != b.WorkerID || a.Code != b.Code || a.WaitReason != b.WaitReason {
			return false
		}
	}
	return true
}

type Blocker struct {
	WorkerID   string                `json:"worker_id,omitempty"`
	Code       string                `json:"code"`
	WaitReason vocabulary.WaitReason `json:"wait_reason,omitempty"`
	SnapshotID uint64                `json:"snapshot_id,omitempty"`
}

type WorkerDecision struct {
	WorkerID   string
	SnapshotID uint64
	Blockers   []Blocker
}

func SelectWorker(candidates []worker.DispatchCandidate, request WorkerRequest, now time.Time, ttl time.Duration, scheduler resource.Scheduler) WorkerDecision {
	ordered := append([]worker.DispatchCandidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Definition.ID < ordered[j].Definition.ID })
	type runnable struct {
		id         string
		snapshotID uint64
		running    int
	}
	var runnableWorkers []runnable
	decision := WorkerDecision{}
	for _, candidate := range ordered {
		workerID := candidate.Definition.ID
		if code := staticBlocker(candidate, request.Requirement, now, ttl); code != "" {
			decision.Blockers = append(decision.Blockers, Blocker{WorkerID: workerID, Code: code, SnapshotID: candidate.Metrics.SnapshotID})
			continue
		}
		snapshot := resource.Snapshot{
			AverageCPU5m: candidate.Metrics.AverageCPU5m, AverageGPU5m: candidate.Metrics.AverageGPU5m,
			AvailableMemoryGB: float64(candidate.Metrics.AvailableMemoryBytes) / float64(1<<30),
			ScratchFreeGB:     float64(candidate.Metrics.ScratchFreeBytes) / float64(1<<30),
			IdleStableFor:     candidate.Metrics.IdleStableFor, HeavyRunning: candidate.Metrics.HeavyRunning,
			LightRunning: candidate.Metrics.LightRunning, BusyPhysicalDeviceIDs: candidate.Metrics.BusyPhysicalDeviceIDs,
		}
		resourceDecision := scheduler.Evaluate(snapshot, request.Resources)
		if !resourceDecision.Start {
			decision.Blockers = append(decision.Blockers, Blocker{
				WorkerID: workerID, SnapshotID: candidate.Metrics.SnapshotID, Code: "RESOURCE", WaitReason: resourceDecision.Reason,
			})
			continue
		}
		running := candidate.Metrics.HeavyRunning
		if request.Resources.Class == vocabulary.ResourceLight {
			running = candidate.Metrics.LightRunning
		}
		runnableWorkers = append(runnableWorkers, runnable{id: workerID, snapshotID: candidate.Metrics.SnapshotID, running: running})
	}
	if len(runnableWorkers) == 0 {
		return decision
	}
	sort.Slice(runnableWorkers, func(i, j int) bool {
		if runnableWorkers[i].running != runnableWorkers[j].running {
			return runnableWorkers[i].running < runnableWorkers[j].running
		}
		return runnableWorkers[i].id < runnableWorkers[j].id
	})
	decision.WorkerID = runnableWorkers[0].id
	decision.SnapshotID = runnableWorkers[0].snapshotID
	return decision
}

func staticBlocker(candidate worker.DispatchCandidate, requirement worker.Requirement, now time.Time, ttl time.Duration) string {
	definition := candidate.Definition
	if !definition.Enabled {
		return "DISABLED"
	}
	if candidate.LastHeartbeat.IsZero() || now.Before(candidate.LastHeartbeat) || now.Sub(candidate.LastHeartbeat) > ttl {
		return "OFFLINE"
	}
	if !candidate.Metrics.AcceptingWork {
		return "NOT_ACCEPTING_WORK"
	}
	if requirement.PinnedWorkerID != "" && definition.ID != requirement.PinnedWorkerID {
		return "PINNED_TO_OTHER_WORKER"
	}
	if !supports(definition.ProcessingKinds, requirement.ProcessingKinds) {
		return "PROCESSING_KIND_UNSUPPORTED"
	}
	if !worker.SupportsConfiguredOrAttachedStorage(definition.AllowedStorageIDs, candidate.Metrics, requirement.StorageIDs) ||
		!supports(candidate.Metrics.AccessibleStorageIDs, requirement.StorageIDs) {
		return "STORAGE_UNAVAILABLE"
	}
	if candidate.Metrics.SnapshotID == 0 || candidate.Metrics.ObservedAt.IsZero() || now.Before(candidate.Metrics.ObservedAt) ||
		now.Sub(candidate.Metrics.ObservedAt) > ttl {
		return "STALE_SNAPSHOT"
	}
	return ""
}

func supports[T ~string](available map[T]bool, required []T) bool {
	for _, value := range required {
		if !available[value] {
			return false
		}
	}
	return true
}

type ResourceReservation struct {
	ID                string                   `json:"id"`
	JobID             string                   `json:"job_id"`
	StepID            string                   `json:"step_id"`
	WorkerID          string                   `json:"worker_id"`
	SnapshotID        uint64                   `json:"snapshot_id"`
	ResourceClass     vocabulary.ResourceClass `json:"resource_class"`
	MemoryBytes       uint64                   `json:"memory_bytes"`
	ScratchBytes      uint64                   `json:"scratch_bytes"`
	PhysicalDeviceIDs []string                 `json:"physical_device_ids,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
}

func (reservation ResourceReservation) Validate() error {
	if reservation.ID == "" || reservation.JobID == "" || reservation.StepID == "" || reservation.WorkerID == "" ||
		reservation.SnapshotID == 0 || !reservation.ResourceClass.Valid() || reservation.CreatedAt.IsZero() {
		return ErrInvalidReservation
	}
	seen := make(map[string]bool, len(reservation.PhysicalDeviceIDs))
	for _, deviceID := range reservation.PhysicalDeviceIDs {
		if deviceID == "" || seen[deviceID] {
			return ErrInvalidReservation
		}
		seen[deviceID] = true
	}
	return nil
}

type Capacity struct {
	AvailableMemoryBytes uint64
	ScratchFreeBytes     uint64
	HeavyRunning         int
	LightRunning         int
	MaxHeavy             int
	MaxLight             int
}
