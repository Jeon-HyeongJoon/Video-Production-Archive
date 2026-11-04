package archivetranscription

import (
	"context"
	"errors"
	"path"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var ErrStageOneProjection = errors.New("archive run stage-one projection is incomplete")

type OrganizationRuns interface {
	Receipts(context.Context) ([]organization.ArchiveOrganizationReceipt, error)
}

type JobSnapshots interface {
	Load(string) (command.Snapshot, error)
}

// JobStageOneProjection rebuilds the trusted stage-one gate and package
// references from the organization receipt and immutable Job outputs. Browser
// input is deliberately not accepted at this boundary.
type JobStageOneProjection struct {
	runs OrganizationRuns
	jobs JobSnapshots
}

func NewJobStageOneProjection(runs OrganizationRuns, jobs JobSnapshots) (*JobStageOneProjection, error) {
	if runs == nil || jobs == nil {
		return nil, ErrStageOneProjection
	}
	return &JobStageOneProjection{runs: runs, jobs: jobs}, nil
}

func (projection *JobStageOneProjection) StageOne(ctx context.Context, archiveRun archiverun.State) ([]archiverun.FolderProgress, []StoredRecording, error) {
	receipts, err := projection.runs.Receipts(ctx)
	if err != nil {
		return nil, nil, err
	}
	var receipt organization.ArchiveOrganizationReceipt
	for _, candidate := range receipts {
		if candidate.ID == archiveRun.ID {
			receipt = candidate
			break
		}
	}
	if receipt.ID == "" || receipt.WorkerID != archiveRun.WorkerID || receipt.InputRevision != archiveRun.FolderDiscoveryRevision {
		return nil, nil, ErrStageOneProjection
	}
	owned := make(map[string]bool, len(archiveRun.FolderIDs))
	for _, folderID := range archiveRun.FolderIDs {
		owned[folderID] = true
	}
	progress := make([]archiverun.FolderProgress, 0, len(receipt.Folders))
	recordings := make([]StoredRecording, 0, len(receipt.Folders))
	seen := make(map[string]bool, len(receipt.Folders))
	for _, folder := range receipt.Folders {
		if !owned[folder.Folder] || seen[folder.Folder] {
			return nil, nil, ErrStageOneProjection
		}
		seen[folder.Folder] = true
		if folder.Decision.Disposition == vocabulary.OrganizationHold {
			progress = append(progress, archiverun.FolderProgress{
				FolderID: folder.Folder, Disposition: folder.Decision.Disposition, StageOne: workflow.Waiting,
			})
			continue
		}
		if strings.TrimSpace(folder.JobID) == "" {
			return nil, nil, ErrStageOneProjection
		}
		snapshot, err := projection.jobs.Load(folder.JobID)
		if err != nil {
			return nil, nil, err
		}
		state, err := job.Decode(snapshot)
		if err != nil {
			return nil, nil, err
		}
		stageState := stageOneState(state)
		stored := completedStep(state, "organize") != nil
		progress = append(progress, archiverun.FolderProgress{
			FolderID: folder.Folder, Disposition: folder.Decision.Disposition, StageOne: stageState, Stored: stored,
		})
		if !stored || stageState != workflow.Completed ||
			(folder.Decision.Disposition != vocabulary.OrganizationAsIs && folder.Decision.Disposition != vocabulary.OrganizationRenderRecordings) {
			continue
		}
		recording, err := storedRecordingFromJob(archiveRun, folder, state)
		if err != nil {
			return nil, nil, err
		}
		recordings = append(recordings, recording)
	}
	if len(seen) != len(owned) {
		return nil, nil, ErrStageOneProjection
	}
	return progress, recordings, nil
}

func stageOneState(state job.State) workflow.StepState {
	allCompleted := true
	for _, step := range state.Steps {
		if step.Workflow.State != workflow.Completed {
			allCompleted = false
		}
	}
	if allCompleted {
		return workflow.Completed
	}
	for _, step := range state.Steps {
		if step.Workflow.ID == state.CurrentStepID {
			return step.Workflow.State
		}
	}
	return workflow.Failed
}

func completedStep(state job.State, stepID string) *job.Step {
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.Workflow.ID == stepID && step.Workflow.State == workflow.Completed && step.ExecutionSpec != nil && step.OutputRecord != nil {
			return step
		}
	}
	return nil
}

func storedRecordingFromJob(archiveRun archiverun.State, folder organization.CoordinatedFolder, state job.State) (StoredRecording, error) {
	organized := completedStep(state, "organize")
	if organized == nil || len(organized.OutputRecord.Outputs) != 1 || len(organized.ExecutionSpec.Outputs) != 1 {
		return StoredRecording{}, ErrStageOneProjection
	}
	packageOutput := organized.OutputRecord.Outputs[0]
	packageTarget := organized.ExecutionSpec.Outputs[0]
	parameters := organized.ExecutionSpec.Parameters
	if packageOutput.Kind != execution.OutputOrganizedMedia || packageOutput.ID != packageTarget.ID ||
		packageOutput.StorageID != archiveRun.ArchiveStorageID || len(parameters.SourceInputs) != 1 {
		return StoredRecording{}, ErrStageOneProjection
	}
	source := parameters.SourceInputs[0]
	videoChecksum := source.Checksum
	if folder.Decision.Disposition == vocabulary.OrganizationRenderRecordings {
		rendered := completedStep(state, "render")
		if rendered == nil {
			return StoredRecording{}, ErrStageOneProjection
		}
		videoChecksum = ""
		for _, output := range rendered.OutputRecord.Outputs {
			if output.Kind == execution.OutputRendition {
				videoChecksum = output.Checksum
				break
			}
		}
		if videoChecksum == "" {
			return StoredRecording{}, ErrStageOneProjection
		}
	}
	presentation, err := archive.PresentPackage(packageOutput.ID, folder.Folder, []string{source.OriginalName})
	if err != nil {
		return StoredRecording{}, ErrStageOneProjection
	}
	videoFileName := presentation.VideoFileName
	legacyMetadataPath := path.Join("packages", packageOutput.ID, "revisions", "000001", "metadata.json")
	if packageTarget.RelativePath == legacyMetadataPath {
		videoFileName = "video.mp4"
	}
	return StoredRecording{
		FolderID: folder.Folder, WorkspaceDriveID: parameters.WorkspaceDriveID,
		OriginalAssetID: source.OriginalAssetID, OrganizationDecisionID: parameters.OrganizationDecisionID,
		Disposition: folder.Decision.Disposition, ArchivePackageID: packageOutput.ID, ArchivePackageRevision: 1,
		VideoRelativePath: path.Join(path.Dir(packageTarget.RelativePath), videoFileName),
		VideoChecksum:     videoChecksum, SelectedProfileReference: parameters.SelectedProfileReference,
		SourceInputs: append([]execution.InputReference(nil), parameters.SourceInputs...),
	}, nil
}

var _ StageOneProjection = (*JobStageOneProjection)(nil)
