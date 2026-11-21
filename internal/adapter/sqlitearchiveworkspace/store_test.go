package sqlitearchiveworkspace_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-001
func TestArchiveWorkspaceProjectsJobStatusInterventionAndOperationalDetailWithoutLedgerMutation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitearchiveworkspace.Open(t.TempDir() + "/archiveworkspace.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	inputs := []archiveworkspace.ProjectionInput{
		archiveWorkspaceInput("JOB-RUNNING", workflow.Running, "", "", archiveworkspace.ToolCallRunning, 0.45, now),
		archiveWorkspaceInput("JOB-WAITING", workflow.Waiting, vocabulary.WaitApproval, "", archiveworkspace.ToolCallCompleted, 0.2, now.Add(time.Minute)),
		archiveWorkspaceInput("JOB-FAILED", workflow.Failed, "", "FFMPEG_EXIT", archiveworkspace.ToolCallFailed, 0.7, now.Add(2*time.Minute)),
	}
	inputs[1].Steps[0].ExecutorKind = vocabulary.ExecutorHuman
	inputs[1].Steps[0].ActorID = "operator-1"
	inputs[1].Timeline = append(inputs[1].Timeline, archiveworkspace.TimelineItem{
		Kind: "HUMAN_COMMAND", Reference: "COMMAND-1", Summary: "approval requested", OccurredAt: now.Add(time.Minute - time.Second/2),
	})
	before := make(map[string]archiveworkspace.JobView, len(inputs))
	for _, input := range inputs {
		view, err := archiveworkspace.Project(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Replace(ctx, view); err != nil {
			t.Fatal(err)
		}
		before[view.Summary.JobID] = view
	}
	stale := inputs[0]
	stale.Job.Revision = 3
	stale.Steps[0].State = workflow.Failed
	stale.Steps[0].ErrorClass = "STALE_PROJECTION"
	staleView, err := archiveworkspace.Project(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(ctx, staleView); err != nil {
		t.Fatal(err)
	}

	query, err := archiveworkspace.NewQueryService(store)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := query.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 3 {
		t.Fatalf("inbox jobs=%d", len(inbox))
	}
	byID := make(map[string]archiveworkspace.JobSummary, len(inbox))
	for _, item := range inbox {
		byID[item.JobID] = item
	}

	running := byID["JOB-RUNNING"]
	if running.CurrentStepID != "STEP-1" ||
		running.StepState != workflow.Running || running.Progress != 0.45 ||
		running.ArchivistAction != "render.create_rendition" || running.ToolCallStatus != archiveworkspace.ToolCallRunning ||
		running.WorkerID != "WORKER-1" || running.Worker == nil || running.Worker.AvailableMemoryBytes != 12<<30 ||
		running.LogReference != "logs/JOB-RUNNING/STEP-1.log" || running.NeedsHumanIntervention {
		t.Fatalf("running job projection is incomplete: %+v", running)
	}
	waiting := byID["JOB-WAITING"]
	if waiting.WaitReason != vocabulary.WaitApproval || waiting.ExecutorKind != vocabulary.ExecutorHuman || waiting.ActorID != "operator-1" ||
		!waiting.NeedsHumanIntervention || waiting.InterventionReason != "APPROVAL_REQUIRED" {
		t.Fatalf("waiting job intervention is wrong: %+v", waiting)
	}
	waitingDetail, err := query.Detail(ctx, "JOB-WAITING")
	if err != nil {
		t.Fatal(err)
	}
	if len(waitingDetail.Timeline) != 3 || waitingDetail.Timeline[1].Kind != "HUMAN_COMMAND" || waitingDetail.Timeline[1].Reference != "COMMAND-1" {
		t.Fatalf("human command is missing from shared timeline: %+v", waitingDetail.Timeline)
	}
	failed := byID["JOB-FAILED"]
	if failed.ErrorClass != "FFMPEG_EXIT" || !failed.NeedsHumanIntervention || failed.InterventionReason != "STEP_FAILED" {
		t.Fatalf("failed job intervention is wrong: %+v", failed)
	}

	detail, err := query.Detail(ctx, "JOB-FAILED")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Steps) != 1 || len(detail.ArchivistActions) != 1 || len(detail.Workers) != 1 || len(detail.Timeline) != 2 ||
		detail.Timeline[1].Kind != "PROCESS_LOG" || detail.Timeline[1].Reference != "logs/JOB-FAILED/STEP-1.log" {
		t.Fatalf("job operational detail is incomplete: %+v", detail)
	}

	for jobID, original := range before {
		after, err := store.Get(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, original) {
			t.Fatalf("archive workspace query mutated %s projection: before=%+v after=%+v", jobID, original, after)
		}
	}
}

func archiveWorkspaceInput(jobID string, state workflow.StepState, waitReason vocabulary.WaitReason, errorClass string,
	toolStatus archiveworkspace.ToolCallStatus, progress float64, occurredAt time.Time,
) archiveworkspace.ProjectionInput {
	logReference := "logs/" + jobID + "/STEP-1.log"
	return archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: jobID, Revision: 4, TemplateName: "OccasionRecording", TargetTitle: "Office Event",
			CurrentStepID: "STEP-1",
		},
		Steps: []archiveworkspace.StepStatus{{
			ID: "STEP-1", State: state, Progress: progress,
			ExecutorKind: vocabulary.ExecutorWorker, ActorID: "archivist-1", WorkerID: "WORKER-1",
			WaitReason: waitReason, ErrorClass: errorClass, LogReference: logReference, LastEventAt: occurredAt,
		}},
		ArchivistActions: []archiveworkspace.ArchivistAction{{
			Action: "render.create_rendition", ToolCallID: "TOOL-1", ToolCallStatus: toolStatus, OccurredAt: occurredAt.Add(-time.Second),
		}},
		Workers: []archiveworkspace.WorkerResource{{
			WorkerID: "WORKER-1", CPUPercent: 20, GPUPercent: 10,
			AvailableMemoryBytes: 12 << 30, AvailableVRAMBytes: 6 << 30, ScratchFreeBytes: 200 << 30,
			HeavyRunning: 1, AcceptingWork: true, AccessibleStorageIDs: []string{"STORAGE-1"}, HeartbeatAt: occurredAt,
		}},
		Timeline: []archiveworkspace.TimelineItem{
			{Kind: "ARCHIVIST_TOOL_CALL", Reference: "TOOL-1", Summary: string(toolStatus), OccurredAt: occurredAt.Add(-time.Second)},
			{Kind: "PROCESS_LOG", Reference: logReference, Summary: errorClass, OccurredAt: occurredAt},
		},
	}
}
