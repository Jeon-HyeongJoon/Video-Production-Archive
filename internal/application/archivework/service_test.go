package archivework

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-028
func TestArchiveWorkGroupsFixedFolderJobsUnderOneArchiveRunAndKeepsStandaloneJob(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	run := archiverun.State{
		ID: "ARCHIVE-RUN-1", Revision: 1, WorkerID: "worker-windows", SourceStorageID: "source-e",
		SourceVolumeID: "volume-e", SourcePhysicalDeviceID: "disk-e", ArchiveStorageID: "archive-local",
		StartIdempotencyKey: "run-1", FolderDiscoveryRevision: 7, FolderIDs: []string{"event-a", "event-b"},
		SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "operator-local"},
		StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
		CreatedAt:       now,
	}
	inspect := linkedJob("JOB-INSPECT", "archive-intake-inspection", "volume-e", "ARCHIVE-RUN-1", "", workflow.Completed, now.Add(time.Minute))
	folder := linkedJob("JOB-FOLDER-A", "archive-intake-as-is", "event-a", "ARCHIVE-RUN-1", "event-a", workflow.Running, now.Add(2*time.Minute))
	standalone := linkedJob("JOB-STANDALONE", "occasion-recording", "행사 기록", "", "", workflow.Queued, now.Add(3*time.Minute))
	ledger := fixedSnapshots{snapshots: []command.Snapshot{snapshotOf(t, run.ID, run.Revision, run), snapshotOf(t, inspect.ID, inspect.Revision, inspect), snapshotOf(t, folder.ID, folder.Revision, folder), snapshotOf(t, standalone.ID, standalone.Revision, standalone)}}
	jobs := fixedJobs{summaries: []archiveworkspace.JobSummary{
		summaryOf(inspect, workflow.Completed, 1), summaryOf(folder, workflow.Running, 0.4), summaryOf(standalone, workflow.Queued, 0),
	}}
	drives := fixedDrives{views: []archiveintakeworkspace.DriveView{{
		WorkspaceDriveID: "volume-e", ArchiveRunID: run.ID, WorkerID: run.WorkerID, ArchiveRunState: "STAGE_ONE_RUNNING",
		HoldCount: 1, Folders: []archiveintakeworkspace.FolderView{{Folder: "event-a", VideoCount: 3}, {Folder: "event-b", VideoCount: 2}},
	}}}
	service, err := New(ledger, jobs, drives)
	if err != nil {
		t.Fatal(err)
	}

	values, err := service.Inbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].WorkID != run.ID || values[0].Kind != ArchiveRunWork || values[0].State != StateAttention ||
		values[0].FolderCount != 2 || values[0].VideoCount != 5 || values[0].HoldCount != 1 || values[0].ChildJobCount != 2 ||
		values[1].WorkID != standalone.ID || values[1].Kind != StandaloneJob {
		t.Fatalf("unexpected grouped work projection: %+v", values)
	}
	detail, err := service.Detail(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail.FolderIDs, []string{"event-a", "event-b"}) || len(detail.Children) != 2 ||
		detail.Children[0].Role != ChildInspection || detail.Children[1].Role != ChildFolder || detail.Children[1].Folder != "event-a" {
		t.Fatalf("ArchiveRun did not retain its fixed scope and child Jobs: %+v", detail)
	}
}

