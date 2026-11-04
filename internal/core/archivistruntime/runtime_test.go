package archivistruntime

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

func testSession(t *testing.T) Session {
	t.Helper()
	session, err := NewSession(SessionSpec{
		ID: "SESSION-1", ArchivistHostID: "host-a", Backend: vocabulary.RuntimeEmbeddedCore,
		CLIID: "codex", WorkspaceRootID: "video-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func leasedTurn(t *testing.T, now time.Time) (Turn, TurnLease) {
	t.Helper()
	turn, err := NewTurn("TURN-1", "SESSION-1", "REQUEST-1")
	if err != nil {
		t.Fatal(err)
	}
	turn, lease, err := ClaimTurn(turn, "host-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return turn, lease
}

// Requirement: LRH-002
func TestPinnedSessionWaitsForConfiguredHostWithoutReroute(t *testing.T) {
	session := testSession(t)
	before := session
	_, err := SelectExecutionHost(session, map[string]vocabulary.ArchivistHostState{
		"host-a": vocabulary.ArchivistHostOffline,
		"host-b": vocabulary.ArchivistHostOnline,
	})
	if !errors.Is(err, ErrPinnedHostUnavailable) {
		t.Fatalf("got %v, want pinned host unavailable", err)
	}
	if session != before {
		t.Fatalf("host selection mutated pinned session: before=%+v after=%+v", before, session)
	}
}

// Requirement: LRH-003
func TestArchivistTurnLeaseRejectsStaleFencingReports(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	turn, leaseA := leasedTurn(t, now)
	if _, _, err := ClaimTurn(turn, "host-b", now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrTurnLeaseHeld) {
		t.Fatalf("second active lease was not rejected: %v", err)
	}
	turn, leaseB, err := ClaimTurn(turn, "host-b", now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if leaseB.FencingToken <= leaseA.FencingToken {
		t.Fatalf("fencing token did not increase: old=%d new=%d", leaseA.FencingToken, leaseB.FencingToken)
	}
	if err := ValidateTurnReport(turn, leaseA.ArchivistHostID, leaseA.FencingToken, now.Add(2*time.Minute)); !errors.Is(err, ErrStaleTurnLease) {
		t.Fatalf("stale host report accepted: %v", err)
	}
	if err := ValidateTurnReport(turn, leaseB.ArchivistHostID, leaseB.FencingToken, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("current host report rejected: %v", err)
	}
}

// Requirement: LRH-004
func TestArchivistTurnDeliverySeparatesAcceptedFromStartedAndDeduplicatesRequest(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	turn, lease := leasedTurn(t, now)
	var deliver bool
	var err error
	turn, deliver, err = PrepareTurnDelivery(turn, lease.ArchivistHostID, lease.FencingToken, now, "REQUEST-1")
	if err != nil || !deliver {
		t.Fatalf("first delivery not requested: deliver=%v err=%v", deliver, err)
	}
	turn, deliver, err = PrepareTurnDelivery(turn, lease.ArchivistHostID, lease.FencingToken, now, "REQUEST-1")
	if err != nil || deliver {
		t.Fatalf("duplicate delivery was not suppressed: deliver=%v err=%v", deliver, err)
	}
	turn, _, err = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{
		Cursor: 1, RequestID: "REQUEST-1", Kind: RuntimeInputAccepted,
	})
	if err != nil || turn.State != vocabulary.ArchivistTurnSubmitted {
		t.Fatalf("input acceptance state=%s err=%v", turn.State, err)
	}
	turn, _, err = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{
		Cursor: 2, RequestID: "REQUEST-1", Kind: RuntimeTurnStarted,
	})
	if err != nil || turn.State != vocabulary.ArchivistTurnRunning {
		t.Fatalf("turn start state=%s err=%v", turn.State, err)
	}
}

// Requirement: LRH-005
func TestRuntimeRebindUsesDurableSessionInsteadOfStaleHandle(t *testing.T) {
	session := testSession(t)
	previous := RuntimeBinding{SessionID: session.ID, ResumeReference: "resume-1", Handle: "terminal-old"}
	discovered := RuntimeBinding{SessionID: session.ID, ResumeReference: "resume-1", Handle: "terminal-new"}
	rebound, err := RebindRuntime(session, previous, discovered)
	if err != nil || rebound.Handle != "terminal-new" || rebound.Handle == previous.Handle {
		t.Fatalf("runtime did not rebind to discovery: rebound=%+v err=%v", rebound, err)
	}
	wrong := discovered
	wrong.ResumeReference = "resume-other"
	if _, err := RebindRuntime(session, previous, wrong); !errors.Is(err, ErrRuntimeBindingMismatch) {
		t.Fatalf("mismatched resume reference accepted: %v", err)
	}
}

// Requirement: LRH-006
func TestResumePlanPrefersNativeReferenceAndReplaysCompletedToolCalls(t *testing.T) {
	calls := []CheckpointToolCall{
		{ID: "CALL-DONE", IdempotencyKey: "tool-done", Completed: true, ResultSummary: "asset found"},
		{ID: "CALL-PENDING", IdempotencyKey: "tool-pending"},
	}
	native, err := PlanResume("find source", "catalog ready", "resume-native", calls)
	if err != nil || native.Mode != ResumeNative || native.NativeResumeReference != "resume-native" || native.Checkpoint != nil {
		t.Fatalf("native resume was not preferred: plan=%+v err=%v", native, err)
	}
	fallback, err := PlanResume("find source", "catalog ready", "", calls)
	if err != nil || fallback.Mode != ResumeCheckpoint || fallback.Checkpoint == nil {
		t.Fatalf("checkpoint was not built: plan=%+v err=%v", fallback, err)
	}
	if fallback.Checkpoint.CompletedResults["CALL-DONE"] != "asset found" ||
		!reflect.DeepEqual(fallback.Checkpoint.PendingToolCalls, []PendingToolCall{{ID: "CALL-PENDING", IdempotencyKey: "tool-pending"}}) {
		t.Fatalf("checkpoint re-executes completed calls or loses pending calls: %+v", fallback.Checkpoint)
	}
}

// Requirement: LRH-007
func TestRuntimeLaunchPlanUsesOnlyAllowlistedExecutableAndWorkspace(t *testing.T) {
	session := testSession(t)
	executable := filepath.Join(t.TempDir(), "codex")
	workspace := t.TempDir()
	policy := LaunchPolicy{
		CLIs:           map[string]CLIExecutable{"codex": {Executable: executable, FixedArguments: []string{"--resume"}}},
		WorkspaceRoots: map[string]string{"video-project": workspace},
	}
	plan, err := BuildLaunchPlan(session, policy)
	if err != nil || plan.Executable != executable || plan.WorkingDirectory != workspace ||
		!reflect.DeepEqual(plan.Arguments, []string{"--resume"}) {
		t.Fatalf("allowlisted launch plan mismatch: plan=%+v err=%v", plan, err)
	}
	unsafe := session
	unsafe.CLIID = "codex; /bin/sh"
	if _, err := BuildLaunchPlan(unsafe, policy); !errors.Is(err, ErrUnsafeRuntimeExecution) {
		t.Fatalf("unregistered shell-like CLI accepted: %v", err)
	}
	unsafe = session
	unsafe.WorkspaceRootID = "/tmp/arbitrary"
	if _, err := BuildLaunchPlan(unsafe, policy); !errors.Is(err, ErrUnsafeRuntimeExecution) {
		t.Fatalf("arbitrary workspace accepted: %v", err)
	}
}

// Requirement: LRH-008
func TestRuntimeEventCursorReplaysOnceAndRequiresExplicitCompletion(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	turn, lease := leasedTurn(t, now)
	turn, _, _ = PrepareTurnDelivery(turn, lease.ArchivistHostID, lease.FencingToken, now, "REQUEST-1")
	turn, _, _ = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{Cursor: 1, RequestID: "REQUEST-1", Kind: RuntimeInputAccepted})
	turn, _, _ = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{Cursor: 2, RequestID: "REQUEST-1", Kind: RuntimeTurnStarted})
	turn, applied, err := ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{Cursor: 3, RequestID: "REQUEST-1", Kind: RuntimeTerminalIdle})
	if err != nil || !applied || turn.State != vocabulary.ArchivistTurnRunning {
		t.Fatalf("terminal idle changed final state: state=%s applied=%v err=%v", turn.State, applied, err)
	}
	before := turn
	turn, applied, err = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{Cursor: 3, RequestID: "REQUEST-1", Kind: RuntimeTerminalIdle})
	if err != nil || applied || turn != before {
		t.Fatalf("duplicate cursor was applied: before=%+v after=%+v applied=%v err=%v", before, turn, applied, err)
	}
	turn, applied, err = ReduceRuntimeEvent(turn, lease.ArchivistHostID, lease.FencingToken, now, RuntimeEvent{
		Cursor: 4, RequestID: "REQUEST-1", Kind: RuntimeTurnCompleted,
		FinalResponse: &FinalResponse{Summary: "turn completed", ResultIDs: []string{"JOB-1"}},
	})
	if err != nil || !applied || turn.State != vocabulary.ArchivistTurnCompleted {
		t.Fatalf("explicit completion was not applied: state=%s applied=%v err=%v", turn.State, applied, err)
	}
	failed, failedLease := leasedTurn(t, now)
	failed, _, _ = PrepareTurnDelivery(failed, failedLease.ArchivistHostID, failedLease.FencingToken, now, "REQUEST-1")
	failed, _, _ = ReduceRuntimeEvent(failed, failedLease.ArchivistHostID, failedLease.FencingToken, now, RuntimeEvent{Cursor: 1, RequestID: "REQUEST-1", Kind: RuntimeInputAccepted})
	failed, _, _ = ReduceRuntimeEvent(failed, failedLease.ArchivistHostID, failedLease.FencingToken, now, RuntimeEvent{Cursor: 2, RequestID: "REQUEST-1", Kind: RuntimeTurnStarted})
	failure := FailureReport{Code: "RUNTIME_FAILED", Summary: "turn failed"}
	failed, applied, err = ReduceRuntimeEvent(failed, failedLease.ArchivistHostID, failedLease.FencingToken, now, RuntimeEvent{
		Cursor: 3, RequestID: "REQUEST-1", Kind: RuntimeTurnFailed, Failure: &failure,
	})
	if err != nil || !applied || failed.State != vocabulary.ArchivistTurnFailed {
		t.Fatalf("explicit failure was not applied: state=%s applied=%v err=%v", failed.State, applied, err)
	}
}

