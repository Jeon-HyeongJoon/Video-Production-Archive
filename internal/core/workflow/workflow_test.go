package workflow

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: WFL-001
func TestStepBecomesReadyOnlyAfterEveryDependencyCompletes(t *testing.T) {
	s := Step{ID: "qc", State: Queued, Dependencies: []string{"render", "summary"}}
	steps := map[string]*Step{"render": {State: Completed}, "summary": {State: Running}}
	if s.Ready(steps) {
		t.Fatal("step became ready with an incomplete dependency")
	}
	steps["summary"].State = Completed
	if !s.Ready(steps) {
		t.Fatal("step did not become ready")
	}
}

// Requirement: WFL-002
func TestWorkflowAllowsOnlyDeclaredTransitionsAndFreezesTerminalState(t *testing.T) {
	s := Step{State: Queued}
	if err := s.Transition(Running); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("queued -> running must be illegal: %v", err)
	}
	if err := s.Transition(Reserved); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(Running); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(Verifying); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(Completed); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(Queued); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("terminal state changed: %v", err)
	}
}

// Requirement: WFL-003
func TestManualTakeoverPreservesWorkerExecutionLeaseAndState(t *testing.T) {
	lease := &ExecutionLease{OwnerWorkerID: "worker-1", ExpiresAt: time.Now().Add(time.Minute), FencingToken: 7}
	s := Step{State: Running, ControlMode: ArchivistMode, ExecutionLease: lease}
	if err := s.TakeManualControl(); err != nil {
		t.Fatal(err)
	}
	if s.ControlMode != ManualMode || s.ExecutionLease != lease || s.State != Running {
		t.Fatalf("unexpected takeover result: %+v", s)
	}
}

func TestWorkflowStepRecoversExpiredRunningLeaseWithoutConsumingAttempt(t *testing.T) {
	now := time.Now()
	s := Step{State: Running, ExecutionLease: &ExecutionLease{OwnerWorkerID: "worker-1", ExpiresAt: now.Add(-time.Second)}, Attempt: 1}
	if !s.RecoverExpiredLease(now) || s.State != Queued || s.ExecutionLease != nil || s.Attempt != 1 {
		t.Fatalf("step was not safely recovered: %+v", s)
	}
}

// Requirement: WFL-005
func TestFailureRetriesUntilMaxAttemptsThenFails(t *testing.T) {
	s := Step{State: Running, MaxAttempts: 2}
	if err := s.RecordFailure(Failure{Class: FailureTransient}); err != nil {
		t.Fatal(err)
	}
	if s.State != Queued || s.Attempt != 1 {
		t.Fatalf("first failure: %+v", s)
	}
	s.State = Running
	if err := s.RecordFailure(Failure{Class: FailureTransient}); err != nil {
		t.Fatal(err)
	}
	if s.State != Failed || s.Attempt != 2 {
		t.Fatalf("second failure: %+v", s)
	}
}

// Requirement: WFL-006
func TestApprovalGatedStepCannotRunBeforeApproval(t *testing.T) {
	s := Step{ID: "archive", State: Queued, ApprovalRequired: true, ApprovalRevision: 3}
	if err := s.AuthorizeRun(); !errors.Is(err, ErrApprovalRequired) || s.State != Waiting || s.WaitReason != WaitApproval {
		t.Fatalf("unapproved step was not gated: state=%s err=%v", s.State, err)
	}
	s.Approvals = []Approval{{ID: "APR-1", Sequence: 1, ActorType: vocabulary.ActorHuman, ActorID: "operator-1", TargetID: "archive", TargetRevision: 3, Decision: Approved, Reason: "QC checked", Basis: []string{"QC-1"}, OccurredAt: time.Now()}}
	s.State = Queued
	if err := s.AuthorizeRun(); err != nil {
		t.Fatalf("approved step blocked: %v", err)
	}
	s.ApprovalRevision = 4
	if err := s.AuthorizeRun(); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("stale approval accepted: %v", err)
	}
}

// Requirement: WFL-007
func TestValidExecutionLeaseIsExclusiveAndNewLeaseGetsHigherFencingToken(t *testing.T) {
	now := time.Now()
	s := Step{State: Reserved}
	first, err := s.ClaimExecutionLease("worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimExecutionLease("worker-b", now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrExecutionLeaseHeld) {
		t.Fatalf("overlapping lease accepted: %v", err)
	}
	second, err := s.ClaimExecutionLease("worker-b", now.Add(61*time.Second), time.Minute)
	if err != nil || second <= first {
		t.Fatalf("first=%d second=%d err=%v", first, second, err)
	}
}

// Requirement: WFL-008
func TestWorkerResultRequiresCurrentOwnerAndFencingToken(t *testing.T) {
	now := time.Now()
	s := Step{State: Reserved}
	oldToken, _ := s.ClaimExecutionLease("worker-a", now, time.Second)
	newToken, _ := s.ClaimExecutionLease("worker-b", now.Add(2*time.Second), time.Minute)
	if err := s.AcceptWorkerResult("worker-a", oldToken); !errors.Is(err, ErrStaleFencingToken) {
		t.Fatalf("stale result accepted: %v", err)
	}
	if err := s.AcceptWorkerResult("worker-b", newToken); err != nil {
		t.Fatalf("current result rejected: %v", err)
	}
}

