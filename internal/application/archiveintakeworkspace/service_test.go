package archiveintakeworkspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

// Requirement: DSH-008
func TestArchiveRunProjectionPreservesFixedStageWaitReasonsAndArchiveStates(t *testing.T) {
	const driveID = "VOLUME-BULK-1"
	files := make([]organization.CoordinatedFile, 240)
	for index := range files {
		files[index] = organization.CoordinatedFile{
			RelativePath:  fmt.Sprintf("recordings/clip-%04d.mov", index+1),
			ObservationID: fmt.Sprintf("OBS-%04d", index+1), OriginalAssetID: fmt.Sprintf("ASSET-%04d", index+1),
			InspectionID: fmt.Sprintf("INSPECTION-%04d", index+1),
		}
	}
	renderDecision := organization.OrganizationDecision{
		ID: "DECISION-RENDER", Sequence: 1, WorkspaceDriveID: driveID, Folder: "recordings", InputRevision: 9,
		Disposition: vocabulary.OrganizationRenderRecordings, ProfileReference: "archive-1080p-6mbps@v1",
	}
	interviewDecision := organization.OrganizationDecision{
		ID: "DECISION-INTERVIEW", Sequence: 1, WorkspaceDriveID: driveID, Folder: "interviews", InputRevision: 9,
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1",
	}
	holdInput := func(id, relativePath string, frameRate mediaanalysis.Rational) organization.Input {
		return organization.Input{
			OriginalAssetID: id, RelativePath: relativePath,
			Inspection: mediaanalysis.MediaInspection{
				OriginalAssetID: id, DurationMillis: 12_000,
				VideoStreams: []mediaanalysis.VideoStream{{
					Index: 0, Codec: "h264", Width: 3840, Height: 2160, FrameRate: frameRate, BitRate: 50_000_000,
				}},
				AudioStreams: []mediaanalysis.AudioStream{{Index: 1, Codec: "pcm_s16be", SampleRateHz: 48_000, Channels: 2}},
			},
		}
	}
	holdDecision := organization.OrganizationDecision{
		ID: "DECISION-HOLD", Sequence: 1, WorkspaceDriveID: driveID, Folder: "unclassified", InputRevision: 9,
		Disposition: vocabulary.OrganizationHold, HoldReasons: []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
		Inputs: []organization.Input{
			holdInput("ASSET-HOLD-1", "unclassified/C0001.MP4", mediaanalysis.Rational{Numerator: 30, Denominator: 1}),
			holdInput("ASSET-HOLD-3", "unclassified/C0003.MP4", mediaanalysis.Rational{Numerator: 120, Denominator: 1}),
		},
	}
	deviceDecision := organization.OrganizationDecision{
		ID: "DECISION-DEVICE", Sequence: 1, WorkspaceDriveID: driveID, Folder: "device-wait", InputRevision: 9,
		Disposition: vocabulary.OrganizationRenderRecordings, ProfileReference: "archive-1080p-20mbps@v1",
	}
	committedDecision := organization.OrganizationDecision{
		ID: "DECISION-COMMITTED", Sequence: 1, WorkspaceDriveID: driveID, Folder: "committed-no-stt", InputRevision: 9,
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1",
	}
	store := workstationStore{
		receipts: []organization.ArchiveOrganizationReceipt{{
			ID: "RUN-9", SessionID: "INTAKE-9", InputRevision: 9, WorkspaceDriveID: driveID, WorkerID: "WORKER-WINDOWS",
			Files: files, Folders: []organization.CoordinatedFolder{
				{Folder: "recordings", Decision: renderDecision, JobID: "JOB-RENDER"},
				{Folder: "interviews", Decision: interviewDecision, JobID: "JOB-INTERVIEW"},
				{Folder: "unclassified", Decision: holdDecision},
				{Folder: "device-wait", Decision: deviceDecision, JobID: "JOB-DEVICE"},
				{Folder: "committed-no-stt", Decision: committedDecision, JobID: "JOB-COMMITTED"},
			}, CompletedAt: time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC),
		}, {
			ID: "ZZZ-OLDER-RUN", SessionID: "INTAKE-9", InputRevision: 9, WorkspaceDriveID: driveID, WorkerID: "WORKER-WINDOWS",
			Files: []organization.CoordinatedFile{{RelativePath: "older/clip.mov"}}, Folders: []organization.CoordinatedFolder{{
				Folder: "older", Decision: organization.OrganizationDecision{
					ID: "DECISION-OLDER", Sequence: 1, WorkspaceDriveID: driveID, Folder: "older", InputRevision: 9,
					Disposition: vocabulary.OrganizationHold, HoldReasons: []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
				},
			}}, CompletedAt: time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC),
		}},
		decisions: map[string]organization.OrganizationDecision{
			"recordings": renderDecision, "interviews": interviewDecision, "unclassified": holdDecision,
			"device-wait": deviceDecision, "committed-no-stt": committedDecision,
		},
	}
	jobs := workstationJobs{
		"RUN-9": archiveRunSnapshot(t, archiverun.State{
			ID: "RUN-9", Revision: 1, WorkerID: "WORKER-WINDOWS", SourceStorageID: "SOURCE-1",
			SourceVolumeID: driveID, SourcePhysicalDeviceID: "DISK-1", ArchiveStorageID: "ARCHIVE-1", FolderDiscoveryRevision: 9,
			FolderIDs:       []string{"committed-no-stt", "device-wait", "interviews", "recordings", "unclassified"},
			SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "OPERATOR-1"},
			StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
			CreatedAt:       time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC),
		}),
		"JOB-RENDER": jobSnapshot(t, job.State{
			ID: "JOB-RENDER", Revision: 4, TemplateName: "archive-intake-render-recordings", TargetTitle: "recordings",
			CurrentStepID: "render", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "render", State: workflow.Waiting, WaitReason: workflow.WaitMemory}, Progress: 0.4},
				{Workflow: workflow.Step{ID: "qc", State: workflow.Queued}},
				{Workflow: workflow.Step{ID: "organize", State: workflow.Queued}},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Queued}},
			},
		}),
		"JOB-INTERVIEW": jobSnapshot(t, job.State{
			ID: "JOB-INTERVIEW", Revision: 6, TemplateName: "archive-intake-as-is", TargetTitle: "interviews",
			CurrentStepID: "archive_catalog", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "qc", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "organize", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{
					Outputs: []execution.StepOutput{{ID: "VP-20261030-INTERVIEW", Kind: execution.OutputOrganizedMedia}},
				}},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Completed}},
			},
		}),
		"JOB-DEVICE": jobSnapshot(t, job.State{
			ID: "JOB-DEVICE", Revision: 5, TemplateName: "archive-intake-render-recordings", TargetTitle: "device-wait",
			CurrentStepID: "organize", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "render", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "qc", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "organize", State: workflow.Waiting, WaitReason: workflow.WaitDevice}, WaitTargetStorageID: "ARCHIVE-1"},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Queued}},
			},
		}),
		"JOB-COMMITTED": jobSnapshot(t, job.State{
			ID: "JOB-COMMITTED", Revision: 6, TemplateName: "archive-intake-as-is", TargetTitle: "committed-no-stt",
			CurrentStepID: "archive_catalog", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "qc", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "organize", State: workflow.Completed}, OutputRecord: &execution.StepOutputRecord{
					Outputs: []execution.StepOutput{{ID: "VP-20261030-COMMITTED", Kind: execution.OutputOrganizedMedia}},
				}},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Completed}},
			},
		}),
		"STT-JOB-INTERVIEW": jobSnapshot(t, job.State{
			ID: "STT-JOB-INTERVIEW", Revision: 3, TemplateName: "archive-transcription", TargetTitle: "interviews",
			CurrentStepID: "transcript", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "audio", State: workflow.Completed}, ExecutionSpec: archiveRunSpec("RUN-9", "interviews")},
				{Workflow: workflow.Step{ID: "transcript", State: workflow.Waiting, WaitReason: workflow.WaitSlot}},
				{Workflow: workflow.Step{ID: "subtitle", State: workflow.Queued}},
				{Workflow: workflow.Step{ID: "transcript_text", State: workflow.Queued}},
				{Workflow: workflow.Step{ID: "archive_package_revision", State: workflow.Queued}},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Queued}},
			},
		}),
		"STT-JOB-INTERVIEW-2": jobSnapshot(t, job.State{
			ID: "STT-JOB-INTERVIEW-2", Revision: 8, TemplateName: "archive-transcription", TargetTitle: "interviews",
			CurrentStepID: "archive_catalog", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "audio", State: workflow.Completed}, ExecutionSpec: archiveRunSpec("RUN-9", "interviews")},
				{Workflow: workflow.Step{ID: "transcript", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "subtitle", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "transcript_text", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "archive_package_revision", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Completed}},
			},
		}),
	}
	service, err := archiveintakeworkspace.New(store, jobs, archiveintakeworkspace.WithArchiveStateReader(packageStates{
		"VP-20261030-INTERVIEW": archive.PackageCommitted,
		"VP-20261030-COMMITTED": archive.PackageCommitted,
	}))
	if err != nil {
		t.Fatal(err)
	}

	views, err := service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].WorkspaceDriveID != driveID || views[0].FileCount != 240 || views[0].FolderCount != 5 ||
		views[0].ArchiveRunID != "RUN-9" || views[0].ArchiveRunRevision != 1 || views[0].SourceStorageID != "SOURCE-1" ||
		views[0].ArchiveStorageID != "ARCHIVE-1" || views[0].StageOneRemaining != 2 || views[0].StageTwoRemaining != 1 ||
		views[0].HoldCount != 1 || views[0].EligibleTranscriptionCount != 0 {
		t.Fatalf("bulk drive was not projected as one workstation: %+v", views)
	}
	render := folderView(t, views[0], "recordings")
	wantStages := []archiveintakeworkspace.StageView{
		{ID: "classification", State: workflow.Completed, CompletedCount: 240, TotalCount: 240},
		{ID: "render", State: workflow.Waiting, WaitReason: workflow.WaitMemory, CompletedCount: 96, TotalCount: 240},
		{ID: "qc", State: workflow.Queued, TotalCount: 240},
		{ID: "archive", State: workflow.Queued, TotalCount: 240},
	}
	if fmt.Sprint(render.Stages) != fmt.Sprint(wantStages) {
		t.Fatalf("render folder stages lost persisted order or state: got=%+v want=%+v", render.Stages, wantStages)
	}
	interview := folderView(t, views[0], "interviews")
	if len(interview.Stages) != 8 || interview.Stages[4] != (archiveintakeworkspace.StageView{
		ID: "transcript", State: workflow.Waiting, WaitReason: workflow.WaitSlot, CompletedCount: 1, TotalCount: 2,
	}) || interview.ArchiveState != archive.PackageCommitted || interview.CanAddTranscription {
		t.Fatalf("stt resource wait reason was not preserved: %+v", interview.Stages)
	}
	hold := folderView(t, views[0], "unclassified")
	if len(hold.Stages) != 1 || hold.Stages[0] != (archiveintakeworkspace.StageView{
		ID: "classification", State: workflow.Waiting, WaitReason: workflow.WaitInput,
	}) || !reflect.DeepEqual(hold.WorkOptions, []archiveintakeworkspace.WorkOptionView{
		{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 6_000},
		{Disposition: vocabulary.OrganizationRenderRecordings, TargetVideoBitRateKbps: 20_000},
		{Disposition: vocabulary.OrganizationCompileSourceSequence},
	}) {
		t.Fatalf("hold classification stage was not waiting: %+v", hold.Stages)
	}
	device := folderView(t, views[0], "device-wait")
	if len(device.Stages) != 4 || device.Stages[3] != (archiveintakeworkspace.StageView{
		ID: "archive", State: workflow.Waiting, WaitReason: workflow.WaitDevice, WaitTargetStorageID: "ARCHIVE-1",
	}) {
		t.Fatalf("device wait reason was not preserved on archive stage: %+v", device.Stages)
	}
	for _, folder := range views[0].Folders {
		for _, stage := range folder.Stages {
			if stage.State != workflow.Waiting && stage.WaitReason != "" {
				t.Fatalf("non-waiting stage exposed a guessed reason: %+v", stage)
			}
		}
	}
	payload, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "recordings/clip-") || strings.Contains(string(payload), `C:\\`) || strings.Contains(string(payload), "/Volumes/") {
		t.Fatalf("workstation projection exposed a file path: %s", payload)
	}

	// A newly persisted ArchiveRun must replace an older completed receipt in
	// the workspace immediately, before the worker inspection has produced the
	// new run's organization receipt.
	pendingRun := archiverun.State{
		ID: "RUN-10-PENDING", Revision: 1, WorkerID: "WORKER-WINDOWS", SourceStorageID: "SOURCE-1",
		SourceVolumeID: driveID, SourcePhysicalDeviceID: "DISK-1", ArchiveStorageID: "ARCHIVE-1",
		FolderDiscoveryRevision: 10, FolderIDs: []string{"new-selection"},
		SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "OPERATOR-1"},
		StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
		CreatedAt:       time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC),
	}
	pendingJobs := make(workstationJobs, len(jobs)+1)
	for id, snapshot := range jobs {
		pendingJobs[id] = snapshot
	}
	pendingJobs["RUN-10-PENDING"] = archiveRunSnapshot(t, pendingRun)
	pendingService, err := archiveintakeworkspace.New(store, pendingJobs)
	if err != nil {
		t.Fatal(err)
	}
	pendingViews, err := pendingService.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingViews) != 1 || pendingViews[0].ArchiveRunID != pendingRun.ID || pendingViews[0].InputRevision != 10 ||
		len(pendingViews[0].Folders) != 1 || pendingViews[0].Folders[0].Folder != "new-selection" {
		t.Fatalf("newly persisted run was hidden behind an older completed receipt: %+v", pendingViews)
	}
}

