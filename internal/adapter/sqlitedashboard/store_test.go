package sqlitedashboard_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitedashboard"
	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-001
func TestDashboardProjectsJobStatusInterventionAndOperationalDetailWithoutLedgerMutation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitedashboard.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	inputs := []dashboard.ProjectionInput{
		dashboardInput("JOB-RUNNING", workflow.Running, "", "", dashboard.ToolCallRunning, 0.45, now),
		dashboardInput("JOB-WAITING", workflow.Waiting, vocabulary.WaitApproval, "", dashboard.ToolCallCompleted, 0.2, now.Add(time.Minute)),
		dashboardInput("JOB-FAILED", workflow.Failed, "", "FFMPEG_EXIT", dashboard.ToolCallFailed, 0.7, now.Add(2*time.Minute)),
	}
	before := make(map[string]dashboard.JobView, len(inputs))
	for _, input := range inputs {
		view, err := dashboard.Project(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Replace(ctx, view); err != nil {
			t.Fatal(err)
		}
		before[view.Summary.JobID] = view
	}

	query, err := dashboard.NewQueryService(store)
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
	byID := make(map[string]dashboard.JobSummary, len(inbox))
	for _, item := range inbox {
		byID[item.JobID] = item
	}

	running := byID["JOB-RUNNING"]
	if running.ControlMode != workflow.AgentMode || running.CurrentStepID != "STEP-1" ||
		running.StepState != workflow.Running || running.Progress != 0.45 ||
		running.AgentAction != "render.create_rendition" || running.ToolCallStatus != dashboard.ToolCallRunning ||
		running.WorkerID != "WORKER-1" || running.Worker == nil || running.Worker.AvailableMemoryBytes != 12<<30 ||
		running.LogReference != "logs/JOB-RUNNING/STEP-1.log" || running.NeedsHumanIntervention {
		t.Fatalf("running job projection is incomplete: %+v", running)
	}
	waiting := byID["JOB-WAITING"]
	if waiting.WaitReason != vocabulary.WaitApproval || !waiting.NeedsHumanIntervention || waiting.InterventionReason != "APPROVAL_REQUIRED" {
		t.Fatalf("waiting job intervention is wrong: %+v", waiting)
	}
	failed := byID["JOB-FAILED"]
	if failed.ErrorClass != "FFMPEG_EXIT" || !failed.NeedsHumanIntervention || failed.InterventionReason != "STEP_FAILED" {
		t.Fatalf("failed job intervention is wrong: %+v", failed)
	}

	detail, err := query.Detail(ctx, "JOB-FAILED")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Steps) != 1 || len(detail.AgentActions) != 1 || len(detail.Workers) != 1 || len(detail.Timeline) != 2 ||
		detail.Timeline[1].Kind != "PROCESS_LOG" || detail.Timeline[1].Reference != "logs/JOB-FAILED/STEP-1.log" {
		t.Fatalf("job operational detail is incomplete: %+v", detail)
	}

	for jobID, original := range before {
		after, err := store.Get(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, original) {
			t.Fatalf("dashboard query mutated %s projection: before=%+v after=%+v", jobID, original, after)
		}
	}
}

func dashboardInput(jobID string, state workflow.StepState, waitReason vocabulary.WaitReason, errorClass string,
	toolStatus dashboard.ToolCallStatus, progress float64, occurredAt time.Time,
) dashboard.ProjectionInput {
	logReference := "logs/" + jobID + "/STEP-1.log"
	return dashboard.ProjectionInput{
		Job: dashboard.Job{
			ID: jobID, Revision: 4, TemplateName: "OccasionRecording", TargetTitle: "Office Event",
			ControlMode: workflow.AgentMode, CurrentStepID: "STEP-1",
		},
		Steps: []dashboard.StepStatus{{
			ID: "STEP-1", State: state, Progress: progress,
			ExecutorKind: vocabulary.ExecutorWorker, ActorID: "agent-1", WorkerID: "WORKER-1",
			WaitReason: waitReason, ErrorClass: errorClass, LogReference: logReference, LastEventAt: occurredAt,
		}},
		AgentActions: []dashboard.AgentAction{{
			Action: "render.create_rendition", ToolCallID: "TOOL-1", ToolCallStatus: toolStatus, OccurredAt: occurredAt.Add(-time.Second),
		}},
		Workers: []dashboard.WorkerResource{{
			WorkerID: "WORKER-1", CPUPercent: 20, GPUPercent: 10,
			AvailableMemoryBytes: 12 << 30, AvailableVRAMBytes: 6 << 30, ScratchFreeBytes: 200 << 30,
			HeavyRunning: 1, AcceptingWork: true, AccessibleStorageIDs: []string{"STORAGE-1"}, HeartbeatAt: occurredAt,
		}},
		Timeline: []dashboard.TimelineItem{
			{Kind: "AGENT_TOOL_CALL", Reference: "TOOL-1", Summary: string(toolStatus), OccurredAt: occurredAt.Add(-time.Second)},
			{Kind: "PROCESS_LOG", Reference: logReference, Summary: errorClass, OccurredAt: occurredAt},
		},
	}
}
