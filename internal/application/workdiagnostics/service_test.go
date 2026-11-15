package workdiagnostics

import (
	"context"
	"testing"
	"time"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type fixedWorkReader struct{}

func (fixedWorkReader) Detail(context.Context, string) (archivework.View, error) {
	return archivework.View{}, archivework.ErrWorkNotFound
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

func hasFact(values []Fact, code string) bool {
	for _, value := range values {
		if value.Code == code {
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