func TestArchiveRunProjectionCarriesPersistedTranscriptQualityAssessment(t *testing.T) {
	decision := organization.OrganizationDecision{
		ID: "DECISION-1", Sequence: 1, WorkspaceDriveID: "VOLUME-1", Folder: "recording", InputRevision: 4,
		Disposition: vocabulary.OrganizationAsIs,
	}
	store := workstationStore{
		receipts: []organization.ArchiveOrganizationReceipt{{
			ID: "ARCHIVE-RUN-1", SessionID: "INTAKE-1", InputRevision: 4, WorkspaceDriveID: "VOLUME-1", WorkerID: "WORKER-1",
			Folders:     []organization.CoordinatedFolder{{Folder: "recording", Decision: decision, JobID: "JOB-1"}},
			CompletedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		}},
		decisions: map[string]organization.OrganizationDecision{"recording": decision},
	}
	quality := &transcription.QualityAssessment{
		PolicyReference: transcription.QualityPolicyReference, State: transcription.QualityReviewRecommended,
		Reasons:        []transcription.QualityReason{transcription.QualityReasonSegmentTooLong, transcription.QualityReasonLowTextDensity},
		DurationMillis: 19_120, SegmentCount: 1, TextCharacterCount: 9,
	}
	jobs := workstationJobs{
		"ARCHIVE-RUN-1": archiveRunSnapshot(t, archiverun.State{
			ID: "ARCHIVE-RUN-1", Revision: 2, WorkerID: "WORKER-1", SourceStorageID: "SOURCE-1", ArchiveStorageID: "ARCHIVE-1",
			SourceVolumeID: "VOLUME-1", SourcePhysicalDeviceID: "DISK-1",
			FolderDiscoveryRevision: 4, FolderIDs: []string{"recording"},
			SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "OPERATOR-1"},
			StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
			CreatedAt:       time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		}),
		"JOB-1": jobSnapshot(t, job.State{
			ID: "JOB-1", Revision: 1, TemplateName: "archive-intake-as-is", TargetTitle: "recording",
			CurrentStepID: "organize", Steps: []job.Step{{
				Workflow:     workflow.Step{ID: "organize", State: workflow.Completed},
				OutputRecord: &execution.StepOutputRecord{Outputs: []execution.StepOutput{{ID: "VP-1", Kind: execution.OutputOrganizedMedia}}},
			}},
		}),
		"STT-JOB-1": jobSnapshot(t, job.State{
			ID: "STT-JOB-1", Revision: 3, TemplateName: "archive-transcription", TargetTitle: "recording",
			CurrentStepID: "transcript", Steps: []job.Step{{
				Workflow: workflow.Step{ID: "transcript", State: workflow.Completed},
				ExecutionSpec: &execution.StepExecutionSpec{
					Parameters: execution.StepExecutionParameters{ArchiveRunID: "ARCHIVE-RUN-1", MediaFolder: "recording"},
					Outputs:    []execution.OutputTarget{{ID: "TRANSCRIPT-1", Kind: execution.OutputTranscript, OriginalAssetID: "ASSET-1"}},
				},
				OutputRecord: &execution.StepOutputRecord{Outputs: []execution.StepOutput{{
					ID: "TRANSCRIPT-1", Kind: execution.OutputTranscript, TranscriptQuality: quality,
				}}},
			}},
		}),
	}
	service, err := archiveintakeworkspace.New(store, jobs)
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	folder := folderView(t, views[0], "recording")
	if len(folder.TranscriptQualities) != 1 || folder.TranscriptQualities[0].TranscriptID != "TRANSCRIPT-1" ||
		folder.TranscriptQualities[0].OriginalAssetID != "ASSET-1" ||
		folder.TranscriptQualities[0].State != transcription.QualityReviewRecommended ||
		!reflect.DeepEqual(folder.TranscriptQualities[0].Reasons, quality.Reasons) {
		t.Fatalf("persisted transcript quality was not projected explicitly: %+v", folder.TranscriptQualities)
	}
}

