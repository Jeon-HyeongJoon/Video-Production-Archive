package archivetranscription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest      = errors.New("invalid archive transcription start request")
	ErrStageOneIncomplete  = errors.New("archive run stage one is not complete")
	ErrInvalidStoredResult = errors.New("stored recording does not belong to the archive run")
)

const StartActionCardCommand = "archive_run.transcription.start"

// CoordinatorActorID is the Control-owned actor that continues an ArchiveRun
// into its separate transcription Jobs when the stage-one gate opens.
const CoordinatorActorID = "archive-transcription-coordinator"

type ArchiveRunStore interface {
	Load(string) (command.Snapshot, error)
}

type JobBus interface {
	Execute(command.Envelope) (command.Result, error)
}

// StoredRecording is a trusted projection of a verified stage-one package.
// It contains only IDs, checksums and Storage-relative paths needed to create
// the first Worker execution spec for a separate stage-two Job.
type StoredRecording struct {
	FolderID                   string
	WorkspaceDriveID           string
	OriginalAssetID            string
	OrganizationDecisionID     string
	Disposition                vocabulary.OrganizationDisposition
	ArchivePackageID           string
	ArchivePackageRevision     uint64
	ArchivePackageMetadataPath string
	VideoRelativePath          string
	VideoChecksum              string
	SelectedProfileReference   string
	SourceInputs               []execution.InputReference
	TranscriptionEligible      bool
}

type StageOneProjection interface {
	StageOne(context.Context, archiverun.State) ([]archiverun.FolderProgress, []StoredRecording, error)
}

type StartRequest struct {
	ArchiveRunID               string
	ExpectedArchiveRunRevision uint64
	IdempotencyKey             string
	Actor                      command.Actor
	StartedAt                  time.Time
}

type Result struct {
	ArchiveRunID  string
	JobIDs        []string
	EligibleCount int
	HoldCount     int
}

type Service struct {
	archiveRuns ArchiveRunStore
	projection  StageOneProjection
	jobs        JobBus
}

func New(archiveRuns ArchiveRunStore, projection StageOneProjection, jobs JobBus) (*Service, error) {
	if archiveRuns == nil || projection == nil || jobs == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{archiveRuns: archiveRuns, projection: projection, jobs: jobs}, nil
}

