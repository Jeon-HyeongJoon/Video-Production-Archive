package archivework

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
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

// Requirement: DSH-036
func TestArchiveWorkMarksFailedRunHandledByEarliestReplacementScope(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	failedRun := archiverun.State{
		ID: "ARCHIVE-RUN-FAILED", Revision: 1, WorkerID: "worker-windows", SourceStorageID: "source-e",
		SourceVolumeID: "volume-e", SourcePhysicalDeviceID: "disk-e", ArchiveStorageID: "archive-local",
		StartIdempotencyKey: "failed-run", FolderDiscoveryRevision: 7, FolderIDs: []string{"event-a"},
		SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "operator-local"},
		StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}}, CreatedAt: now,
	}
	replacementRun := failedRun
	replacementRun.ID, replacementRun.StartIdempotencyKey, replacementRun.CreatedAt = "ARCHIVE-RUN-REPLACEMENT", "replacement-run", now.Add(time.Hour)
	laterRun := replacementRun
	laterRun.ID, laterRun.StartIdempotencyKey, laterRun.CreatedAt = "ARCHIVE-RUN-LATER", "later-run", now.Add(2*time.Hour)
	failed := linkedJob("JOB-FAILED", "archive-intake-as-is", "event-a", failedRun.ID, "event-a", workflow.Failed, now.Add(10*time.Minute))
	completed := linkedJob("JOB-REPLACEMENT", "archive-intake-as-is", "event-a", replacementRun.ID, "event-a", workflow.Completed, now.Add(70*time.Minute))
	later := linkedJob("JOB-LATER", "archive-intake-as-is", "event-a", laterRun.ID, "event-a", workflow.Completed, now.Add(130*time.Minute))
	snapshots := []command.Snapshot{
		snapshotOf(t, failedRun.ID, failedRun.Revision, failedRun), snapshotOf(t, replacementRun.ID, replacementRun.Revision, replacementRun),
		snapshotOf(t, laterRun.ID, laterRun.Revision, laterRun), snapshotOf(t, failed.ID, failed.Revision, failed),
		snapshotOf(t, completed.ID, completed.Revision, completed), snapshotOf(t, later.ID, later.Revision, later),
	}
	jobs := fixedJobs{summaries: []archiveworkspace.JobSummary{
		{JobID: failed.ID, Revision: 1, TemplateName: failed.TemplateName, TargetTitle: failed.TargetTitle, CurrentStepID: "organize", StepState: workflow.Failed, ErrorClass: "STEP_EXECUTION_FAILED", LogReference: "worker://operation-failed", LastEventAt: now.Add(10 * time.Minute)},
		summaryOf(completed, workflow.Completed, 1), summaryOf(later, workflow.Completed, 1),
	}}
	service, err := New(fixedSnapshots{snapshots: snapshots}, jobs, fixedDrives{})
	if err != nil {
		t.Fatal(err)
	}

	values, err := service.Inbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found Summary
	for _, value := range values {
		if value.WorkID == failedRun.ID {
			found = value
		}
	}
	if found.State != StateFailed || found.ReplacementWorkID != replacementRun.ID {
		t.Fatalf("failed outcome or earliest replacement link was lost: %+v", found)
	}
	detail, err := service.Detail(context.Background(), failedRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Failures) != 1 || detail.Failures[0].Folder != "event-a" || detail.Failures[0].StepID != "organize" ||
		detail.Failures[0].ErrorClass != "STEP_EXECUTION_FAILED" || detail.Failures[0].LogReference != "worker://operation-failed" {
		t.Fatalf("parent work lost structured child failure evidence: %+v", detail.Failures)
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

// Requirement: DSH-030
func TestArchiveWorkReadsPersistedSourceVideoFactsForOneJob(t *testing.T) {
	now := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	state := linkedJob("JOB-FOLDER-MEDIA", "archive-intake-render-recordings", "event-a", "ARCHIVE-RUN-MEDIA", "event-a", workflow.Completed, now)
	inputs := []execution.InputReference{
		{
			OriginalAssetID: "ASSET-2", OriginalName: "B002.mov", StorageID: "source-e", RelativePath: "event-a/B002.mov",
			Checksum: strings.Repeat("b", 64), SourceCreatedAt: now.Add(-time.Hour), DurationMillis: 120_000,
			SourceSizeBytes: 240_000_000, SourceContainer: "mov", SourceBitRateBPS: 16_000_000,
			VideoCodec: "h264", Width: 1920, Height: 1080, VideoBitRateBPS: 15_000_000,
			AudioCodec: "aac", AudioBitRateBPS: 256_000, AudioSampleRateHz: 48_000, AudioChannels: 2,
		},
		{
			OriginalAssetID: "ASSET-1", StorageID: "source-e", RelativePath: "event-a/A001.mp4",
			Checksum: strings.Repeat("a", 64), SourceCreatedAt: now.Add(-2 * time.Hour), DurationMillis: 60_000,
			VideoCodec: "h264", Width: 1920, Height: 1080, VideoBitRateBPS: 5_700_000,
		},
	}
	state.Steps[0].ExecutionSpec = &execution.StepExecutionSpec{Parameters: execution.StepExecutionParameters{ArchiveRunID: "ARCHIVE-RUN-MEDIA", SourceInputs: inputs}}
	state.Steps = append(state.Steps, job.Step{ExecutionSpec: &execution.StepExecutionSpec{Parameters: execution.StepExecutionParameters{ArchiveRunID: "ARCHIVE-RUN-MEDIA", SourceInputs: inputs}}})
	inspection := linkedJob("JOB-INSPECTION-MEDIA", "archive-intake-inspection", "volume-e", "ARCHIVE-RUN-MEDIA", "", workflow.Completed, now)
	drives := fixedDrives{views: []archiveintakeworkspace.DriveView{{
		ArchiveRunID: "ARCHIVE-RUN-MEDIA", Folders: []archiveintakeworkspace.FolderView{{
			Folder: "event-a", ClassificationAssets: []archiveintakeworkspace.ClassificationAssetView{{
				OriginalAssetID: "ASSET-1", RelativePath: "event-a/A001.mp4", Checksum: strings.Repeat("a", 64),
				DurationMillis: 60_000, SizeBytes: 45_000_000, BitRate: 6_000_000, Containers: []string{"mp4"},
				VideoStreams: []mediaanalysis.VideoStream{{Codec: "h264", Width: 1920, Height: 1080, BitRate: 5_700_000}},
				AudioStreams: []mediaanalysis.AudioStream{{Codec: "aac", BitRate: 192_000, SampleRateHz: 48_000, Channels: 2}},
			}},
		}},
	}}}
	service, err := New(fixedSnapshots{snapshots: []command.Snapshot{
		snapshotOf(t, state.ID, state.Revision, state), snapshotOf(t, inspection.ID, inspection.Revision, inspection),
	}}, fixedJobs{}, drives)
	if err != nil {
		t.Fatal(err)
	}

	media, err := service.Media(context.Background(), state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 2 || media[0].OriginalAssetID != "ASSET-1" || media[0].OriginalName != "A001.mp4" ||
		media[0].RelativePath != "event-a/A001.mp4" || media[0].DurationMillis != 60_000 || media[0].SizeBytes != 45_000_000 ||
		media[0].Container != "mp4" || media[0].VideoCodec != "h264" || media[0].Width != 1920 || media[0].Height != 1080 ||
		media[0].VideoBitRateBPS != 5_700_000 || media[0].AudioCodec != "aac" {
		t.Fatalf("persisted source video facts were not projected without duplicates: %+v", media)
	}
	inspected, err := service.Media(context.Background(), inspection.ID)
	if err != nil || len(inspected) != 1 || inspected[0].OriginalName != "A001.mp4" || inspected[0].SizeBytes != 45_000_000 ||
		inspected[0].Container != "mp4" || inspected[0].VideoCodec != "h264" || inspected[0].AudioCodec != "aac" {
		t.Fatalf("inspection Job did not reuse persisted ArchiveRun observations: media=%+v err=%v", inspected, err)
	}
}

func linkedJob(id, template, title, runID, folder string, state workflow.StepState, at time.Time) job.State {
	var spec *execution.StepExecutionSpec
	if runID != "" {
		spec = &execution.StepExecutionSpec{Parameters: execution.StepExecutionParameters{ArchiveRunID: runID, MediaFolder: folder}}
	}
	return job.State{
		ID: id, Revision: 1, TemplateName: template, TargetTitle: title, CurrentStepID: "step",
		Steps: []job.Step{{Workflow: workflow.Step{ID: "step", State: state}, ExecutionSpec: spec, LastEventAt: at}},
	}
}

func summaryOf(state job.State, stepState workflow.StepState, progress float64) archiveworkspace.JobSummary {
	return archiveworkspace.JobSummary{
		JobID: state.ID, Revision: state.Revision, TemplateName: state.TemplateName, TargetTitle: state.TargetTitle,
		CurrentStepID: state.CurrentStepID, StepState: stepState, Progress: progress,
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

// Requirement: DSH-056
func TestCancelledWorkReportsPublishedPackagesAndPreservesUnfinishedChildren(t *testing.T) {
	const runID = "ARCHIVE-RUN-CANCEL"
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	inspection := linkedJob("inspection", "archive-intake-inspection", "drive", runID, "", workflow.Completed, now)
	stopped := linkedJob("stopped", "archive-intake-render-recordings", "stage", runID, "stage", workflow.Cancelled, now)
	standalone := linkedJob("standalone", "occasion-recording", "stage", "", "", workflow.Cancelled, now)
	for _, tc := range []struct {
		name    string
		sibling workflow.StepState
		results int
		want    State
	}{
		{"cancelled before publication", "", 0, StateCancelled},
		{"already published sibling", workflow.Completed, 1, StateCancelled},
		{"another folder running", workflow.Running, 0, StateActive},
		{"another folder queued", workflow.Queued, 0, StateQueued},
		{"unresolved sibling failure", workflow.Failed, 0, StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := []job.State{inspection, stopped, standalone}
			if tc.sibling != "" {
				states = append(states, linkedJob("sibling", "archive-intake-render-recordings", "other", runID, "other", tc.sibling, now))
			}
			var ledger fixedSnapshots
			var jobs fixedJobs
			for _, state := range states {
				ledger.snapshots = append(ledger.snapshots, snapshotOf(t, state.ID, state.Revision, state))
				jobs.summaries = append(jobs.summaries, summaryOf(state, state.Steps[0].Workflow.State, 0))
			}
			drive := archiveintakeworkspace.DriveView{ArchiveRunID: runID, ArchiveRunState: "COMPLETED"}
			if tc.results > 0 {
				drive.CompletedWork = []archiveintakeworkspace.CompletedWorkView{{ArchiveRunID: runID, JobID: "sibling", ArchivePackageID: "published"}}
			}
			service, err := New(ledger, jobs, fixedDrives{views: []archiveintakeworkspace.DriveView{drive}})
			if err != nil {
				t.Fatal(err)
			}
			run, err := service.Detail(context.Background(), runID)
			if err != nil || run.Summary.State != tc.want || run.Summary.ResultCount != tc.results || len(run.CompletedWork) != tc.results {
				t.Fatalf("projected run %+v, results %+v, err %v", run.Summary, run.CompletedWork, err)
			}
			single, err := service.Detail(context.Background(), standalone.ID)
			if err != nil || single.Summary.State != StateCancelled {
				t.Fatalf("standalone %+v, err %v", single.Summary, err)
			}
		})
	}
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

// Requirement: DSH-052
func TestArchiveRunTitlesUseOnlyCommonEventInPinnedFolders(t *testing.T) {
	const event = "37년도/# 1. 교회 촬영물/370105-남양주-주일정오예배_촬영-본당"
	for _, tc := range []struct {
		name    string
		folders []string
		want    string
	}{
		{"one event", []string{event + "/CAM1", event + "/CAM2"}, "남양주 주일정오예배"},
		{"another event", []string{"37년도/370106-신년-행사/PGM"}, "신년 행사"},
		{"mixed events", []string{event, "37년도/370106-신년-행사"}, ""},
		{"missing event", []string{event, "37년도/CAM1"}, ""},
		{"ambiguous date", []string{event + "/370106-신년-행사"}, ""},
		{"different roots", []string{event, "복사/" + event}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := archiverun.State{ID: "RUN", Revision: 1, WorkerID: "WORKER", SourceStorageID: "SOURCE", SourceVolumeID: "VOLUME", SourcePhysicalDeviceID: "DISK", ArchiveStorageID: "DEST",
				StartIdempotencyKey: "start", FolderDiscoveryRevision: 2, FolderIDs: tc.folders, CreatedAt: time.Now().UTC(),
				SelectedBy: command.Actor{Type: vocabulary.ActorHuman, ID: "operator"}, StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}}}
			legacy := archiveintakeworkspace.DriveView{ArchiveRunID: "LEGACY", SourceVolumeID: "VOLUME"}
			for _, folder := range tc.folders {
				legacy.Folders = append(legacy.Folders, archiveintakeworkspace.FolderView{Folder: folder})
			}
			partial := legacy
			partial.ArchiveRunID = "PARTIAL"
			partial.FolderCount = len(tc.folders) + 1
			service, err := New(fixedSnapshots{[]command.Snapshot{snapshotOf(t, run.ID, 1, run)}}, fixedJobs{}, fixedDrives{[]archiveintakeworkspace.DriveView{legacy, partial}})
			if err != nil {
				t.Fatal(err)
			}
			summaries, err := service.Inbox(context.Background())
			if err != nil || len(summaries) != 3 {
				t.Fatalf("list: %+v %v", summaries, err)
			}
			for _, summary := range summaries {
				want := tc.want
				if summary.WorkID == "PARTIAL" {
					want = ""
				}
				detail, err := service.Detail(context.Background(), summary.WorkID)
				if err != nil || summary.Title != want || detail.Summary.Title != want || !reflect.DeepEqual(detail.FolderIDs, tc.folders) {
					t.Fatalf("%s: title %q detail %+v, want %q (%v)", summary.WorkID, summary.Title, detail, want, err)
				}
			}
		})
	}
}