// Requirement: DSH-027
func TestArchiveRunProjectionUsesLatestCompletedCatalogPublicationTime(t *testing.T) {
	receiptAt := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	stageOneAt := receiptAt.Add(4 * time.Second)
	stageTwoAt := receiptAt.Add(84 * time.Second)
	decision := organization.OrganizationDecision{
		ID: "DECISION-1", Sequence: 1, WorkspaceDriveID: "VOLUME-1", Folder: "recording", InputRevision: 4,
		Disposition: vocabulary.OrganizationAsIs,
	}
	store := workstationStore{
		receipts: []organization.ArchiveOrganizationReceipt{{
			ID: "ARCHIVE-RUN-1", SessionID: "INTAKE-1", InputRevision: 4, WorkspaceDriveID: "VOLUME-1", WorkerID: "WORKER-1",
			Files:       []organization.CoordinatedFile{{RelativePath: "recording/video.mp4", OriginalAssetID: "ASSET-1"}},
			Folders:     []organization.CoordinatedFolder{{Folder: "recording", Decision: decision, JobID: "JOB-1"}},
			CompletedAt: receiptAt,
		}},
		decisions: map[string]organization.OrganizationDecision{"recording": decision},
	}

	for _, test := range []struct {
		name          string
		stageTwoState workflow.StepState
		want          time.Time
	}{
		{name: "completed stage two publication", stageTwoState: workflow.Completed, want: stageTwoAt},
		{name: "unfinished stage two publication", stageTwoState: workflow.Running, want: stageOneAt},
	} {
		t.Run(test.name, func(t *testing.T) {
			jobs := workstationJobs{
				"ARCHIVE-RUN-1": archiveRunSnapshot(t, archiverun.State{
					ID: "ARCHIVE-RUN-1", Revision: 1, WorkerID: "WORKER-1", SourceStorageID: "SOURCE-1", ArchiveStorageID: "ARCHIVE-1",
					SourceVolumeID: "VOLUME-1", SourcePhysicalDeviceID: "DISK-1",
					FolderDiscoveryRevision: 4, FolderIDs: []string{"recording"},
					SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "OPERATOR-1"},
					StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
					CreatedAt:       receiptAt,
				}),
				"JOB-1": jobSnapshot(t, job.State{
					ID: "JOB-1", Revision: 2, TemplateName: "archive-intake-as-is", TargetTitle: "recording",
					CurrentStepID: "archive_catalog", Steps: []job.Step{{
						Workflow:    workflow.Step{ID: "archive_catalog", State: workflow.Completed},
						LastEventAt: stageOneAt, ExecutionSpec: archiveRunSpec("ARCHIVE-RUN-1", "recording"),
					}},
				}),
				"STT-JOB-1": jobSnapshot(t, job.State{
					ID: "STT-JOB-1", Revision: 2, TemplateName: "archive-transcription", TargetTitle: "recording",
					CurrentStepID: "archive_catalog", Steps: []job.Step{{
						Workflow:    workflow.Step{ID: "archive_catalog", State: test.stageTwoState},
						LastEventAt: stageTwoAt, ExecutionSpec: archiveRunSpec("ARCHIVE-RUN-1", "recording"),
					}},
				}),
			}
			service, err := archiveintakeworkspace.New(store, jobs)
			if err != nil {
				t.Fatal(err)
			}
			views, err := service.Projection(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(views) != 1 || !views[0].CatalogUpdatedAt.Equal(test.want) {
				t.Fatalf("catalog publication time was not projected from the latest completed step: %+v", views)
			}
		})
	}
}