// Requirement: WFL-009
func TestExpiredLeaseReconciliationDoesNotRequeuePublishedArtifact(t *testing.T) {
	now := time.Now()
	s := Step{State: Running, ExecutionLease: &ExecutionLease{OwnerWorkerID: "worker-a", ExpiresAt: now.Add(-time.Second)}}
	if !s.ReconcileExpiredLease(now, true) || s.State != Verifying {
		t.Fatalf("published result was scheduled for duplicate execution: %+v", s)
	}
}

// Requirement: WFL-010
func TestFailureClassificationSeparatesResourceWaitAndPermanentInputFailure(t *testing.T) {
	resourceWait := Step{State: Running, Attempt: 1, MaxAttempts: 3}
	if err := resourceWait.RecordFailure(Failure{Class: FailureResource, WaitReason: WaitDisk}); err != nil {
		t.Fatal(err)
	}
	if resourceWait.State != Waiting || resourceWait.WaitReason != WaitDisk || resourceWait.Attempt != 1 {
		t.Fatalf("resource shortage consumed retry: %+v", resourceWait)
	}
	badInput := Step{State: Running, MaxAttempts: 3}
	if err := badInput.RecordFailure(Failure{Class: FailureInput}); err != nil {
		t.Fatal(err)
	}
	if badInput.State != Failed {
		t.Fatalf("bad input was retried: %+v", badInput)
	}
}

// Requirement: WFL-011
func TestTerminalStepRejectsEveryPublicMutation(t *testing.T) {
	now := time.Now()
	for _, state := range []StepState{Completed, Failed, Cancelled} {
		s := Step{State: state, MaxAttempts: 2}
		if err := s.TakeManualControl(); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s manual mutation accepted: %v", state, err)
		}
		if err := s.RecordFailure(Failure{Class: FailureTransient}); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s failure mutation accepted: %v", state, err)
		}
		if _, err := s.ClaimExecutionLease("worker", now, time.Minute); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s lease mutation accepted: %v", state, err)
		}
	}
}

// Requirement: WFL-012
func TestStateChangingOperationsRespectTransitionTableForEverySourceState(t *testing.T) {
	states := []StepState{Queued, Waiting, Reserved, Running, Verifying, Paused, Failed, Completed, Cancelled}
	for _, state := range states {
		t.Run(string(state)+"/authorize", func(t *testing.T) {
			step := Step{ID: "approval", State: state, ApprovalRequired: true, ApprovalRevision: 1}
			if state == Waiting {
				step.WaitReason = WaitApproval
			}
			err := step.AuthorizeRun()
			allowed := state == Queued || state == Waiting
			if allowed && !errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("allowed source %s returned %v", state, err)
			}
			if !allowed && !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("illegal source %s returned %v", state, err)
			}
		})
		t.Run(string(state)+"/resource-wait", func(t *testing.T) {
			step := Step{State: state}
			err := step.RecordFailure(Failure{Class: FailureResource, WaitReason: WaitScratch})
			if state == Running {
				if err != nil || step.State != Waiting || step.WaitReason != WaitScratch {
					t.Fatalf("running resource wait failed: %+v err=%v", step, err)
				}
			} else if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("illegal source %s returned %v", state, err)
			}
		})
	}
}

// Requirement: WFL-013
func TestLatestApprovalDecisionForRevisionWins(t *testing.T) {
	now := time.Now()
	base := Approval{ID: "APR-1", Sequence: 1, ActorType: vocabulary.ActorHuman, ActorID: "operator-1", TargetID: "archive", TargetRevision: 3, Decision: Approved, Reason: "reviewed", Basis: []string{"QC-1"}, OccurredAt: now}
	step := Step{ID: "archive", State: Queued, ApprovalRequired: true, ApprovalRevision: 3, Approvals: []Approval{base}}
	rejection := base
	rejection.ID = "APR-2"
	rejection.Sequence = 2
	rejection.Decision = Rejected
	rejection.Reason = "manifest changed"
	step.Approvals = append(step.Approvals, rejection)
	if err := step.AuthorizeRun(); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("later rejection did not revoke approval: %v", err)
	}
	reapproval := base
	reapproval.ID = "APR-3"
	reapproval.Sequence = 3
	step.Approvals = append(step.Approvals, reapproval)
	if err := step.AuthorizeRun(); err != nil || step.State != Queued {
		t.Fatalf("latest reapproval did not unlock step: state=%s err=%v", step.State, err)
	}
	duplicateSequence := rejection
	duplicateSequence.ID = "APR-corrupt"
	step.Approvals = append(step.Approvals, duplicateSequence)
	if err := step.AuthorizeRun(); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("duplicate persisted sequence was accepted: %v", err)
	}
}