// Requirement: DSH-029
func TestArchiveWorkRebuildsCurrentArchiveRunFromPersistedDriveProjection(t *testing.T) {
	now := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	folder := linkedJob("JOB-FOLDER-A", "archive-intake-render-recordings", "event-a", "ARCHIVE-RUN-IMPORTED", "event-a", workflow.Running, now)
	standalone := linkedJob("JOB-STANDALONE", "occasion-recording", "행사 기록", "", "", workflow.Queued, now.Add(time.Minute))
	ledger := fixedSnapshots{snapshots: []command.Snapshot{
		snapshotOf(t, folder.ID, folder.Revision, folder), snapshotOf(t, standalone.ID, standalone.Revision, standalone),
	}}
	jobs := fixedJobs{summaries: []archiveworkspace.JobSummary{summaryOf(folder, workflow.Running, 0.5), summaryOf(standalone, workflow.Queued, 0)}}
	drives := fixedDrives{views: []archiveintakeworkspace.DriveView{{
		WorkspaceDriveID: "volume-e", ArchiveRunID: "ARCHIVE-RUN-IMPORTED", WorkerID: "worker-windows",
		SourceVolumeID: "volume-e", SourceStorageID: "source-e", ArchiveStorageID: "archive-local",
		ArchiveRunState: "STAGE_ONE_RUNNING", FolderCount: 2,
		Folders: []archiveintakeworkspace.FolderView{{Folder: "event-a", VideoCount: 3}, {Folder: "event-b", VideoCount: 2}},
	}}}
	service, err := New(ledger, jobs, drives)
	if err != nil {
		t.Fatal(err)
	}

	values, err := service.Inbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].WorkID != "ARCHIVE-RUN-IMPORTED" || values[0].Kind != ArchiveRunWork ||
		values[0].FolderCount != 2 || values[0].VideoCount != 5 || values[0].ChildJobCount != 1 ||
		values[1].WorkID != standalone.ID || values[1].Kind != StandaloneJob {
		t.Fatalf("current ArchiveRun was flattened after projection rebuild: %+v", values)
	}
	detail, err := service.Detail(context.Background(), "ARCHIVE-RUN-IMPORTED")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail.FolderIDs, []string{"event-a", "event-b"}) || len(detail.Children) != 1 || detail.Children[0].Job.JobID != folder.ID {
		t.Fatalf("rebuilt ArchiveRun lost fixed scope or child Job: %+v", detail)
	}
}

func linkedJob(id, template, title, runID, folder string, state workflow.StepState, at time.Time) job.State {
	var spec *execution.StepExecutionSpec
	if runID != "" {
		spec = &execution.StepExecutionSpec{Parameters: execution.StepExecutionParameters{ArchiveRunID: runID, MediaFolder: folder}}
	}
	return job.State{
		ID: id, Revision: 1, TemplateName: template, TargetTitle: title, ControlMode: workflow.AssistedMode, CurrentStepID: "step",
		Steps: []job.Step{{Workflow: workflow.Step{ID: "step", State: state}, ExecutionSpec: spec, LastEventAt: at}},
	}
}

func summaryOf(state job.State, stepState workflow.StepState, progress float64) archiveworkspace.JobSummary {
	return archiveworkspace.JobSummary{
		JobID: state.ID, Revision: state.Revision, TemplateName: state.TemplateName, TargetTitle: state.TargetTitle,
		ControlMode: state.ControlMode, CurrentStepID: state.CurrentStepID, StepState: stepState, Progress: progress,
		ExecutorKind: vocabulary.ExecutorWorker, WorkerID: "worker-windows", LastEventAt: state.Steps[0].LastEventAt,
	}
}

func snapshotOf(t *testing.T, id string, revision uint64, state any) command.Snapshot {
	t.Helper()
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return command.Snapshot{TargetID: id, Revision: revision, State: payload}
}

type fixedSnapshots struct{ snapshots []command.Snapshot }

func (store fixedSnapshots) ListSnapshots() ([]command.Snapshot, error) {
	return append([]command.Snapshot(nil), store.snapshots...), nil
}

type fixedJobs struct{ summaries []archiveworkspace.JobSummary }

func (reader fixedJobs) Inbox(context.Context) ([]archiveworkspace.JobSummary, error) {
	return append([]archiveworkspace.JobSummary(nil), reader.summaries...), nil
}

func (reader fixedJobs) Detail(_ context.Context, jobID string) (archiveworkspace.JobView, error) {
	for _, summary := range reader.summaries {
		if summary.JobID == jobID {
			return archiveworkspace.JobView{Summary: summary}, nil
		}
	}
	return archiveworkspace.JobView{}, archiveworkspace.ErrJobNotFound
}

type fixedDrives struct {
	views []archiveintakeworkspace.DriveView
}

func (reader fixedDrives) Projection(context.Context) ([]archiveintakeworkspace.DriveView, error) {
	return append([]archiveintakeworkspace.DriveView(nil), reader.views...), nil
}