func TestCompletedWorkProjectionRetainsOlderPackageWithoutCountingHoldAsOutput(t *testing.T) {
	const driveID = "VOLUME-1"
	completedAt := time.Date(2026, 11, 4, 16, 43, 24, 0, time.UTC)
	oldDecision := organization.OrganizationDecision{
		ID: "DECISION-OLD", Sequence: 1, WorkspaceDriveID: driveID,
		Folder: "shoot/CLIP", InputRevision: 4, Disposition: vocabulary.OrganizationCompileSourceSequence,
		ProfileReference: "archive-1080p-20mbps@v1",
		Inputs: []organization.Input{
			{OriginalAssetID: "ASSET-1", RelativePath: "shoot/CLIP/C0001.MP4"},
			{OriginalAssetID: "ASSET-2", RelativePath: "shoot/CLIP/C0002.MP4"},
		},
	}
	holdDecision := organization.OrganizationDecision{
		ID: "DECISION-HOLD", Sequence: 1, WorkspaceDriveID: driveID,
		Folder: "new/hold", InputRevision: 5, Disposition: vocabulary.OrganizationHold,
		HoldReasons: []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
	}
	store := workstationStore{
		receipts: []organization.ArchiveOrganizationReceipt{
			{ID: "ARCHIVE-RUN-OLD", SessionID: "INTAKE-1", InputRevision: 4, WorkspaceDriveID: driveID, WorkerID: "WORKER-1",
				Folders: []organization.CoordinatedFolder{{Folder: oldDecision.Folder, Decision: oldDecision, JobID: "JOB-OLD"}}, CompletedAt: completedAt.Add(-time.Hour)},
			{ID: "ARCHIVE-RUN-NEW", SessionID: "INTAKE-2", InputRevision: 5, WorkspaceDriveID: driveID, WorkerID: "WORKER-1",
				Folders: []organization.CoordinatedFolder{{Folder: holdDecision.Folder, Decision: holdDecision}}, CompletedAt: completedAt.Add(time.Hour)},
		},
		decisions: map[string]organization.OrganizationDecision{
			"ARCHIVE-RUN-OLD\x00" + oldDecision.Folder:  oldDecision,
			"ARCHIVE-RUN-NEW\x00" + holdDecision.Folder: holdDecision,
		},
	}
	organizeSpec := &execution.StepExecutionSpec{
		Parameters: execution.StepExecutionParameters{ArchiveRunID: "ARCHIVE-RUN-OLD", MediaFolder: oldDecision.Folder},
		Outputs: []execution.OutputTarget{{
			ID: "VP-20261104-CLIP", Kind: execution.OutputOrganizedMedia, StorageID: "ARCHIVE-1",
			RelativePath: "packages/2026/11/04/VP-20261104-CLIP/revisions/000001/metadata.json",
		}},
	}
	jobs := workstationJobs{
		"ARCHIVE-RUN-NEW": archiveRunSnapshot(t, archiverun.State{
			ID: "ARCHIVE-RUN-NEW", Revision: 1, WorkerID: "WORKER-1", SourceStorageID: "SOURCE-1", ArchiveStorageID: "ARCHIVE-1",
			SourceVolumeID: driveID, SourcePhysicalDeviceID: "DISK-1",
			FolderDiscoveryRevision: 5, FolderIDs: []string{holdDecision.Folder},
			SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "OPERATOR-1"},
			StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}}, CreatedAt: completedAt.Add(time.Hour),
		}),
		"JOB-OLD": jobSnapshot(t, job.State{
			ID: "JOB-OLD", Revision: 6, TemplateName: "archive-intake-source-sequence", TargetTitle: oldDecision.Folder,
			CurrentStepID: "archive_catalog", Steps: []job.Step{
				{Workflow: workflow.Step{ID: "source_timeline", State: workflow.Completed}, ExecutionSpec: archiveRunSpec("ARCHIVE-RUN-OLD", oldDecision.Folder)},
				{Workflow: workflow.Step{ID: "sequence_render", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "qc", State: workflow.Completed}},
				{Workflow: workflow.Step{ID: "organize", State: workflow.Completed}, ExecutionSpec: organizeSpec,
					OutputRecord: &execution.StepOutputRecord{RecordedAt: completedAt, Outputs: []execution.StepOutput{{
						ID: "VP-20261104-CLIP", Kind: execution.OutputOrganizedMedia, StorageID: "ARCHIVE-1",
						RelativePath: "packages/2026/11/04/VP-20261104-CLIP/revisions/000001/metadata.json",
					}}},
				},
				{Workflow: workflow.Step{ID: "archive_catalog", State: workflow.Completed}},
			},
		}),
	}
	service, err := archiveintakeworkspace.New(store, jobs, archiveintakeworkspace.WithArchiveStateReader(packageStates{
		"VP-20261104-CLIP": archive.PackageCommitted,
	}))
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ArchiveRunID != "ARCHIVE-RUN-NEW" || len(views[0].Folders) != 1 || views[0].Folders[0].Folder != holdDecision.Folder {
		t.Fatalf("latest run folders were not kept separate from completion history: %+v", views)
	}
	if len(views[0].CompletedWork) != 1 {
		t.Fatalf("completed package history was not retained or HOLD became output: %+v", views[0].CompletedWork)
	}
	completed := views[0].CompletedWork[0]
	if completed.ArchiveRunID != "ARCHIVE-RUN-OLD" || completed.ArchivePackageID != "VP-20261104-CLIP" ||
		completed.ArchiveVideoFileName != "CLIP.mp4" || completed.SourceVideoCount != 2 || completed.ArchiveStorageID != "ARCHIVE-1" ||
		completed.ArchiveRelativePath != "packages/2026/11/04/VP-20261104-CLIP/revisions/000001" ||
		completed.ArchiveState != archive.PackageCommitted || !completed.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed package facts were not projected: %+v", completed)
	}
}

