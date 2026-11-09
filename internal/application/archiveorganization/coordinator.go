package archiveorganization

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

	"videoarchive/internal/adapter/mediafolder"
	appintake "videoarchive/internal/application/archiveintake"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	coreintake "videoarchive/internal/core/intake"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
)

var ErrInvalidRequest = errors.New("invalid archive intake request")

type Catalog interface {
	AppendObservation(context.Context, coreintake.InputFileObservation) error
	AppendOriginal(context.Context, asset.OriginalAsset) error
	AppendInspection(context.Context, mediaanalysis.MediaInspection) error
	FindObservation(context.Context, string) (coreintake.InputFileObservation, bool, error)
	FindOriginal(context.Context, string) (asset.OriginalAsset, bool, error)
	FindInspection(context.Context, string) (mediaanalysis.MediaInspection, bool, error)
}

type ReceiptStore interface {
	FindReceipt(context.Context, string) (organization.ArchiveOrganizationReceipt, bool, error)
	SaveReceipt(context.Context, organization.ArchiveOrganizationReceipt) error
}

type JobBus interface {
	Execute(command.Envelope) (command.Result, error)
}

type MediaAnalyzer interface {
	Inspect(context.Context, string, asset.OriginalAsset, string, string) (mediaanalysis.MediaInspection, error)
}

type Request struct {
	ArchiveRunID      string
	IdempotencyKey    string
	InputRevision     uint64
	FolderScope       organization.FolderScope
	Session           *appintake.SessionAccess
	ArchiveStorageID  string
	ActorID           string
	StartedAt         time.Time
	ExecutionPriority resource.ExecutionPriority
}

type Result struct {
	Receipt  organization.ArchiveOrganizationReceipt
	Replayed bool
}

type Coordinator struct {
	catalog  Catalog
	receipts ReceiptStore
	jobs     JobBus
	analyzer MediaAnalyzer
}

func NewCoordinator(catalog Catalog, receipts ReceiptStore, jobs JobBus, analyzer MediaAnalyzer) (*Coordinator, error) {
	if catalog == nil || receipts == nil || jobs == nil || analyzer == nil {
		return nil, ErrInvalidRequest
	}
	return &Coordinator{catalog: catalog, receipts: receipts, jobs: jobs, analyzer: analyzer}, nil
}

func NewInspectedCoordinator(catalog Catalog, receipts ReceiptStore, jobs JobBus) (*Coordinator, error) {
	if catalog == nil || receipts == nil || jobs == nil {
		return nil, ErrInvalidRequest
	}
	return &Coordinator{catalog: catalog, receipts: receipts, jobs: jobs}, nil
}

