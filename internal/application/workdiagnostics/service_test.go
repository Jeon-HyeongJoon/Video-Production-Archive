package workdiagnostics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/execution"
	corejob "videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type fixedWorkReader struct{ view archivework.View }

func (r fixedWorkReader) Detail(_ context.Context, workID string) (archivework.View, error) {
	if r.view.Summary.WorkID == workID {
		return r.view, nil
	}
	return archivework.View{}, archivework.ErrWorkNotFound
}

// Requirement: OBS-003
func TestWorkDiagnosticsExposePersistedFailureEvidenceAsFactsWithoutRawProcessData(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	exitCode := 1
	view := archiveworkspace.JobView{
		Summary: archiveworkspace.JobSummary{
			JobID: "JOB-FAILED", Revision: 11, CurrentStepID: "organize", StepState: workflow.Failed,
			ErrorClass: "STEP_EXECUTION_FAILED", LastEventAt: now.Add(-time.Minute),
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "organize", State: workflow.Failed, LastEventAt: now.Add(-time.Minute),
			ExecutorKind: vocabulary.ExecutorWorker,
			FailureEvidence: []corejob.StepFailureEvidence{
				{EvidenceID: "EVD-WORKER-FIRST", StepID: "organize", Operation: execution.OperationOrganizeMedia,
					FailureSequence: 1, WorkflowAttempt: 1, WorkerID: "WORKER-1", FencingToken: 4,
					FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED", CauseCode: "ORGANIZE_MEDIA_FAILED",
					Summary: "Archive media organization operation failed", ObservedAt: now.Add(-3 * time.Minute), ResultingState: workflow.Queued},
				{EvidenceID: "EVD-WORKER-SECOND", StepID: "organize", Operation: execution.OperationOrganizeMedia,
					FailureSequence: 2, WorkflowAttempt: 2, WorkerID: "WORKER-1", FencingToken: 5,
					FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED", CauseCode: "PROCESS_EXIT_NONZERO",
					Summary: `C:\private\token-secret.log`, ProcessExitCode: &exitCode, LogReference: `C:\private\token-secret.log`,
					ObservedAt: now.Add(-time.Minute), ResultingState: workflow.Failed},
			},
		}},
	}
	service, err := New(
		fixedWorkReader{}, fixedJobReader{view: view}, fixedWorkerReader{},
		fixedExecutionReader{}, func() time.Time { return now }, 10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := service.Diagnose(context.Background(), archivistruntime.WorkReference{
		Kind: archivistruntime.WorkJob, WorkID: "JOB-FAILED", Revision: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !diagnostic.ShouldReport || len(diagnostic.Jobs) != 1 || len(diagnostic.Jobs[0].FailureEvidence) != 2 ||
		diagnostic.Jobs[0].FailureEvidence[0].EvidenceID != "EVD-WORKER-FIRST" ||
		diagnostic.Jobs[0].FailureEvidence[1].ProcessExitCode == nil || *diagnostic.Jobs[0].FailureEvidence[1].ProcessExitCode != exitCode ||
		!hasFactEvidence(diagnostic.Facts, "WORKER_FAILURE_RECORDED", "EVD-WORKER-FIRST") ||
		!hasFactEvidence(diagnostic.Facts, "WORKER_FAILURE_RECORDED", "EVD-WORKER-SECOND") {
		t.Fatalf("persisted failure evidence was not projected as facts: %+v", diagnostic)
	}
	payload, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("cancelled run retains its child's previous failure", func(t *testing.T) {
		stopped := view
		stopped.Summary.StepState, stopped.Summary.ErrorClass = workflow.Cancelled, ""
		work := fixedWorkReader{view: archivework.View{
			Summary: archivework.Summary{WorkID: "RUN-CANCELLED", State: archivework.StateCancelled},
			Children: []archivework.ChildJob{
				{Job: archiveworkspace.JobSummary{JobID: "inspection", StepState: workflow.Completed}},
				{Job: stopped.Summary},
			},
		}}
		svc, err := New(work, fixedJobReader{view: stopped}, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Diagnose(context.Background(), archivistruntime.WorkReference{Kind: archivistruntime.WorkArchiveRun, WorkID: work.view.Summary.WorkID, Revision: 1})
		if err != nil || len(got.Jobs) != 1 || got.Jobs[0].StepState != workflow.Cancelled || !hasFactEvidence(got.Facts, "WORKER_FAILURE_RECORDED", "EVD-WORKER-SECOND") {
			t.Fatalf("cancelled work lost recorded failure evidence: %+v, %v", got, err)
		}
	})
	for _, forbidden := range []string{`C:\private`, "token-secret", "password", "credential"} {
		if strings.Contains(strings.ToLower(string(payload)), strings.ToLower(forbidden)) {
			t.Fatalf("diagnostic exposed raw process data: %s", payload)
		}
	}
	t.Run("short path recovery requires the latest matching failure and verified audio", func(t *testing.T) {
		failed := view.Steps[0].FailureEvidence[1]
		failed.Operation, failed.StepID, failed.CauseCode = execution.OperationTranscribeAudio, "transcript", execution.WhisperLongOutputOpenFailed
		view.Summary.TemplateName, view.Summary.CurrentStepID = "archive-transcription", failed.StepID
		view.Steps = []archiveworkspace.StepStatus{{ID: "audio", State: workflow.Completed}, {ID: failed.StepID, State: workflow.Failed, FailureEvidence: []corejob.StepFailureEvidence{failed}}}
		ref := archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: view.Summary.JobID, Revision: view.Summary.Revision}
		read := func(inbox []archiveworkspace.JobSummary) Snapshot {
			t.Helper()
			reader, err := New(fixedWorkReader{}, fixedJobReader{view: view, inbox: inbox}, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := reader.Diagnose(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			return snapshot
		}
		if len(read(nil).RecoveryActions) != 0 {
			t.Fatal("unverified audio produced a recovery action")
		}
		view.Steps[0].HasVerificationReferences = true
		available := read(nil)
		if len(available.RecoveryActions) != 1 || available.RecoveryActions[0].ActionCode != corejob.RecoveryActionRetryTranscriptionShortPath ||
			available.RecoveryActions[0].Basis[0] != failed.EvidenceID || !hasFact(available.Facts, "RECOVERY_AUDIO_VERIFIED") {
			t.Fatalf("missing recovery basis: %+v", available)
		}
		recovery := archiveworkspace.JobSummary{JobID: available.RecoveryActions[0].RecoveryJobID, Revision: 17, CurrentStepID: "archive_catalog", StepState: workflow.Completed, LastEventAt: now, RecoverySource: &corejob.RecoverySource{FailureEvidenceID: failed.EvidenceID, JobID: ref.WorkID}}
		after := read([]archiveworkspace.JobSummary{recovery})
		if len(after.RecoveryActions) != 0 || after.Jobs[0].RecoveryJob == nil || after.Jobs[0].RecoveryJob.State != workflow.Completed || after.Jobs[0].RecoveryJob.FailureEvidenceID != failed.EvidenceID {
			t.Fatalf("missing linked result: %+v", after)
		}
		other := failed
		other.FailureSequence++
		other.CauseCode = "PROCESS_EXIT_NONZERO"
		other.EvidenceID = "EVD-NEWER"
		// Sequence, independent of observation time, selects the latest settlement.
		other.ObservedAt = failed.ObservedAt.Add(-time.Second)
		view.Steps[1].FailureEvidence = append(view.Steps[1].FailureEvidence, other)
		if len(read(nil).RecoveryActions) != 0 {
			t.Fatal("an older classified failure authorized a retry")
		}
	})

}

// Requirement: OBS-004
func TestWorkDiagnosticsExposeAvailableDetailAndMarkLegacyReferenceUnresolvable(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	exitCode := 22
	artifact := &execution.ProcessLogArtifact{
		ID: "PLOG-A1B2C3", Reference: "evidence://process-log/PLOG-A1B2C3", State: execution.ProcessLogAvailable,
		Digest: "sha256:process-log", CapturedBytes: 140, TotalBytes: 240, Truncated: true,
		SanitizedTail: "decoder rejected the input stream\ntoken=should-not-leave-control",
	}
	failure := corejob.StepFailureEvidence{
		EvidenceID: "EVD-WORKER-PROCESS", StepID: "render", Operation: execution.OperationRenderRecording,
		FailureSequence: 1, WorkflowAttempt: 1, WorkerID: "WORKER-1", FencingToken: 8,
		FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED", CauseCode: "PROCESS_EXIT_NONZERO",
		Summary: "Render process failed", ProcessExitCode: &exitCode, LogReference: artifact.Reference, ProcessLog: artifact,
		ObservedAt: now.Add(-time.Minute), ResultingState: workflow.Failed,
	}
	view := archiveworkspace.JobView{
		Summary: archiveworkspace.JobSummary{JobID: "JOB-PROCESS", Revision: 5, CurrentStepID: "render", StepState: workflow.Failed,
			ErrorClass: failure.ErrorClass, LastEventAt: failure.ObservedAt},
		Steps: []archiveworkspace.StepStatus{{ID: "render", State: workflow.Failed, FailureEvidence: []corejob.StepFailureEvidence{failure}}},
	}
	service, err := New(fixedWorkReader{}, fixedJobReader{view: view}, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reference := archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-PROCESS", Revision: 5}
	diagnostic, err := service.Diagnose(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	projected := diagnostic.Jobs[0].FailureEvidence[0]
	if projected.ProcessLog == nil || projected.ProcessLog.State != execution.ProcessLogAvailable || projected.ProcessLog.SanitizedTail != "" ||
		!hasSuggestedAction(diagnostic.SuggestedActions, "INSPECT_FAILURE_DETAIL") {
		t.Fatalf("available detail was not projected safely: %+v", diagnostic)
	}
	detail, err := service.FailureEvidence(context.Background(), reference, failure.EvidenceID)
	if err != nil || detail.CauseCode != failure.CauseCode || detail.WorkflowAttempt != failure.WorkflowAttempt || detail.Summary != failure.Summary || detail.ProcessLog.State != execution.ProcessLogAvailable ||
		!strings.Contains(detail.ProcessLog.SanitizedTail, "decoder rejected") || strings.Contains(strings.ToLower(detail.ProcessLog.SanitizedTail), "should-not-leave") {
		t.Fatalf("failure detail was not independently sanitized: detail=%+v err=%v", detail, err)
	}

	legacy := failure
	legacy.EvidenceID = "EVD-WORKER-LEGACY"
	legacy.LogReference = "worker://operation-failed"
	legacy.ProcessLog = nil
	view.Steps[0].FailureEvidence = []corejob.StepFailureEvidence{legacy}
	service, _ = New(fixedWorkReader{}, fixedJobReader{view: view}, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, 10*time.Minute)
	diagnostic, err = service.Diagnose(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	projected = diagnostic.Jobs[0].FailureEvidence[0]
	if projected.ProcessLog == nil || projected.ProcessLog.State != execution.ProcessLogLegacyUnresolvable ||
		hasSuggestedAction(diagnostic.SuggestedActions, "INSPECT_FAILURE_DETAIL") || !hasSuggestedAction(diagnostic.SuggestedActions, "FAILURE_DETAIL_UNAVAILABLE") {
		t.Fatalf("legacy reference was presented as resolvable: %+v", diagnostic)
	}
}

type fixedJobReader struct {
	view  archiveworkspace.JobView
	inbox []archiveworkspace.JobSummary
}

func (reader fixedJobReader) Detail(context.Context, string) (archiveworkspace.JobView, error) {
	return reader.view, nil
}

func (reader fixedJobReader) Inbox(context.Context) ([]archiveworkspace.JobSummary, error) {
	return append([]archiveworkspace.JobSummary(nil), reader.inbox...), nil
}

type fixedWorkerReader struct{ values []worker.Status }

func (reader fixedWorkerReader) Statuses(context.Context) ([]worker.Status, error) {
	return reader.values, nil
}

type fixedExecutionReader struct{ value ExecutionObservation }

func (reader fixedExecutionReader) ActiveExecution(context.Context, string, string) (ExecutionObservation, bool, error) {
	return reader.value, reader.value.ReservationID != "", nil
}

// Requirement: OBS-002
func TestWorkDiagnosticsCorrelatesStepWorkerLeaseReservationAndSeparatesFactsFromHypotheses(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	view := archiveworkspace.JobView{Summary: archiveworkspace.JobSummary{
		JobID: "JOB-1", Revision: 7, CurrentStepID: "render", StepState: workflow.Running,
		Progress: 0.4, WorkerID: "WORKER-1", LastEventAt: now.Add(-20 * time.Minute),
		LogReference: `C:\private\token.txt`,
	}}
	execution := ExecutionObservation{
		ReservationID: "RES-1", WorkerID: "WORKER-1", ResourceClass: vocabulary.ResourceHeavy,
		MemoryBytes: 1024, ScratchBytes: 2048, ReservedAt: now.Add(-30 * time.Minute), HasLease: true,
		LeaseAcquiredAt: now.Add(-30 * time.Minute), LeaseHeartbeatAt: now.Add(-10 * time.Second),
		LeaseExpiresAt: now.Add(time.Minute), FencingToken: 3,
	}
	service, err := New(
		fixedWorkReader{}, fixedJobReader{view: view},
		fixedWorkerReader{values: []worker.Status{{
			WorkerID: "WORKER-1", State: "BUSY", LastHeartbeat: now.Add(-5 * time.Second),
			Capacity: worker.Capacity{AvailableMemoryBytes: 4096, ScratchFreeBytes: 8192},
		}}},
		fixedExecutionReader{value: execution}, func() time.Time { return now }, 10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := service.Diagnose(context.Background(), archivistruntime.WorkReference{
		Kind: archivistruntime.WorkJob, WorkID: "JOB-1", Revision: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !diagnostic.ShouldReport || len(diagnostic.Jobs) != 1 || diagnostic.Jobs[0].StepState != workflow.Running ||
		diagnostic.Jobs[0].LogReference != "" || diagnostic.Jobs[0].Execution == nil ||
		diagnostic.Jobs[0].Execution.ReservationID != "RES-1" || diagnostic.Jobs[0].Worker == nil ||
		diagnostic.Jobs[0].Worker.State != "BUSY" {
		t.Fatalf("unexpected diagnostic: %+v", diagnostic)
	}
	if !hasFact(diagnostic.Facts, "LEASE_HEARTBEAT_OBSERVED") || !hasHypothesis(diagnostic.Hypotheses, "PROGRESS_UNOBSERVED") ||
		len(diagnostic.Fingerprint) != 24 {
		t.Fatalf("facts and hypotheses were not separated: %+v", diagnostic)
	}
	if view.Summary.StepState != workflow.Running || view.Summary.WaitReason != "" {
		t.Fatalf("diagnostics mutated the persisted observation: %+v", view.Summary)
	}
}

func TestWorkDiagnosticsExposeStructuredProgressEvidenceAsFacts(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	progress := corejob.StepProgressEvidence{
		EvidenceID: "EVD-PROGRESS-STT", StepID: "transcript", WorkerID: "WORKER-1", FencingToken: 9,
		ReportSequence: 4, ObservedAt: now.Add(-time.Minute), Update: execution.ProgressUpdate{
			Operation: execution.OperationTranscribeAudio, Status: execution.ProgressCompleted,
			ChunkIndex: 2, ChunkCount: 5, SourceStartMillis: 720_000, SourceEndMillis: 1_440_000, Progress: 0.4,
		},
	}
	view := archiveworkspace.JobView{
		Summary: archiveworkspace.JobSummary{
			JobID: "JOB-STT", Revision: 9, CurrentStepID: "transcript", StepState: workflow.Running,
			Progress: 0.4, WorkerID: "WORKER-1", LastEventAt: progress.ObservedAt,
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "transcript", State: workflow.Running, Progress: 0.4, WorkerID: "WORKER-1",
			LastEventAt: progress.ObservedAt, ProgressEvidence: []corejob.StepProgressEvidence{progress},
		}},
	}
	lease := ExecutionObservation{
		ReservationID: "RES-STT", WorkerID: "WORKER-1", ResourceClass: vocabulary.ResourceHeavy,
		ReservedAt: now.Add(-20 * time.Minute), HasLease: true, LeaseAcquiredAt: now.Add(-20 * time.Minute),
		LeaseHeartbeatAt: now.Add(-10 * time.Second), LeaseExpiresAt: now.Add(time.Minute), FencingToken: 9,
	}
	service, err := New(
		fixedWorkReader{}, fixedJobReader{view: view}, fixedWorkerReader{values: []worker.Status{{
			WorkerID: "WORKER-1", State: "BUSY", LastHeartbeat: now.Add(-5 * time.Second),
			Metrics: worker.Metrics{HeavyRunning: 1}, Capacity: worker.Capacity{AvailableMemoryBytes: 4096, ScratchFreeBytes: 8192},
		}}}, fixedExecutionReader{value: lease}, func() time.Time { return now }, 10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := service.Diagnose(context.Background(), archivistruntime.WorkReference{
		Kind: archivistruntime.WorkJob, WorkID: "JOB-STT", Revision: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostic.Jobs) != 1 || len(diagnostic.Jobs[0].ProgressEvidence) != 1 ||
		diagnostic.Jobs[0].ProgressEvidence[0] != progress ||
		!hasFactEvidence(diagnostic.Facts, "WORKER_PROGRESS_RECORDED", progress.EvidenceID) ||
		hasHypothesis(diagnostic.Hypotheses, "PROGRESS_UNOBSERVED") {
		t.Fatalf("structured STT progress was not exposed as a confirmed fact: %+v", diagnostic)
	}
}

func TestWorkDiagnosticsOffersOneReplacementForTransientTerminalArchivePublicationFailure(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	failure := corejob.StepFailureEvidence{
		EvidenceID: "EVD-PACKAGE-FAILED", StepID: "archive_package_revision",
		Operation: execution.OperationReviseArchivePackage, FailureSequence: 3, WorkflowAttempt: 3,
		WorkerID: "WORKER-1", FencingToken: 13, FailureClass: workflow.FailureTransient,
		ErrorClass: "STEP_EXECUTION_FAILED", CauseCode: "REVISE_ARCHIVE_PACKAGE_FAILED",
		Summary: "Archive package revision operation failed", ObservedAt: now.Add(-time.Minute), ResultingState: workflow.Failed,
	}
	view := archiveworkspace.JobView{
		Summary: archiveworkspace.JobSummary{JobID: "JOB-STT-FAILED", Revision: 17, CurrentStepID: failure.StepID,
			StepState: workflow.Failed, ErrorClass: failure.ErrorClass, LastEventAt: failure.ObservedAt},
		Steps: []archiveworkspace.StepStatus{{ID: failure.StepID, State: workflow.Failed, ExecutorKind: vocabulary.ExecutorWorker,
			LastEventAt: failure.ObservedAt, FailureEvidence: []corejob.StepFailureEvidence{failure}}},
	}
	service, err := New(fixedWorkReader{}, fixedJobReader{view: view}, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := service.Diagnose(context.Background(), archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-STT-FAILED", Revision: 17})
	if err != nil || len(diagnostic.RecoveryActions) != 1 {
		t.Fatalf("terminal archive publication did not produce one recovery action: diagnostic=%+v err=%v", diagnostic, err)
	}
	action := diagnostic.RecoveryActions[0]
	if action.ActionCode != corejob.RecoveryActionRetryTerminalArchivePublication || action.Basis[0] != failure.EvidenceID ||
		action.FencingToken != failure.FencingToken || action.RecoveryJobID != corejob.TerminalRecoveryJobID("JOB-STT-FAILED", failure.EvidenceID) {
		t.Fatalf("recovery action was not bound to terminal failure evidence: %+v", action)
	}

	reader := fixedJobReader{view: view, inbox: []archiveworkspace.JobSummary{{
		JobID: action.RecoveryJobID, Revision: 1, LastEventAt: now,
		RecoverySource: &corejob.RecoverySource{FailureEvidenceID: failure.EvidenceID},
	}}}
	service, err = New(fixedWorkReader{}, reader, fixedWorkerReader{}, fixedExecutionReader{}, func() time.Time { return now }, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err = service.Diagnose(context.Background(), archivistruntime.WorkReference{Kind: archivistruntime.WorkJob, WorkID: "JOB-STT-FAILED", Revision: 17})
	if err != nil || len(diagnostic.RecoveryActions) != 0 || !hasFact(diagnostic.Facts, "TERMINAL_RECOVERY_JOB_CREATED") {
		t.Fatalf("existing recovery Job did not suppress duplicate action: diagnostic=%+v err=%v", diagnostic, err)
	}
}

func hasFact(values []Fact, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}

func hasFactEvidence(values []Fact, code, evidenceID string) bool {
	for _, value := range values {
		if value.Code == code && value.EvidenceID == evidenceID {
			return true
		}
	}
	return false
}

func hasHypothesis(values []Hypothesis, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}

func hasSuggestedAction(values []SuggestedAction, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}
