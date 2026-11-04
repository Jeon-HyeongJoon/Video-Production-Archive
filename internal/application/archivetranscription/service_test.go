package archivetranscription

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: APP-008
func TestStartCreatesOneIdempotentTranscriptionJobPerStoredRecordingInReadyArchiveRun(t *testing.T) {
	now := time.Date(2026, 10, 31, 13, 0, 0, 0, time.UTC)
	store, err := sqlitecommand.Open(filepath.Join(t.TempDir(), "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	archiveRunHandler, err := archiverun.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	archiveRunCommands := command.NewAuthorizedCreatingExecutor(store, func(command.Envelope) error { return nil }, archiveRunHandler.Handle)
	if _, err := archiveRunCommands.Execute(command.Envelope{
		CommandID: "COMMAND-ARCHIVE-RUN-1", CommandType: archiverun.CreateCommand,
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, RequestedVia: "HTTP",
		TargetID: "ARCHIVE-RUN-1", ExpectedRevision: 0, IdempotencyKey: "archiveRun:create:1", Reason: "selected archive run",
		Parameters: map[string]any{
			"worker_id": "WORKER-1", "source_storage_id": "SOURCE-1", "source_volume_id": "VOLUME-1",
			"source_physical_device_id": "PHYSICAL-1", "archive_storage_id": "ARCHIVE-1",
			"folder_discovery_revision": uint64(7),
			"folder_ids":                []string{"as-is", "render", "sequence", "hold", "failed"},
			"selected_by":               map[string]any{"type": vocabulary.ActorHuman, "id": "operator-1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	jobHandler, err := job.NewHandler(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	jobCommands := command.NewAuthorizedCreatingExecutor(store, func(command.Envelope) error { return nil }, jobHandler.Handle)
	projection := fixtureStageOneProjection{
		folders: []archiverun.FolderProgress{
			{FolderID: "as-is", Disposition: vocabulary.OrganizationAsIs, StageOne: workflow.Completed, Stored: true},
			{FolderID: "render", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Completed, Stored: true},
			{FolderID: "sequence", Disposition: vocabulary.OrganizationCompileSourceSequence, StageOne: workflow.Completed, Stored: true},
			{FolderID: "hold", Disposition: vocabulary.OrganizationHold, StageOne: workflow.Waiting},
			{FolderID: "failed", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Failed},
		},
		recordings: []StoredRecording{
			storedRecording("render", "ASSET-2", "DECISION-2", vocabulary.OrganizationRenderRecordings, "VP-20261031-0002", "b"),
			storedRecording("as-is", "ASSET-1", "DECISION-1", vocabulary.OrganizationAsIs, "VP-20261031-0001", "a"),
		},
	}
	service, err := New(store, projection, jobCommands)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := New(store, fixtureStageOneProjection{folders: []archiverun.FolderProgress{
		{FolderID: "as-is", Disposition: vocabulary.OrganizationAsIs, StageOne: workflow.Running},
		{FolderID: "render", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Completed, Stored: true},
		{FolderID: "sequence", Disposition: vocabulary.OrganizationCompileSourceSequence, StageOne: workflow.Completed, Stored: true},
		{FolderID: "hold", Disposition: vocabulary.OrganizationHold, StageOne: workflow.Waiting},
		{FolderID: "failed", Disposition: vocabulary.OrganizationRenderRecordings, StageOne: workflow.Failed},
	}}, jobCommands)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Start(context.Background(), StartRequest{
		ArchiveRunID: "ARCHIVE-RUN-1", ExpectedArchiveRunRevision: 1, IdempotencyKey: "archiveRun:stt:blocked",
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, StartedAt: now,
	}); !errors.Is(err, ErrStageOneIncomplete) {
		t.Fatalf("running stage one did not reject STT start: %v", err)
	}
	if snapshots, err := store.ListSnapshots(); err != nil || len(snapshots) != 1 {
		t.Fatalf("rejected STT start created partial Jobs: snapshots=%+v err=%v", snapshots, err)
	}
	request := StartRequest{
		ArchiveRunID: "ARCHIVE-RUN-1", ExpectedArchiveRunRevision: 1, IdempotencyKey: "archiveRun:stt:1",
		Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, StartedAt: now,
	}
	first, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArchiveRunID != "ARCHIVE-RUN-1" || first.EligibleCount != 2 || first.HoldCount != 1 ||
		len(first.JobIDs) != 2 || len(second.JobIDs) != 2 || first.JobIDs[0] != second.JobIDs[0] || first.JobIDs[1] != second.JobIDs[1] {
		t.Fatalf("unexpected transcription result: first=%+v second=%+v", first, second)
	}
	snapshots, err := store.ListSnapshots()
	if err != nil || len(snapshots) != 3 {
		t.Fatalf("expected one archiveRun and two idempotent STT Jobs, snapshots=%+v err=%v", snapshots, err)
	}
	for _, jobID := range first.JobIDs {
		snapshot, err := store.Load(jobID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if state.TemplateName != "archive-transcription" || state.CurrentStepID != "audio" || len(state.Steps) != 6 {
			t.Fatalf("stage two was not a separate transcription Job: %+v", state)
		}
		var audioSpec *execution.StepExecutionSpec
		for index := range state.Steps {
			if state.Steps[index].Workflow.ID == "audio" {
				audioSpec = state.Steps[index].ExecutionSpec
			}
		}
		if audioSpec == nil || audioSpec.Validate() != nil || audioSpec.Inputs[0].StorageID != "ARCHIVE-1" ||
			audioSpec.Inputs[0].RelativePath[:9] != "packages/" || audioSpec.Parameters.ArchivePackageID == "" ||
			audioSpec.Parameters.ArchivePackageRevision != 1 || audioSpec.Parameters.WorkspaceDriveID != "VOLUME-1" {
			t.Fatalf("unsafe or incomplete first stage-two execution spec: %+v", audioSpec)
		}
		for _, step := range state.Steps {
			if len(step.PhysicalDeviceIDs) != 1 || step.PhysicalDeviceIDs[0] != "PHYSICAL-1" {
				t.Fatalf("catalog-serializing device reservation context is missing: %+v", step)
			}
		}
	}
}

type fixtureStageOneProjection struct {
	folders    []archiverun.FolderProgress
	recordings []StoredRecording
}

func (fixture fixtureStageOneProjection) StageOne(_ context.Context, _ archiverun.State) ([]archiverun.FolderProgress, []StoredRecording, error) {
	return append([]archiverun.FolderProgress(nil), fixture.folders...), append([]StoredRecording(nil), fixture.recordings...), nil
}

func storedRecording(folder, originalID, decisionID string, disposition vocabulary.OrganizationDisposition, packageID, checksumByte string) StoredRecording {
	checksum := ""
	for len(checksum) < 64 {
		checksum += checksumByte
	}
	return StoredRecording{
		FolderID: folder, WorkspaceDriveID: "VOLUME-1", OriginalAssetID: originalID,
		OrganizationDecisionID: decisionID, Disposition: disposition,
		ArchivePackageID: packageID, ArchivePackageRevision: 1,
		VideoRelativePath: "packages/" + packageID + "/revisions/000001/video.mp4", VideoChecksum: checksum,
		SelectedProfileReference: "organization-1080p-6000k@v1",
		SourceInputs: []execution.InputReference{{
			OriginalAssetID: originalID, OriginalName: originalID + ".mp4", StorageID: "SOURCE-1",
			RelativePath: folder + "/" + originalID + ".mp4", Checksum: checksum,
			DurationMillis: 60_000, VideoCodec: "h264", Width: 1920, Height: 1080,
			VideoBitRateBPS: 6_000_000, HasAudio: true, ProfileReference: "organization-1080p-6000k@v1",
		}},
	}
}