func (service *Service) Start(ctx context.Context, request StartRequest) (Result, error) {
	request.ArchiveRunID = strings.TrimSpace(request.ArchiveRunID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor.ID = strings.TrimSpace(request.Actor.ID)
	if request.ArchiveRunID == "" || request.ExpectedArchiveRunRevision == 0 || request.IdempotencyKey == "" ||
		(request.Actor.Type != vocabulary.ActorHuman && request.Actor.Type != vocabulary.ActorArchivist &&
			(request.Actor.Type != vocabulary.ActorSystem || request.Actor.ID != CoordinatorActorID)) ||
		request.Actor.ID == "" || request.StartedAt.IsZero() {
		return Result{}, ErrInvalidRequest
	}
	snapshot, err := service.archiveRuns.Load(request.ArchiveRunID)
	if err != nil || snapshot.Revision != request.ExpectedArchiveRunRevision {
		return Result{}, errors.Join(ErrInvalidRequest, err)
	}
	archiveRun, err := archiverun.Decode(snapshot)
	if err != nil {
		return Result{}, errors.Join(ErrInvalidRequest, err)
	}
	folders, recordings, err := service.projection.StageOne(ctx, archiveRun)
	if err != nil {
		return Result{}, err
	}
	gate, err := archiverun.EvaluateTranscriptionGate(folders)
	if err != nil {
		return Result{}, err
	}
	if !gate.Ready {
		return Result{}, ErrStageOneIncomplete
	}
	eligible := make(map[string]bool, len(gate.EligibleFolderIDs))
	for _, folderID := range gate.EligibleFolderIDs {
		eligible[folderID] = true
	}
	sort.Slice(recordings, func(i, j int) bool {
		if recordings[i].FolderID != recordings[j].FolderID {
			return recordings[i].FolderID < recordings[j].FolderID
		}
		return recordings[i].ArchivePackageID < recordings[j].ArchivePackageID
	})
	result := Result{ArchiveRunID: archiveRun.ID, EligibleCount: len(gate.EligibleFolderIDs), HoldCount: gate.HoldCount}
	seenPackages := make(map[string]bool, len(recordings))
	recordingFolders := make(map[string]bool, len(gate.EligibleFolderIDs))
	for _, recording := range recordings {
		if !eligible[recording.FolderID] {
			continue
		}
		if seenPackages[recording.ArchivePackageID] || validateStoredRecording(archiveRun, recording) != nil {
			return Result{}, ErrInvalidStoredResult
		}
		seenPackages[recording.ArchivePackageID] = true
		recordingFolders[recording.FolderID] = true
		jobID, err := service.createJob(request, archiveRun, recording)
		if err != nil {
			return Result{}, err
		}
		result.JobIDs = append(result.JobIDs, jobID)
	}
	if len(recordingFolders) != len(gate.EligibleFolderIDs) {
		return Result{}, ErrInvalidStoredResult
	}
	return result, nil
}

func (service *Service) createJob(request StartRequest, archiveRun archiverun.State, recording StoredRecording) (string, error) {
	template := pipeline.ArchiveTranscription()
	jobID := stableID("STT-JOB", archiveRun.ID, recording.ArchivePackageID)
	audioProfile, err := audio.KoreanSpeechEnhancementProfile(2)
	if err != nil {
		return "", err
	}
	videoArtifactID := stableID("ARCHIVE-VIDEO", recording.ArchivePackageID, strconv.FormatUint(recording.ArchivePackageRevision, 10))
	input := execution.InputReference{
		OriginalAssetID: recording.OriginalAssetID, ArtifactID: videoArtifactID,
		StorageID: archiveRun.ArchiveStorageID, RelativePath: recording.VideoRelativePath,
		Checksum: recording.VideoChecksum, Kind: execution.OutputRendition, HasAudio: recording.TranscriptionEligible,
	}
	audioID := stableID("AUDIO", recording.OrganizationDecisionID, videoArtifactID)
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: jobID, StepID: "audio", TemplateName: template.Name,
		Operation: execution.OperationExtractSpeechAudio,
		Inputs:    []execution.InputReference{input},
		Outputs: []execution.OutputTarget{{
			ID: audioID, Kind: execution.OutputAudio, StorageID: archiveRun.ArchiveStorageID,
			RelativePath: path.Join("staging", jobID, "audio", audioID+".wav"), OriginalAssetID: recording.OriginalAssetID,
		}},
		ProfileReference: audioProfile.Reference(),
		Parameters: execution.StepExecutionParameters{
			ArchiveRunID:           archiveRun.ID,
			OrganizationDecisionID: recording.OrganizationDecisionID, WorkspaceDriveID: recording.WorkspaceDriveID,
			ArchiveStorageID: archiveRun.ArchiveStorageID,
			MediaFolder:      recording.FolderID, InputRevision: archiveRun.FolderDiscoveryRevision,
			ArchivePackageID: recording.ArchivePackageID, ArchivePackageRevision: recording.ArchivePackageRevision,
			ArchivePackageMetadataPath: recording.ArchivePackageMetadataPath,
			Disposition:                recording.Disposition, SelectedProfileReference: recording.SelectedProfileReference,
			TranscriptionEligible: recording.TranscriptionEligible, SourceInputs: append([]execution.InputReference(nil), recording.SourceInputs...),
		},
	})
	if err != nil {
		return "", err
	}
	commandID := stableID("COMMAND", jobID)
	requestedVia := "APPLICATION"
	if request.Actor.Type == vocabulary.ActorArchivist {
		requestedVia = "mcp"
	}
	result, err := service.jobs.Execute(command.Envelope{
		CommandID: commandID, CommandType: job.CreateCommand, Actor: request.Actor, RequestedVia: requestedVia,
		TargetID: jobID, ExpectedRevision: 0,
		IdempotencyKey: request.IdempotencyKey + ":job:" + recording.ArchivePackageID,
		Parameters: map[string]any{
			"template_name": template.Name, "target_title": recording.FolderID,
			"execution_priority":   resource.Immediate,
			"required_storage_ids": []string{archiveRun.ArchiveStorageID},
			"physical_device_ids":  nonEmptyString(archiveRun.SourcePhysicalDeviceID),
			"step_execution_specs": map[string]execution.StepExecutionSpec{"audio": spec},
		},
		Reason: "archive run transcription start",
	})
	if err != nil {
		return "", err
	}
	return result.TargetID, nil
}

func nonEmptyString(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return []string{value}
}

func validateStoredRecording(archiveRun archiverun.State, recording StoredRecording) error {
	recording.FolderID = strings.TrimSpace(recording.FolderID)
	recording.WorkspaceDriveID = strings.TrimSpace(recording.WorkspaceDriveID)
	recording.OriginalAssetID = strings.TrimSpace(recording.OriginalAssetID)
	recording.OrganizationDecisionID = strings.TrimSpace(recording.OrganizationDecisionID)
	recording.ArchivePackageID = strings.TrimSpace(recording.ArchivePackageID)
	recording.SelectedProfileReference = strings.TrimSpace(recording.SelectedProfileReference)
	if recording.FolderID == "" || recording.WorkspaceDriveID == "" || recording.OriginalAssetID == "" || recording.OrganizationDecisionID == "" ||
		recording.ArchivePackageID == "" || recording.ArchivePackageRevision == 0 || recording.ArchivePackageMetadataPath == "" || recording.VideoRelativePath == "" ||
		recording.VideoChecksum == "" || recording.SelectedProfileReference == "" || len(recording.SourceInputs) == 0 || !recording.TranscriptionEligible ||
		(recording.Disposition != vocabulary.OrganizationAsIs && recording.Disposition != vocabulary.OrganizationRenderRecordings) {
		return ErrInvalidStoredResult
	}
	owned := false
	for _, folderID := range archiveRun.FolderIDs {
		owned = owned || folderID == recording.FolderID
	}
	if !owned {
		return ErrInvalidStoredResult
	}
	return nil
}

func stableID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}