// Requirement: ORG-014
func TestResolvedHoldContinuesSameArchiveRunWithOneIdempotentFolderJob(t *testing.T) {
	now := time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)
	hold := organization.OrganizationDecision{
		ID: "DECISION-HOLD", Sequence: 1, WorkspaceDriveID: "VOLUME-1", Folder: "hold", InputRevision: 7,
		Disposition: vocabulary.OrganizationHold,
		Inputs: []organization.Input{{
			OriginalAssetID: "ASSET-1", RelativePath: "hold/a.mp4",
			Inspection: mediaanalysis.MediaInspection{
				ID: "INSPECTION-1", OriginalAssetID: "ASSET-1", OriginalChecksum: strings.Repeat("a", 64),
				Containers: []string{"mp4"}, DurationMillis: 60_000, SizeBytes: 45_000_000, BitRate: 6_192_000,
				VideoStreams: []mediaanalysis.VideoStream{{
					Index: 0, Codec: "h264", Width: 1920, Height: 1080,
					FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: 6_000_000,
				}},
				AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
				ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: now,
			},
		}},
		HoldReasons:   []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
		DecidedByType: vocabulary.ActorSystem, DecidedBy: "SYSTEM", DecidedAt: now,
	}
	store := &resolvedHoldStore{
		receipt: organization.ArchiveOrganizationReceipt{
			ID: "ARCHIVE-RUN-1", SessionID: "INTAKE-1", InputRevision: 7,
			WorkspaceDriveID: "VOLUME-1", WorkerID: "WORKER-1",
			Files:   []organization.CoordinatedFile{{RelativePath: "hold/a.mp4", OriginalAssetID: "ASSET-1"}},
			Folders: []organization.CoordinatedFolder{{Folder: "hold", Decision: hold}}, CompletedAt: now,
		},
		latest: hold,
	}
	jobs := workstationJobs{
		"ARCHIVE-RUN-1": archiveRunSnapshot(t, archiverun.State{
			ID: "ARCHIVE-RUN-1", Revision: 1, WorkerID: "WORKER-1", SourceStorageID: "SOURCE-1",
			SourceVolumeID: "VOLUME-1", SourcePhysicalDeviceID: "DISK-1", ArchiveStorageID: "ARCHIVE-1",
			FolderDiscoveryRevision: 7, FolderIDs: []string{"hold"},
			SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"},
			StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}},
			CreatedAt:       now,
		}),
	}
	starter := &resolvedFolderStarterFixture{t: t, jobs: jobs, effects: map[string]bool{}}
	service, err := archiveintakeworkspace.New(store, jobs, archiveintakeworkspace.WithResolvedFolderStarter(starter))
	if err != nil {
		t.Fatal(err)
	}
	request := archiveintakeworkspace.OverrideCommand{
		CommandID: "COMMAND-CLASSIFY", DecisionID: "DECISION-AS-IS", IdempotencyKey: "CLASSIFY-1",
		ArchiveRunID: "ARCHIVE-RUN-1", WorkspaceDriveID: "VOLUME-1", Folder: "hold", ExpectedInputRevision: 7,
		Disposition: vocabulary.OrganizationAsIs, Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:session-1"},
		Reason: "verified optimized rendition", Basis: []string{"ASSET-1", "INSPECTION-1"}, OccurredAt: now,
	}
	invalid := request
	invalid.Disposition = vocabulary.OrganizationExcludeFromRun
	if _, err := service.Override(context.Background(), invalid); !errors.Is(err, organization.ErrInvalidDecision) || starter.attempts != 0 || store.latest.Sequence != 1 {
		t.Fatalf("ordinary HOLD accepted a noise-only choice: %v", err)
	}
	first, err := service.Override(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Override(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	folder := folderView(t, views[0], "hold")
	if first.ID != second.ID || store.latest.Sequence != 2 || starter.attempts != 2 || len(starter.effects) != 1 ||
		starter.archiveRun.ID != "ARCHIVE-RUN-1" || starter.archiveRun.SourceStorageID != "SOURCE-1" ||
		starter.archiveRun.SourcePhysicalDeviceID != "DISK-1" || starter.archiveRun.ArchiveStorageID != "ARCHIVE-1" ||
		starter.decision.ID != "DECISION-AS-IS" || folder.JobID != "JOB-RESOLVED-HOLD" ||
		folder.Disposition != vocabulary.OrganizationAsIs || folder.CurrentStepID == "job_pending" {
		t.Fatalf("resolved HOLD did not continue once in the same ArchiveRun: first=%+v second=%+v starter=%+v folder=%+v", first, second, starter, folder)
	}
}

func folderView(t *testing.T, drive archiveintakeworkspace.DriveView, folder string) archiveintakeworkspace.FolderView {
	t.Helper()
	for _, view := range drive.Folders {
		if view.Folder == folder {
			return view
		}
	}
	t.Fatalf("folder %s was not projected", folder)
	return archiveintakeworkspace.FolderView{}
}

func jobSnapshot(t *testing.T, state job.State) command.Snapshot {
	t.Helper()
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: payload}
}

