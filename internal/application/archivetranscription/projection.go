package archivetranscription

import (
	"context"
	"errors"
	"path"
	"strings"

	"videoarchive/internal/application/archiveorganization"
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
	LatestDecisionForArchiveRun(context.Context, string, string) (organization.OrganizationDecision, error)
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
		decision, err := projection.runs.LatestDecisionForArchiveRun(ctx, receipt.ID, folder.Folder)
		if err != nil {
			return nil, nil, err
		}
		folder.Decision = decision
		folder.JobID = archiveorganization.FolderJobID(receipt.ID, receipt.SessionID, receipt.InputRevision, folder.Folder)
		if decision.Disposition == vocabulary.OrganizationExcludeFromRun {
			progress = append(progress, archiverun.FolderProgress{FolderID: folder.Folder, Disposition: decision.Disposition, StageOne: workflow.Completed})
			continue
		}
		if decision.Disposition == vocabulary.OrganizationHold {
			progress = append(progress, archiverun.FolderProgress{
				FolderID: folder.Folder, Disposition: decision.Disposition, StageOne: workflow.Waiting,
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
		folderRecordings, err := storedRecordingsFromJob(archiveRun, folder, state)
		if err != nil {
			return nil, nil, err
		}
		progress[len(progress)-1].TranscriptionEligible = len(folderRecordings) > 0
		recordings = append(recordings, folderRecordings...)
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

func storedRecordingsFromJob(archiveRun archiverun.State, folder organization.CoordinatedFolder, state job.State) ([]StoredRecording, error) {
	organized := completedStep(state, "organize")
	if organized == nil || len(organized.OutputRecord.Outputs) == 0 ||
		len(organized.OutputRecord.Outputs) != len(organized.ExecutionSpec.Outputs) {
		return nil, ErrStageOneProjection
	}
	parameters := organized.ExecutionSpec.Parameters
	if parameters.Disposition != vocabulary.OrganizationAsIs && parameters.Disposition != vocabulary.OrganizationRenderRecordings {
		return nil, ErrStageOneProjection
	}
	sources := make(map[string]execution.InputReference, len(parameters.SourceInputs))
	for _, source := range parameters.SourceInputs {
		if source.OriginalAssetID == "" || sources[source.OriginalAssetID].OriginalAssetID != "" {
			return nil, ErrStageOneProjection
		}
		sources[source.OriginalAssetID] = source
	}
	renderedChecksums := make(map[string]string)
	if parameters.Disposition == vocabulary.OrganizationRenderRecordings {
		rendered := completedStep(state, "render")
		if rendered == nil || len(rendered.OutputRecord.Outputs) != len(rendered.ExecutionSpec.Outputs) {
			return nil, ErrStageOneProjection
		}
		for index, output := range rendered.OutputRecord.Outputs {
			target := rendered.ExecutionSpec.Outputs[index]
			if output.Kind != execution.OutputRendition || target.Kind != execution.OutputRendition ||
				output.ID != target.ID || target.OriginalAssetID == "" || renderedChecksums[target.OriginalAssetID] != "" {
				return nil, ErrStageOneProjection
			}
			renderedChecksums[target.OriginalAssetID] = output.Checksum
		}
	}
	result := make([]StoredRecording, 0, len(organized.OutputRecord.Outputs))
	for index, packageOutput := range organized.OutputRecord.Outputs {
		packageTarget := organized.ExecutionSpec.Outputs[index]
		if packageOutput.Kind != execution.OutputOrganizedMedia || packageTarget.Kind != execution.OutputOrganizedMedia ||
			packageOutput.ID != packageTarget.ID || packageOutput.RelativePath != packageTarget.RelativePath ||
			packageOutput.StorageID != archiveRun.ArchiveStorageID || packageTarget.OriginalAssetID == "" {
			return nil, ErrStageOneProjection
		}
		source, found := sources[packageTarget.OriginalAssetID]
		if !found {
			return nil, ErrStageOneProjection
		}
		if !source.HasAudio {
			continue
		}
		videoChecksum := source.Checksum
		if parameters.Disposition == vocabulary.OrganizationRenderRecordings {
			videoChecksum = renderedChecksums[source.OriginalAssetID]
			if videoChecksum == "" {
				return nil, ErrStageOneProjection
			}
		}
		profileReference := parameters.SelectedProfileReference
		if parameters.Disposition == vocabulary.OrganizationAsIs && source.ProfileReference != "" {
			profileReference = source.ProfileReference
		}
		presentation, err := archive.PresentPackage(packageOutput.ID, folder.Folder, []string{source.OriginalName})
		if err != nil {
			return nil, ErrStageOneProjection
		}
		videoFileName := presentation.VideoFileName
		legacyMetadataPath := path.Join("packages", packageOutput.ID, "revisions", "000001", "metadata.json")
		if packageTarget.RelativePath == legacyMetadataPath {
			videoFileName = "video.mp4"
		}
		result = append(result, StoredRecording{
			FolderID: folder.Folder, WorkspaceDriveID: parameters.WorkspaceDriveID,
			OriginalAssetID: source.OriginalAssetID, OrganizationDecisionID: parameters.OrganizationDecisionID,
			Disposition: parameters.Disposition, ArchivePackageID: packageOutput.ID, ArchivePackageRevision: 1,
			ArchivePackageMetadataPath: packageTarget.RelativePath,
			VideoRelativePath:          path.Join(path.Dir(packageTarget.RelativePath), videoFileName),
			VideoChecksum:              videoChecksum, SelectedProfileReference: profileReference,
			SourceInputs: []execution.InputReference{source}, TranscriptionEligible: true,
		})
	}
	return result, nil
}

var _ StageOneProjection = (*JobStageOneProjection)(nil)
