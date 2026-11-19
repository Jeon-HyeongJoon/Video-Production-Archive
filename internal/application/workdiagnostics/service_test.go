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

type fixedWorkReader struct{}

func (fixedWorkReader) Detail(context.Context, string) (archivework.View, error) {
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
	for _, forbidden := range []string{`C:\private`, "token-secret", "password", "credential"} {
		if strings.Contains(strings.ToLower(string(payload)), strings.ToLower(forbidden)) {
			t.Fatalf("diagnostic exposed raw process data: %s", payload)
		}
	}
}

type fixedJobReader struct{ view archiveworkspace.JobView }

func (reader fixedJobReader) Detail(context.Context, string) (archiveworkspace.JobView, error) {
	return reader.view, nil
}

type fixedWorkerReader struct{ values []worker.Status }

func (reader fixedWorkerReader) Statuses(context.Context) ([]worker.Status, error) {
	return reader.values, nil
}

type fixedExecutionReader struct{ value ExecutionObservation }

func (reader fixedExecutionReader) ActiveExecution(context.Context, string, string) (ExecutionObservation, bool, error) {
	return reader.value, true, nil
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