func archiveRunSnapshot(t *testing.T, state archiverun.State) command.Snapshot {
	t.Helper()
	if state.StartIdempotencyKey == "" {
		state.StartIdempotencyKey = state.ID + ":start"
	}
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return command.Snapshot{TargetID: state.ID, Revision: state.Revision, State: payload}
}

func archiveRunSpec(archiveRunID, folder string) *execution.StepExecutionSpec {
	return &execution.StepExecutionSpec{Parameters: execution.StepExecutionParameters{
		ArchiveRunID: archiveRunID, MediaFolder: folder,
	}}
}

type packageStates map[string]archive.PackageState

func (states packageStates) PackageState(_ context.Context, _, packageID string) (archive.PackageState, error) {
	return states[packageID], nil
}

type workstationStore struct {
	receipts  []organization.ArchiveOrganizationReceipt
	decisions map[string]organization.OrganizationDecision
}

func (store workstationStore) Receipts(context.Context) ([]organization.ArchiveOrganizationReceipt, error) {
	return append([]organization.ArchiveOrganizationReceipt(nil), store.receipts...), nil
}

func (store workstationStore) LatestDecision(_ context.Context, _ string, folder string) (organization.OrganizationDecision, error) {
	return store.decisions[folder], nil
}

func (store workstationStore) LatestDecisionForArchiveRun(_ context.Context, archiveRunID, folder string) (organization.OrganizationDecision, error) {
	if decision, found := store.decisions[archiveRunID+"\x00"+folder]; found {
		return decision, nil
	}
	return store.decisions[folder], nil
}