func (coordinator *Coordinator) Execute(ctx context.Context, request Request) (Result, error) {
	if request.Session == nil || request.Session.Root == nil || strings.TrimSpace(request.ArchiveRunID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || request.InputRevision == 0 || strings.TrimSpace(request.ActorID) == "" ||
		request.StartedAt.IsZero() || request.Session.Session.ID == "" || request.Session.Session.CandidateRevision == 0 ||
		request.Session.Session.WorkspaceDriveID == "" || request.Session.Session.PhysicalDeviceID == "" ||
		request.Session.Session.StorageID == "" || request.Session.Session.WorkerID == "" ||
		strings.TrimSpace(request.ArchiveStorageID) == "" || request.FolderScope.ValidateStart(request.InputRevision) != nil ||
		(request.ExecutionPriority != resource.Background && request.ExecutionPriority != resource.Immediate) {
		return Result{}, ErrInvalidRequest
	}
	digest := requestDigest(request)
	stored, found, err := coordinator.receipts.FindReceipt(ctx, request.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if found {
		if stored.InputDigest != digest {
			return Result{}, command.ErrIdempotencyConflict
		}
		return Result{Receipt: stored, Replayed: true}, nil
	}
	scanner, err := mediafolder.NewScanner(request.Session.Root)
	if err != nil {
		return Result{}, err
	}
	folders, err := scanner.Discover()
	if err != nil || len(folders) == 0 {
		return Result{}, errors.Join(ErrInvalidRequest, err)
	}
	discovered := make(map[string]mediafolder.Folder, len(folders))
	for _, folder := range folders {
		discovered[folder.RelativePath] = folder
	}
	selectedFolders := make([]mediafolder.Folder, 0, len(request.FolderScope.FolderIDs()))
	for _, folderID := range request.FolderScope.FolderIDs() {
		folder, exists := discovered[folderID]
		if !exists {
			return Result{}, ErrInvalidRequest
		}
		selectedFolders = append(selectedFolders, folder)
	}
	receipt := organization.ArchiveOrganizationReceipt{
		ID: strings.TrimSpace(request.ArchiveRunID), IdempotencyKey: strings.TrimSpace(request.IdempotencyKey), InputDigest: digest,
		SessionID: request.Session.Session.ID, InputRevision: request.InputRevision,
		WorkspaceDriveID: request.Session.Session.WorkspaceDriveID, WorkerID: request.Session.Session.WorkerID,
		CompletedAt: request.StartedAt.UTC(),
	}
	for _, folder := range selectedFolders {
		inputs := make([]organization.Input, 0, len(folder.VideoPaths))
		for _, relativePath := range folder.VideoPaths {
			fileLink, input, err := coordinator.intakeFile(ctx, request, relativePath)
			if err != nil {
				return Result{}, err
			}
			receipt.Files = append(receipt.Files, fileLink)
			inputs = append(inputs, input)
		}
		decision, err := organization.Classify(organization.AutoRequest{
			DecisionID:       stableID("DECISION", request.ArchiveRunID, request.Session.Session.ID, strconv.FormatUint(request.InputRevision, 10), folder.RelativePath),
			WorkspaceDriveID: request.Session.Session.WorkspaceDriveID, Folder: folder.RelativePath,
			InputRevision: request.InputRevision, Inputs: inputs, Policy: organization.DefaultPolicy(), DecidedAt: request.StartedAt,
		})
		if err != nil {
			return Result{}, err
		}
		folderResult := organization.CoordinatedFolder{Folder: folder.RelativePath, Decision: decision}
		if decision.Disposition != vocabulary.OrganizationHold {
			jobID, err := coordinator.createFolderJob(request, decision)
			if err != nil {
				return Result{}, err
			}
			folderResult.JobID = jobID
		}
		receipt.Folders = append(receipt.Folders, folderResult)
	}
	if err := coordinator.receipts.SaveReceipt(ctx, receipt); err != nil {
		return Result{}, err
	}
	return Result{Receipt: receipt}, nil
}

// ExecuteInspected continues one ArchiveRun from immutable Worker observations.
// It never opens the source Storage on Control.
func (coordinator *Coordinator) ExecuteInspected(ctx context.Context, request Request, inspected []execution.InspectedMedia) (Result, error) {
	if request.Session == nil || strings.TrimSpace(request.ArchiveRunID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		request.InputRevision == 0 || strings.TrimSpace(request.ActorID) == "" || request.StartedAt.IsZero() ||
		request.Session.Session.ID == "" || request.Session.Session.WorkspaceDriveID == "" || request.Session.Session.PhysicalDeviceID == "" ||
		request.Session.Session.StorageID == "" || request.Session.Session.WorkerID == "" || strings.TrimSpace(request.ArchiveStorageID) == "" ||
		request.FolderScope.ValidateStart(request.InputRevision) != nil || len(inspected) == 0 ||
		(request.ExecutionPriority != resource.Background && request.ExecutionPriority != resource.Immediate) {
		return Result{}, ErrInvalidRequest
	}
	digest := requestDigest(request)
	stored, found, err := coordinator.receipts.FindReceipt(ctx, request.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if found {
		if stored.InputDigest != digest {
			return Result{}, command.ErrIdempotencyConflict
		}
		return Result{Receipt: stored, Replayed: true}, nil
	}
	byFolder := make(map[string][]execution.InspectedMedia, len(request.FolderScope.FolderIDs()))
	for _, item := range inspected {
		if !request.FolderScope.Includes(item.Folder) || item.Observation.StorageID != request.Session.Session.StorageID ||
			item.Observation.WorkspaceDriveID != request.Session.Session.WorkspaceDriveID || item.Observation.ObservedBy != request.Session.Session.WorkerID ||
			item.Inspection.ObservedBy != request.Session.Session.WorkerID || item.Original.RegisteredBy != request.ActorID {
			return Result{}, ErrInvalidRequest
		}
		identity := []string{request.Session.Session.ID, strconv.FormatUint(request.InputRevision, 10), item.Observation.RelativePath}
		if item.Observation.ID != stableID("OBSERVATION", identity...) || item.Original.ID != stableID("ASSET", identity...) ||
			item.Inspection.ID != stableID("INSPECTION", append([]string{request.ArchiveRunID}, identity...)...) {
			return Result{}, ErrInvalidRequest
		}
		byFolder[item.Folder] = append(byFolder[item.Folder], item)
	}
	receipt := organization.ArchiveOrganizationReceipt{
		ID: request.ArchiveRunID, IdempotencyKey: request.IdempotencyKey, InputDigest: digest,
		SessionID: request.Session.Session.ID, InputRevision: request.InputRevision,
		WorkspaceDriveID: request.Session.Session.WorkspaceDriveID, WorkerID: request.Session.Session.WorkerID,
		CompletedAt: request.StartedAt.UTC(),
	}
	for _, folderID := range request.FolderScope.FolderIDs() {
		items := byFolder[folderID]
		if len(items) == 0 {
			return Result{}, ErrInvalidRequest
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Observation.RelativePath < items[j].Observation.RelativePath })
		inputs := make([]organization.Input, 0, len(items))
		for _, item := range items {
			if err := coordinator.appendInspected(ctx, item); err != nil {
				return Result{}, err
			}
			receipt.Files = append(receipt.Files, organization.CoordinatedFile{
				RelativePath: item.Observation.RelativePath, ObservationID: item.Observation.ID,
				OriginalAssetID: item.Original.ID, InspectionID: item.Inspection.ID,
			})
			inputs = append(inputs, organization.Input{
				OriginalAssetID: item.Original.ID, RelativePath: item.Observation.RelativePath, Inspection: item.Inspection,
			})
		}
		decision, err := organization.Classify(organization.AutoRequest{
			DecisionID:       stableID("DECISION", request.ArchiveRunID, request.Session.Session.ID, strconv.FormatUint(request.InputRevision, 10), folderID),
			WorkspaceDriveID: request.Session.Session.WorkspaceDriveID, Folder: folderID,
			InputRevision: request.InputRevision, Inputs: inputs, Policy: organization.DefaultPolicy(), DecidedAt: request.StartedAt,
		})
		if err != nil {
			return Result{}, err
		}
		folderResult := organization.CoordinatedFolder{Folder: folderID, Decision: decision}
		if decision.Disposition != vocabulary.OrganizationHold {
			jobID, err := coordinator.createFolderJob(request, decision)
			if err != nil {
				return Result{}, err
			}
			folderResult.JobID = jobID
		}
		receipt.Folders = append(receipt.Folders, folderResult)
	}
	if err := coordinator.receipts.SaveReceipt(ctx, receipt); err != nil {
		return Result{}, err
	}
	return Result{Receipt: receipt}, nil
}

func (coordinator *Coordinator) appendInspected(ctx context.Context, item execution.InspectedMedia) error {
	if stored, found, err := coordinator.catalog.FindObservation(ctx, item.Observation.ID); err != nil {
		return err
	} else if found {
		if !sameObservedInput(stored, item.Observation) {
			return ErrInvalidRequest
		}
	} else if err := coordinator.catalog.AppendObservation(ctx, item.Observation); err != nil {
		return err
	}
	if stored, found, err := coordinator.catalog.FindOriginal(ctx, item.Original.ID); err != nil {
		return err
	} else if found {
		if !sameOriginalInput(stored, item.Original) {
			return ErrInvalidRequest
		}
	} else if err := coordinator.catalog.AppendOriginal(ctx, item.Original); err != nil {
		return err
	}
	if stored, found, err := coordinator.catalog.FindInspection(ctx, item.Inspection.ID); err != nil {
		return err
	} else if found {
		if stored.OriginalAssetID != item.Original.ID || stored.OriginalChecksum != item.Original.Checksum {
			return ErrInvalidRequest
		}
	} else if err := coordinator.catalog.AppendInspection(ctx, item.Inspection); err != nil {
		return err
	}
	return nil
}

func (coordinator *Coordinator) intakeFile(ctx context.Context, request Request, relativePath string) (organization.CoordinatedFile, organization.Input, error) {
	size, checksum, err := request.Session.Root.FileSHA256(relativePath)
	if err != nil || size <= 0 {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	identity := []string{request.Session.Session.ID, strconv.FormatUint(request.InputRevision, 10), relativePath}
	observationID := stableID("OBSERVATION", identity...)
	assetID := stableID("ASSET", identity...)
	inspectionID := stableID("INSPECTION", append([]string{request.ArchiveRunID}, identity...)...)
	observation, err := coreintake.NewInputFileObservation(coreintake.InputFileObservation{
		ID: observationID, StorageID: request.Session.Session.StorageID, WorkspaceDriveID: request.Session.Session.WorkspaceDriveID,
		RelativePath: relativePath, OriginalName: path.Base(relativePath), SizeBytes: size, Checksum: checksum,
		ObservedBy: request.Session.Session.WorkerID, ObservedAt: request.StartedAt.UTC(),
	})
	if err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	storedObservation, found, err := coordinator.catalog.FindObservation(ctx, observationID)
	if err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	if found {
		if !sameObservedInput(storedObservation, observation) {
			return organization.CoordinatedFile{}, organization.Input{}, ErrInvalidRequest
		}
		observation = storedObservation
	} else if err := coordinator.catalog.AppendObservation(ctx, observation); err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	original, err := asset.RegisterOriginalAsset(assetID, observation, request.ActorID, request.StartedAt.UTC())
	if err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	storedOriginal, found, err := coordinator.catalog.FindOriginal(ctx, assetID)
	if err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	if found {
		if !sameOriginalInput(storedOriginal, original) {
			return organization.CoordinatedFile{}, organization.Input{}, ErrInvalidRequest
		}
		original = storedOriginal
	} else if err := coordinator.catalog.AppendOriginal(ctx, original); err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	inspection, found, err := coordinator.catalog.FindInspection(ctx, inspectionID)
	if err != nil {
		return organization.CoordinatedFile{}, organization.Input{}, err
	}
	if found {
		if inspection.OriginalAssetID != original.ID || inspection.OriginalChecksum != original.Checksum {
			return organization.CoordinatedFile{}, organization.Input{}, ErrInvalidRequest
		}
	} else {
		inspection, err = coordinator.analyzer.Inspect(ctx, inspectionID, original, relativePath, request.Session.Session.WorkerID)
		if err != nil {
			return organization.CoordinatedFile{}, organization.Input{}, err
		}
		if err := coordinator.catalog.AppendInspection(ctx, inspection); err != nil {
			return organization.CoordinatedFile{}, organization.Input{}, err
		}
	}
	return organization.CoordinatedFile{
		RelativePath: relativePath, ObservationID: observation.ID, OriginalAssetID: original.ID, InspectionID: inspection.ID,
	}, organization.Input{OriginalAssetID: original.ID, RelativePath: relativePath, Inspection: inspection}, nil
}

func sameObservedInput(stored, current coreintake.InputFileObservation) bool {
	return stored.ID == current.ID && stored.StorageID == current.StorageID && stored.WorkspaceDriveID == current.WorkspaceDriveID &&
		stored.RelativePath == current.RelativePath && stored.OriginalName == current.OriginalName && stored.SizeBytes == current.SizeBytes &&
		stored.Checksum == current.Checksum && stored.ObservedBy == current.ObservedBy
}

func sameOriginalInput(stored, current asset.OriginalAsset) bool {
	return stored.ID == current.ID && stored.Checksum == current.Checksum && stored.OriginalName == current.OriginalName &&
		stored.SizeBytes == current.SizeBytes && stored.RegisteredFromObservationID == current.RegisteredFromObservationID
}

func (coordinator *Coordinator) createFolderJob(request Request, decision organization.OrganizationDecision) (string, error) {
	template, err := pipeline.ArchiveOrganization(decision.Disposition)
	if err != nil {
		return "", err
	}
	jobID := FolderJobID(request.ArchiveRunID, request.Session.Session.ID, request.InputRevision, decision.Folder)
	spec, err := firstWorkerSpec(request.ArchiveRunID, jobID, template.Name, request.Session.Session.StorageID, request.ArchiveStorageID, decision)
	if err != nil {
		return "", err
	}
	completedControlSteps := []string(nil)
	if decision.Disposition == vocabulary.OrganizationCompileSourceSequence {
		completedControlSteps = []string{"source_timeline"}
	}
	result, err := coordinator.jobs.Execute(command.Envelope{
		CommandID: stableID("COMMAND", jobID), CommandType: job.CreateCommand,
		Actor: command.Actor{Type: vocabulary.ActorSystem, ID: "archive-intake-coordinator"}, RequestedVia: "APPLICATION",
		TargetID: jobID, ExpectedRevision: 0, IdempotencyKey: request.IdempotencyKey + ":job:" + decision.Folder,
		Parameters: map[string]any{
			"template_name": template.Name, "target_title": decision.Folder,
			"execution_priority":         request.ExecutionPriority,
			"required_storage_ids":       []string{request.Session.Session.StorageID, strings.TrimSpace(request.ArchiveStorageID)},
			"physical_device_ids":        []string{request.Session.Session.PhysicalDeviceID},
			"step_execution_specs":       map[string]execution.StepExecutionSpec{spec.StepID: spec},
			"completed_control_step_ids": completedControlSteps,
		},
		Reason: "archive intake decision",
	})
	if err != nil {
		return "", err
	}
	return result.TargetID, nil
}

// FolderJobID is the stable identity of the stage-one Job owned by one folder
// in one ArchiveRun. Resolved HOLD decisions reuse this identity so projections
// never have to choose a Job from a different run by recency or list order.
func FolderJobID(archiveRunID, intakeSessionID string, inputRevision uint64, folder string) string {
	return stableID("JOB", strings.TrimSpace(archiveRunID), strings.TrimSpace(intakeSessionID), strconv.FormatUint(inputRevision, 10), strings.TrimSpace(folder))
}

func firstWorkerSpec(archiveRunID, jobID, templateName, sourceStorageID, workingStorageID string, decision organization.OrganizationDecision) (execution.StepExecutionSpec, error) {
	inputs := make([]execution.InputReference, 0, len(decision.Inputs))
	for _, input := range decision.Inputs {
		video := input.Inspection.VideoStreams[0]
		var audio mediaanalysis.AudioStream
		if len(input.Inspection.AudioStreams) > 0 {
			audio = input.Inspection.AudioStreams[0]
		}
		inputs = append(inputs, execution.InputReference{
			OriginalAssetID: input.OriginalAssetID, OriginalName: path.Base(input.RelativePath), InspectionID: input.Inspection.ID,
			StorageID: sourceStorageID, RelativePath: input.RelativePath, Checksum: input.Inspection.OriginalChecksum,
			DurationMillis: input.Inspection.DurationMillis, SourceCreatedAt: input.Inspection.ContainerCreatedAt,
			SourceSizeBytes: input.Inspection.SizeBytes, SourceContainer: strings.Join(input.Inspection.Containers, ","),
			SourceBitRateBPS: input.Inspection.BitRate, VideoCodec: video.Codec, VideoProfile: video.Profile,
			Width: video.Width, Height: video.Height, FrameRateNumerator: video.FrameRate.Numerator,
			FrameRateDenominator: video.FrameRate.Denominator, VideoPixelFormat: video.PixelFormat,
			VideoBitRateBPS: video.BitRate, HasAudio: len(input.Inspection.AudioStreams) > 0,
			AudioCodec: audio.Codec, AudioProfile: audio.Profile, AudioBitRateBPS: audio.BitRate,
			AudioSampleRateHz: audio.SampleRateHz, AudioChannels: audio.Channels, AudioChannelLayout: audio.ChannelLayout,
			ProfileReference: inputProfileReference(decision, video.BitRate),
		})
	}
	parameters := execution.StepExecutionParameters{
		ArchiveRunID:           archiveRunID,
		OrganizationDecisionID: decision.ID, WorkspaceDriveID: decision.WorkspaceDriveID, MediaFolder: decision.Folder,
		ArchiveStorageID: workingStorageID,
		InputRevision:    decision.InputRevision, ArchiveDate: decision.DecidedAt.UTC().Format("20060102"), Disposition: decision.Disposition,
		SelectedProfileReference: decision.ProfileReference,
		TranscriptionEligible:    organization.TranscriptionEligible(decision, true),
		SourceInputs:             append([]execution.InputReference(nil), inputs...),
	}
	var stepID string
	var operation execution.Operation
	var outputs []execution.OutputTarget
	switch decision.Disposition {
	case vocabulary.OrganizationAsIs:
		stepID, operation = "qc", execution.OperationQCRendition
		parameters.SelectedProfileReference = decision.ProfileReference
		for _, input := range decision.Inputs {
			assessmentID := stableID("QC", decision.ID, input.OriginalAssetID)
			outputs = append(outputs, execution.OutputTarget{
				ID: assessmentID, Kind: execution.OutputQCAssessment, StorageID: workingStorageID,
				RelativePath:    path.Join("staging", jobID, "qc", assessmentID+".json"),
				OriginalAssetID: input.OriginalAssetID,
			})
		}
		parameters.SourceTimeline = nil
		return execution.NewStepExecutionSpec(execution.StepExecutionSpec{
			JobID: jobID, StepID: stepID, TemplateName: templateName, Operation: operation,
			Inputs: inputs, Outputs: outputs, ProfileReference: decision.ProfileReference, Parameters: parameters,
		})
	case vocabulary.OrganizationRenderRecordings:
		stepID, operation = "render", execution.OperationRenderRecording
		outputs = make([]execution.OutputTarget, 0, len(decision.Inputs))
		for _, input := range decision.Inputs {
			renditionID := stableID("RENDITION", decision.ID, input.OriginalAssetID)
			outputs = append(outputs, execution.OutputTarget{
				ID: renditionID, Kind: execution.OutputRendition, StorageID: workingStorageID,
				RelativePath:    path.Join("staging", jobID, "render", renditionID+".mp4"),
				OriginalAssetID: input.OriginalAssetID,
			})
		}
	case vocabulary.OrganizationCompileSourceSequence:
		stepID, operation = "sequence_render", execution.OperationRenderSourceSequence
		timelineID := stableID("TIMELINE", decision.ID)
		timeline, err := organization.BuildSourceTimeline(timelineID, 1, decision)
		if err != nil {
			return execution.StepExecutionSpec{}, err
		}
		parameters.SourceTimeline = &timeline
		renditionID := stableID("RENDITION", decision.ID, timeline.ID)
		outputs = []execution.OutputTarget{
			{ID: renditionID, Kind: execution.OutputRendition, StorageID: workingStorageID, RelativePath: path.Join("staging", jobID, "sequence_render", renditionID+".mp4")},
			{ID: stableID("MARKERS", decision.ID, timeline.ID), Kind: execution.OutputCutMarkers, StorageID: workingStorageID, RelativePath: path.Join("staging", jobID, "sequence_render", renditionID+".markers.json")},
		}
	default:
		return execution.StepExecutionSpec{}, ErrInvalidRequest
	}
	return execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: jobID, StepID: stepID, TemplateName: templateName, Operation: operation,
		Inputs: inputs, Outputs: outputs, ProfileReference: decision.ProfileReference, Parameters: parameters,
	})
}

func inputProfileReference(decision organization.OrganizationDecision, bitRate int64) string {
	if decision.ProfileReference != "" {
		return decision.ProfileReference
	}
	target := 6_000
	if bitRate > 13_000_000 {
		target = 20_000
	}
	profile, err := rendering.OrganizationProfile(target)
	if err != nil {
		return ""
	}
	return profile.Reference()
}

func requestDigest(request Request) string {
	values := []string{
		strings.TrimSpace(request.ArchiveRunID), request.Session.Session.ID, strconv.FormatUint(request.InputRevision, 10),
		request.Session.Session.WorkspaceDriveID, request.Session.Session.PhysicalDeviceID,
		request.Session.Session.StorageID, strings.TrimSpace(request.ArchiveStorageID),
		strconv.FormatUint(request.FolderScope.DiscoveryRevision(), 10), strings.Join(request.FolderScope.FolderIDs(), "\x1f"),
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func stableID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}
