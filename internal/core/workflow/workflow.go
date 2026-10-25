package workflow

import (
	"errors"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/vocabulary"
)

type StepState string

const (
	Queued    StepState = "QUEUED"
	Waiting   StepState = "WAITING"
	Reserved  StepState = "RESERVED"
	Running   StepState = "RUNNING"
	Verifying StepState = "VERIFYING"
	Paused    StepState = "PAUSED"
	Failed    StepState = "FAILED"
	Completed StepState = "COMPLETED"
	Cancelled StepState = "CANCELLED"
)

type WaitReason = vocabulary.WaitReason

const (
	WaitIdle     = vocabulary.WaitIdle
	WaitMemory   = vocabulary.WaitMemory
	WaitScratch  = vocabulary.WaitScratch
	WaitSlot     = vocabulary.WaitSlot
	WaitDisk     = vocabulary.WaitDisk
	WaitApproval = vocabulary.WaitApproval
	WaitInput    = vocabulary.WaitInput
	WaitDevice   = vocabulary.WaitDevice
	WaitPriority = vocabulary.WaitPriority
)

type ControlMode string

const (
	AgentMode    ControlMode = "AGENT"
	AssistedMode ControlMode = "ASSISTED"
	ManualMode   ControlMode = "MANUAL"
)

var (
	ErrIllegalTransition  = errors.New("illegal step state transition")
	ErrApprovalRequired   = errors.New("step requires approval")
	ErrExecutionLeaseHeld = errors.New("step already has a valid execution lease")
	ErrStaleFencingToken  = errors.New("worker result has a stale fencing token")
)

type ExecutionLease struct {
	OwnerWorkerID string
	AcquiredAt    time.Time
	ExpiresAt     time.Time
	HeartbeatAt   time.Time
	FencingToken  uint64
}

type Approval = approval.Record

const (
	Approved = vocabulary.ApprovalApproved
	Rejected = vocabulary.ApprovalRejected
)

type Step struct {
	ID               string
	Dependencies     []string
	State            StepState
	WaitReason       WaitReason
	ControlMode      ControlMode
	ExecutionLease   *ExecutionLease
	Attempt          int
	MaxAttempts      int
	ApprovalRequired bool
	ApprovalRevision uint64
	Approvals        []Approval
	nextFencingToken uint64
}

func (s Step) Ready(steps map[string]*Step) bool {
	if s.State != Queued && s.State != Waiting {
		return false
	}
	for _, dependency := range s.Dependencies {
		step, ok := steps[dependency]
		if !ok || step.State != Completed {
			return false
		}
	}
	return true
}

func (s *Step) Transition(to StepState) error {
	if to == Waiting {
		return ErrIllegalTransition
	}
	return s.transition(to, "")
}

func (s *Step) Wait(reason WaitReason) error {
	if !reason.Valid() {
		return ErrIllegalTransition
	}
	return s.transition(Waiting, reason)
}

func (s *Step) transition(to StepState, reason WaitReason) error {
	if !allowedTransitions[s.State][to] {
		return ErrIllegalTransition
	}
	s.State = to
	if to == Waiting {
		s.WaitReason = reason
	} else {
		s.WaitReason = ""
	}
	return nil
}

var allowedTransitions = map[StepState]map[StepState]bool{
	Queued:    {Waiting: true, Reserved: true, Cancelled: true},
	Waiting:   {Queued: true, Reserved: true, Cancelled: true},
	Reserved:  {Running: true, Queued: true, Cancelled: true},
	Running:   {Verifying: true, Queued: true, Waiting: true, Paused: true, Failed: true, Cancelled: true},
	Verifying: {Completed: true, Queued: true, Failed: true},
	Paused:    {Queued: true, Cancelled: true},
	Failed:    {}, Completed: {}, Cancelled: {},
}

func (s *Step) TakeManualControl() error {
	if isTerminal(s.State) {
		return ErrIllegalTransition
	}
	s.ControlMode = ManualMode
	return nil
}

func (s *Step) RecoverExpiredLease(now time.Time) bool {
	return s.ReconcileExpiredLease(now, false)
}

func (s *Step) ReconcileExpiredLease(now time.Time, artifactPublished bool) bool {
	if s.State != Running || s.ExecutionLease == nil || now.Before(s.ExecutionLease.ExpiresAt) {
		return false
	}
	target := Queued
	if artifactPublished {
		target = Verifying
	}
	if err := s.transition(target, ""); err != nil {
		return false
	}
	s.ExecutionLease = nil
	return true
}

type FailureClass string

const (
	FailureTransient FailureClass = "TRANSIENT"
	FailureResource  FailureClass = "RESOURCE"
	FailureInput     FailureClass = "INPUT"
)

type Failure struct {
	Class      FailureClass
	WaitReason WaitReason
}

func (s *Step) RecordFailure(failure Failure) error {
	if isTerminal(s.State) || (s.State != Running && s.State != Verifying) {
		return ErrIllegalTransition
	}
	if failure.Class == FailureResource {
		if !resourceWaitReason(failure.WaitReason) {
			return ErrIllegalTransition
		}
		if err := s.Wait(failure.WaitReason); err != nil {
			return err
		}
		s.ExecutionLease = nil
		return nil
	}
	if failure.Class != FailureTransient && failure.Class != FailureInput {
		return ErrIllegalTransition
	}
	nextAttempt := s.Attempt + 1
	target := Queued
	if failure.Class == FailureInput || (s.MaxAttempts > 0 && nextAttempt >= s.MaxAttempts) {
		target = Failed
	}
	if err := s.transition(target, ""); err != nil {
		return err
	}
	s.Attempt = nextAttempt
	s.ExecutionLease = nil
	return nil
}

func resourceWaitReason(reason WaitReason) bool {
	switch reason {
	case WaitMemory, WaitScratch, WaitSlot, WaitDisk, WaitDevice:
		return true
	default:
		return false
	}
}

func (s *Step) AuthorizeRun() error {
	if s.State != Queued && (s.State != Waiting || s.WaitReason != WaitApproval) {
		return ErrIllegalTransition
	}
	if s.ApprovalRequired && !s.hasValidApproval() {
		if s.State == Queued {
			if err := s.Wait(WaitApproval); err != nil {
				return err
			}
		}
		return ErrApprovalRequired
	}
	if s.State == Waiting {
		return s.transition(Queued, "")
	}
	return nil
}

func (s *Step) hasValidApproval() bool {
	return approval.Approved(s.Approvals, s.ID, s.ApprovalRevision, "")
}

func (s *Step) ClaimExecutionLease(workerID string, now time.Time, ttl time.Duration) (uint64, error) {
	if workerID == "" || ttl <= 0 || (s.State != Reserved && s.State != Running) {
		return 0, ErrIllegalTransition
	}
	if s.ExecutionLease != nil && now.Before(s.ExecutionLease.ExpiresAt) {
		return 0, ErrExecutionLeaseHeld
	}
	s.nextFencingToken++
	s.ExecutionLease = &ExecutionLease{
		OwnerWorkerID: workerID, AcquiredAt: now, HeartbeatAt: now,
		ExpiresAt: now.Add(ttl), FencingToken: s.nextFencingToken,
	}
	return s.nextFencingToken, nil
}

func (s *Step) AcceptWorkerResult(workerID string, fencingToken uint64) error {
	if s.ExecutionLease == nil || s.ExecutionLease.OwnerWorkerID != workerID || s.ExecutionLease.FencingToken != fencingToken {
		return ErrStaleFencingToken
	}
	return nil
}

func isTerminal(state StepState) bool {
	return state == Completed || state == Failed || state == Cancelled
}