func (workstationStore) FindOverrideReceipt(context.Context, string) (organization.OrganizationDecision, string, bool, error) {
	return organization.OrganizationDecision{}, "", false, nil
}

func (workstationStore) AppendOverride(context.Context, string, string, string, organization.OrganizationDecision) error {
	return nil
}

type workstationJobs map[string]command.Snapshot

func (jobs workstationJobs) Load(id string) (command.Snapshot, error) {
	snapshot, found := jobs[id]
	if !found {
		return command.Snapshot{}, command.ErrTargetNotFound
	}
	return snapshot, nil
}

func (jobs workstationJobs) ListSnapshots() ([]command.Snapshot, error) {
	snapshots := make([]command.Snapshot, 0, len(jobs))
	for _, snapshot := range jobs {
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

type resolvedHoldStore struct {
	receipt      organization.ArchiveOrganizationReceipt
	latest       organization.OrganizationDecision
	replay       organization.OrganizationDecision
	replayDigest string
}

func (store *resolvedHoldStore) Receipts(context.Context) ([]organization.ArchiveOrganizationReceipt, error) {
	return []organization.ArchiveOrganizationReceipt{store.receipt}, nil
}

func (store *resolvedHoldStore) LatestDecision(context.Context, string, string) (organization.OrganizationDecision, error) {
	return store.latest, nil
}

func (store *resolvedHoldStore) LatestDecisionForArchiveRun(context.Context, string, string) (organization.OrganizationDecision, error) {
	return store.latest, nil
}

func (store *resolvedHoldStore) FindOverrideReceipt(_ context.Context, idempotencyKey string) (organization.OrganizationDecision, string, bool, error) {
	if idempotencyKey == "CLASSIFY-1" && store.replay.ID != "" {
		return store.replay, store.replayDigest, true, nil
	}
	return organization.OrganizationDecision{}, "", false, nil
}

func (store *resolvedHoldStore) AppendOverride(_ context.Context, idempotencyKey, digest, _ string, decision organization.OrganizationDecision) error {
	if idempotencyKey != "CLASSIFY-1" || store.replay.ID != "" {
		return fmt.Errorf("unexpected duplicate override")
	}
	store.latest = decision
	store.replay = decision
	store.replayDigest = digest
	return nil
}

type resolvedFolderStarterFixture struct {
	t          *testing.T
	jobs       workstationJobs
	effects    map[string]bool
	attempts   int
	archiveRun archiverun.State
	decision   organization.OrganizationDecision
}

func (starter *resolvedFolderStarterFixture) StartResolvedFolder(
	_ context.Context,
	_ string,
	idempotencyKey string,
	archiveRun archiverun.State,
	decision organization.OrganizationDecision,
	_ time.Time,
) (string, error) {
	starter.attempts++
	starter.archiveRun = archiveRun
	starter.decision = decision
	if !starter.effects[idempotencyKey] {
		starter.effects[idempotencyKey] = true
		starter.jobs["JOB-RESOLVED-HOLD"] = jobSnapshot(starter.t, job.State{
			ID: "JOB-RESOLVED-HOLD", Revision: 1, TemplateName: "archive-intake-as-is", TargetTitle: decision.Folder,
			CurrentStepID: "qc", Steps: []job.Step{{
				Workflow:      workflow.Step{ID: "qc", State: workflow.Queued},
				ExecutionSpec: archiveRunSpec(archiveRun.ID, decision.Folder),
			}},
		})
	}
	return "JOB-RESOLVED-HOLD", nil
}

// Requirement: DSH-054
func TestWorkspaceRetainsPerFileProfileEvidenceAndSumsTargetSizes(t *testing.T) {
	const folder = "370105-행사_촬영"
	d := organization.OrganizationDecision{ID: "decision", Sequence: 1, WorkspaceDriveID: "drive", Folder: folder, InputRevision: 5, PolicyVersion: 4, Disposition: vocabulary.OrganizationRenderRecordings}
	receipt := organization.ArchiveOrganizationReceipt{ID: "run", WorkspaceDriveID: "drive", InputRevision: 5, WorkerID: "worker", CompletedAt: time.Now().UTC()}
	for _, input := range []struct {
		name, role, profile string
		duration            int64
	}{{"cut.mov", organization.RoleSource, "archive-1080p-20mbps@v1", 10000}, {"rec.mp4", organization.RoleRecording, "archive-1080p-6mbps@v1", 1200000}} {
		rel := folder + "/" + input.name
		d.Inputs = append(d.Inputs, organization.Input{OriginalAssetID: input.name, RelativePath: rel, Inspection: mediaanalysis.MediaInspection{ID: "inspection-" + input.name, SizeBytes: 16000, DurationMillis: input.duration, VideoStreams: []mediaanalysis.VideoStream{{Width: 1920, Height: 1080, BitRate: 25_000_000}}}, RenderSelection: &organization.RenderSelection{Role: input.role, ProfileReference: input.profile, Basis: "검사된 역할 증거"}})
		receipt.Files = append(receipt.Files, organization.CoordinatedFile{OriginalAssetID: input.name, RelativePath: rel})
	}
	receipt.Folders = []organization.CoordinatedFolder{{Folder: folder, Decision: d}}
	store := workstationStore{receipts: []organization.ArchiveOrganizationReceipt{receipt}, decisions: map[string]organization.OrganizationDecision{folder: d}}
	service, err := archiveintakeworkspace.New(store, workstationJobs{})
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil || len(views) != 1 || len(views[0].Folders) != 1 {
		t.Fatal(views, err)
	}
	f := views[0].Folders[0]
	if f.EstimatedOutputBytes != 925000000 || len(f.Videos) != 2 || len(f.ClassificationAssets) != 2 || f.ProfileReference != "" {
		t.Fatal(f)
	}
	for i, v := range f.Videos {
		a := f.ClassificationAssets[i]
		if v.RenderSelection == nil || !reflect.DeepEqual(v.RenderSelection, d.Inputs[i].RenderSelection) || !reflect.DeepEqual(a.RenderSelection, v.RenderSelection) || a.InspectionID != d.Inputs[i].Inspection.ID || v.Measurement == nil || v.Measurement.InspectionID != a.InspectionID || v.Measurement.VideoBitRate != a.VideoStreams[0].BitRate {
			t.Fatalf("missing stored evidence %+v %+v", v, a)
		}
	}
	now := receipt.CompletedAt
	rescan := worker.StorageObservation{StorageID: "S", Connected: true, DiscoveryState: vocabulary.FolderDiscoveryReady, DiscoveryRevision: 6, Folders: []worker.MediaFolderObservation{{Folder: folder, VideoCount: 3}}}
	statuses := statusInventory{{WorkerID: "worker", State: "ONLINE", Metrics: worker.Metrics{ObservedAt: now, StorageObservations: []worker.StorageObservation{rescan}}}}
	service, err = archiveintakeworkspace.New(store, workstationJobs{}, archiveintakeworkspace.WithInventory(candidateInventory{{VolumeID: "drive", Revision: 1, WorkerID: "worker", StorageID: "S", Connected: true}}, storageInventory{}, statuses, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	views, err = service.Projection(context.Background())
	if err != nil || len(views) != 1 || views[0].DiscoveredFileCount != 3 || len(views[0].Folders) != 1 || !reflect.DeepEqual(views[0].Folders[0].Videos, f.Videos) {
		t.Fatalf("new inventory must retain its own count and the run's original measurements: views=%+v, err=%v", views, err)
	}

}