func TestResumedDeliveredTurnContinuesCursorAndRequiresStructuredCompletion(t *testing.T) {
	now := time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)
	turn, firstLease := leasedTurn(t, now)
	turn, _, _ = PrepareTurnDelivery(turn, firstLease.ArchivistHostID, firstLease.FencingToken, now, "REQUEST-1")
	turn, _, _ = ReduceRuntimeEvent(turn, firstLease.ArchivistHostID, firstLease.FencingToken, now, RuntimeEvent{
		Cursor: 1, RequestID: "REQUEST-1", Kind: RuntimeInputAccepted,
	})
	turn, _, _ = ReduceRuntimeEvent(turn, firstLease.ArchivistHostID, firstLease.FencingToken, now, RuntimeEvent{
		Cursor: 2, RequestID: "REQUEST-1", Kind: RuntimeTurnStarted,
	})

	turn, resumedLease, err := ClaimTurn(turn, "host-a", now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	turn, applied, err := ReduceRuntimeEvent(turn, resumedLease.ArchivistHostID, resumedLease.FencingToken, now.Add(2*time.Minute), RuntimeEvent{
		Cursor: 3, RequestID: "REQUEST-1", Kind: RuntimeTurnStarted,
	})
	if err != nil || !applied || turn.State != vocabulary.ArchivistTurnRunning {
		t.Fatalf("resumed start state=%s applied=%v err=%v", turn.State, applied, err)
	}
	if _, _, err := ReduceRuntimeEvent(turn, resumedLease.ArchivistHostID, resumedLease.FencingToken, now.Add(2*time.Minute), RuntimeEvent{
		Cursor: 4, RequestID: "REQUEST-1", Kind: RuntimeTurnCompleted,
	}); !errors.Is(err, ErrInvalidRuntimeEvent) {
		t.Fatalf("completion without structured response was accepted: %v", err)
	}
	response := FinalResponse{Summary: "재개 작업 완료", ResultIDs: []string{"JOB-1", "ASSET-1"}}
	turn, applied, err = ReduceRuntimeEvent(turn, resumedLease.ArchivistHostID, resumedLease.FencingToken, now.Add(2*time.Minute), RuntimeEvent{
		Cursor: 4, RequestID: "REQUEST-1", Kind: RuntimeTurnCompleted, FinalResponse: &response,
	})
	if err != nil || !applied || turn.State != vocabulary.ArchivistTurnCompleted || !reflect.DeepEqual(turn.FinalResponse, &response) {
		t.Fatalf("structured completion was not stored: turn=%+v applied=%v err=%v", turn, applied, err)
	}
}
