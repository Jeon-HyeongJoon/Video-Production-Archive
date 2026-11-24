package workerexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/archivecatalog"
	"videoarchive/internal/adapter/ffmpeg"
	"videoarchive/internal/adapter/ffprobe"
	"videoarchive/internal/adapter/mediafolder"
	"videoarchive/internal/adapter/processlog"
	"videoarchive/internal/adapter/subtitlefile"
	"videoarchive/internal/adapter/transcripttext"
	"videoarchive/internal/adapter/whispercpp"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/qc"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/subtitle"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var ErrProductionExecution = errors.New("production MediaOperation failed")

type operationHandler struct {
	operation execution.Operation
	execute   func(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error)
}

type inspectionOperationHandler struct {
	operations *mediaOperations
}

func (handler *inspectionOperationHandler) Operation() execution.Operation {
	return execution.OperationInspectMedia
}

func (handler *inspectionOperationHandler) Execute(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	outputs, references, _, err := handler.ExecuteDetailed(ctx, spec, roots)
	return outputs, references, err
}

func (handler *inspectionOperationHandler) ExecuteDetailed(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, []execution.InspectedMedia, error) {
	return handler.operations.inspectMedia(ctx, spec, roots)
}

func (handler *operationHandler) Operation() execution.Operation { return handler.operation }

func (handler *operationHandler) Execute(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	return handler.execute(ctx, spec, roots)
}

// NewProduction assembles non-fixture MediaOperation Adapters from local tools
// and already-opened StorageRoot capabilities. Direct CLI and managed Runner
// entry points share the returned registry.
func NewProduction(configuration workerconfig.Configuration, roots *workerconfig.StorageRoots) (*Registry, error) {
	if roots == nil || strings.TrimSpace(configuration.ID) == "" || configuration.Tools.FFmpeg.Executable == "" ||
		configuration.Tools.FFprobe.Executable == "" {
		return nil, ErrInvalidRegistry
	}
	var subprocessRunner execution.Runner = ffmpeg.OSRunner{}
	var probeRunner ffprobe.Runner = ffprobe.OSRunner{}
	if configuration.ProcessLogs != nil {
		logRunner, runnerErr := processlog.NewRunner(roots.ProcessLogRoot(), processlog.Policy{
			MaxFileBytes: configuration.ProcessLogs.MaxFileBytes, TailBytes: configuration.ProcessLogs.TailBytes,
			Retention: configuration.ProcessLogs.Retention, MaxTotalBytes: configuration.ProcessLogs.MaxTotalBytes,
		})
		if runnerErr != nil {
			return nil, errors.Join(ErrInvalidRegistry, runnerErr)
		}
		subprocessRunner = logRunner
		probeRunner = logRunner
	}
	inspector, err := ffprobe.NewInspector(
		configuration.Tools.FFprobe.Executable, configuration.Tools.FFprobe.Version, configuration.ID, probeRunner,
	)
	if err != nil {
		return nil, errors.Join(ErrInvalidRegistry, err)
	}
	media := &mediaOperations{configuration: configuration, inspector: inspector, runner: subprocessRunner, now: func() time.Time { return time.Now().UTC() }}
	handlers := []Handler{
		&inspectionOperationHandler{operations: media},
		media.handler(execution.OperationRenderRecording, media.renderRecording),
		media.handler(execution.OperationRenderSourceSequence, media.renderSourceSequence),
		media.handler(execution.OperationQCRendition, media.qcRendition),
		media.handler(execution.OperationOrganizeMedia, media.organizeMedia),
		media.handler(execution.OperationExtractSpeechAudio, media.extractSpeechAudio),
		media.handler(execution.OperationWriteSubtitle, media.writeSubtitle),
		media.handler(execution.OperationWriteTranscriptText, media.writeTranscriptText),
		media.handler(execution.OperationReviseArchivePackage, media.reviseArchivePackage),
		media.handler(execution.OperationWriteArchiveCatalog, media.writeArchiveCatalog),
		media.handler(execution.OperationVerifyArchiveCatalog, media.verifyArchiveCatalog),
		media.handler(execution.OperationRebuildArchiveCatalog, media.rebuildArchiveCatalog),
		media.handler(execution.OperationReconcileArchivePublication, media.reconcileArchivePublication),
	}
	if configuration.Tools.WhisperCPP != nil {
		handlers = append(handlers, media.handler(execution.OperationTranscribeAudio, media.transcribeAudio))
	}
	return New(roots, handlers...)
}

func (operations *mediaOperations) inspectMedia(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, []execution.InspectedMedia, error) {
	if spec.Operation != execution.OperationInspectMedia || len(spec.Outputs) != 1 || len(spec.Inputs) != 0 {
		return nil, nil, nil, ErrProductionExecution
	}
	sourceRoot, found := roots.Root(spec.Parameters.SourceStorageID)
	if !found {
		return nil, nil, nil, ErrProductionExecution
	}
	scanner, err := mediafolder.NewScanner(sourceRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	folders, err := scanner.Discover()
	if err != nil {
		return nil, nil, nil, err
	}
	requested := make(map[string]bool, len(spec.Parameters.FolderIDs))
	for _, folderID := range spec.Parameters.FolderIDs {
		requested[folderID] = true
	}
	foundFolders := make(map[string]bool, len(requested))
	inspected := make([]execution.InspectedMedia, 0)
	for _, folder := range folders {
		if !requested[folder.RelativePath] {
			continue
		}
		foundFolders[folder.RelativePath] = true
		for _, relativePath := range folder.VideoPaths {
			size, checksum, fileErr := sourceRoot.FileSHA256(relativePath)
			if fileErr != nil || size <= 0 {
				return nil, nil, nil, errors.Join(ErrProductionExecution, fileErr)
			}
			identity := []string{spec.Parameters.ArchiveIntakeSessionID, strconv.FormatUint(spec.Parameters.InputRevision, 10), relativePath}
			observationID := stableInspectionID("OBSERVATION", identity...)
			assetID := stableInspectionID("ASSET", identity...)
			inspectionID := stableInspectionID("INSPECTION", append([]string{spec.Parameters.ArchiveRunID}, identity...)...)
			observation, fileErr := intake.NewInputFileObservation(intake.InputFileObservation{
				ID: observationID, StorageID: spec.Parameters.SourceStorageID, WorkspaceDriveID: spec.Parameters.WorkspaceDriveID,
				RelativePath: relativePath, OriginalName: path.Base(relativePath), SizeBytes: size, Checksum: checksum,
				ObservedBy: operations.configuration.ID, ObservedAt: spec.Parameters.RequestedAt.UTC(),
			})
			if fileErr != nil {
				return nil, nil, nil, fileErr
			}
			original, fileErr := asset.RegisterOriginalAsset(assetID, observation, spec.Parameters.RequestedBy, spec.Parameters.RequestedAt.UTC())
			if fileErr != nil {
				return nil, nil, nil, fileErr
			}
			resolved, fileErr := roots.ExternalProcessPath(spec.Parameters.SourceStorageID, relativePath)
			if fileErr != nil {
				return nil, nil, nil, fileErr
			}
			inspection, fileErr := operations.inspector.Inspect(ctx, ffprobe.Request{
				InspectionID: inspectionID, OriginalAssetID: original.ID, OriginalChecksum: original.Checksum, ResolvedInputPath: resolved,
			})
			if fileErr != nil {
				return nil, nil, nil, fileErr
			}
			inspection.InspectedAt = spec.Parameters.RequestedAt.UTC()
			inspection, fileErr = mediaanalysis.NewMediaInspection(inspection)
			if fileErr != nil {
				return nil, nil, nil, fileErr
			}
			inspected = append(inspected, execution.InspectedMedia{
				Folder: folder.RelativePath, Observation: observation, Original: original, Inspection: inspection,
			})
		}
	}
	if len(foundFolders) != len(requested) || len(inspected) == 0 {
		return nil, nil, nil, ErrProductionExecution
	}
	sort.Slice(inspected, func(i, j int) bool {
		return inspected[i].Observation.RelativePath < inspected[j].Observation.RelativePath
	})
	payload, err := json.Marshal(inspected)
	if err != nil {
		return nil, nil, nil, err
	}
	target := spec.Outputs[0]
	outputRoot := roots.MustRoot(target.StorageID)
	if err := outputRoot.MkdirAll(path.Dir(target.RelativePath), 0o755); err != nil {
		return nil, nil, nil, err
	}
	temporary := temporaryOutputPath(ctx, target.RelativePath)
	if err := outputRoot.WriteFile(temporary, payload, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if existing, existsErr := outputRoot.Exists(target.RelativePath); existsErr != nil {
		return nil, nil, nil, existsErr
	} else if existing {
		if removeErr := outputRoot.Remove(target.RelativePath); removeErr != nil {
			return nil, nil, nil, removeErr
		}
	}
	if err := outputRoot.Rename(temporary, target.RelativePath); err != nil {
		return nil, nil, nil, err
	}
	_, checksum, err := outputRoot.FileSHA256(target.RelativePath)
	if err != nil {
		return nil, nil, nil, err
	}
	return []execution.StepOutput{{
		ID: target.ID, Kind: target.Kind, StorageID: target.StorageID, RelativePath: target.RelativePath, Checksum: checksum,
	}}, []string{"worker://media-inspection/" + checksum}, inspected, nil
}

func stableInspectionID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

type mediaOperations struct {
	configuration workerconfig.Configuration
	inspector     *ffprobe.Inspector
	runner        execution.Runner
	now           func() time.Time
}

type archiveStoredPackage struct {
	metadata archivebundle.Metadata
	path     string
}

func (operations *mediaOperations) handler(operation execution.Operation, execute func(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error)) Handler {
	return &operationHandler{operation: operation, execute: execute}
}

type storagePublisher struct {
	receipt           publicationReceipt
	root              *archive.StorageRoot
	temporaryRelative string
	publishedRelative string
	temporaryResolved string
	publishedResolved string
}

func newStoragePublisher(root *archive.StorageRoot, temporaryRelative, publishedRelative string) (*storagePublisher, error) {
	if root == nil || path.Dir(temporaryRelative) != path.Dir(publishedRelative) || temporaryRelative == publishedRelative {
		return nil, ErrProductionExecution
	}
	temporaryResolved, err := root.ExternalProcessPath(temporaryRelative)
	if err != nil {
		return nil, err
	}
	publishedResolved, err := root.ExternalProcessPath(publishedRelative)
	if err != nil {
		return nil, err
	}
	return &storagePublisher{
		root: root, temporaryRelative: temporaryRelative, publishedRelative: publishedRelative,
		temporaryResolved: temporaryResolved, publishedResolved: publishedResolved,
	}, nil
}

func (publisher *storagePublisher) Publish(temporaryPath, publishedPath string) error {
	if temporaryPath != publisher.temporaryResolved || publishedPath != publisher.publishedResolved {
		return ErrProductionExecution
	}
	if publisher.receipt.SpecDigest != "" {
		if err := publisher.recordBeforePublish(); err != nil {
			return err
		}
	}
	return publisher.root.Rename(publisher.temporaryRelative, publisher.publishedRelative)
}

type storageOutputVerifier struct {
	publication *storagePublisher
	ctx         context.Context
	inspector   *ffprobe.Inspector
	root        *archive.StorageRoot
	relative    string
	resolved    string
}

func (verifier storageOutputVerifier) Verify(resolvedPath string) error {
	if resolvedPath != verifier.resolved {
		return ErrProductionExecution
	}
	observed, err := verifier.inspector.InspectStorageOutput(verifier.ctx, verifier.root, verifier.relative)
	if err != nil || !observed.Decodable || observed.SizeBytes <= 0 {
		return errors.Join(ErrProductionExecution, err)
	}
	if verifier.publication != nil {
		verifier.publication.receipt.Size = observed.SizeBytes
		verifier.publication.receipt.Checksum = observed.Checksum
	}
	return nil
}

func (operations *mediaOperations) renderRecording(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != len(spec.Outputs) || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	profile, err := organizationProfile(spec.ProfileReference)
	if err != nil {
		return nil, nil, err
	}
	reused := make(map[string]bool)
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for index, target := range spec.Outputs {
		input := spec.Inputs[index]
		if target.Kind != execution.OutputRendition || target.OriginalAssetID != input.OriginalAssetID {
			return nil, nil, ErrProductionExecution
		}
		inputPath, err := roots.ExternalProcessPath(input.StorageID, input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		outputRoot := roots.MustRoot(target.StorageID)
		temporaryRelative := temporaryOutputPath(ctx, target.RelativePath)
		publisher, err := newStoragePublisher(outputRoot, temporaryRelative, target.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		token, _ := ctx.Value(executionAttemptContextKey{}).(uint64)
		checksum, err := publisher.resume(spec, target, token)
		if err != nil {
			return nil, nil, err
		}
		if checksum != "" {
			reused[target.ID] = true
			outputs = append(outputs, stepOutput(target, checksum))
			continue
		}
		verifier := storageOutputVerifier{publication: publisher, ctx: ctx, inspector: operations.inspector, root: outputRoot, relative: temporaryRelative, resolved: publisher.temporaryResolved}
		renderer, err := ffmpeg.NewRenderer(
			operations.configuration.Tools.FFmpeg.Executable, operations.configuration.Tools.FFmpeg.Version,
			operations.configuration.ID, ffmpeg.WithProgress(operations.runner, spec.Operation, index+1, len(spec.Outputs), input.DurationMillis), verifier, publisher,
		)
		if err != nil {
			return nil, nil, err
		}
		result, err := renderer.Render(ctx, ffmpeg.Request{
			RenditionID: target.ID, OriginalAssetID: input.OriginalAssetID, ResolvedInputPath: inputPath,
			TemporaryOutputPath: publisher.temporaryResolved, PublishedOutputPath: publisher.publishedResolved,
			ArtifactPath: target.RelativePath, TemporaryArtifactPath: temporaryRelative, OutputRoot: outputRoot, Profile: profile,
		})
		if err != nil {
			return nil, nil, err
		}
		if result.Artifact.Checksum != publisher.receipt.Checksum || result.Artifact.Size != publisher.receipt.Size {
			return nil, nil, errPublicationConflict
		}
		outputs = append(outputs, stepOutput(target, result.Artifact.Checksum))
	}
	return outputs, attachPublishReferences(verificationReferences(spec.Operation, outputs), outputs, spec, reused), nil
}

func (operations *mediaOperations) renderSourceSequence(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if spec.Parameters.SourceTimeline == nil || len(spec.Inputs) < 2 || len(spec.Outputs) != 2 {
		return nil, nil, ErrProductionExecution
	}
	profile, err := organizationProfile(spec.ProfileReference)
	if err != nil || profile.TargetVideoBitRateKbps != 20_000 {
		return nil, nil, ErrProductionExecution
	}
	videoTarget, markerTarget, ok := sequenceTargets(spec.Outputs)
	if !ok || videoTarget.StorageID != markerTarget.StorageID {
		return nil, nil, ErrProductionExecution
	}
	resolvedInputs := make(map[string]string, len(spec.Inputs))
	hasAudio := true
	for _, input := range spec.Inputs {
		resolved, err := roots.ExternalProcessPath(input.StorageID, input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		resolvedInputs[input.OriginalAssetID] = resolved
		hasAudio = hasAudio && input.HasAudio
	}
	outputRoot := roots.MustRoot(videoTarget.StorageID)
	temporaryVideo := temporaryOutputPath(ctx, videoTarget.RelativePath)
	publisher, err := newStoragePublisher(outputRoot, temporaryVideo, videoTarget.RelativePath)
	if err != nil {
		return nil, nil, err
	}
	verifier := storageOutputVerifier{ctx: ctx, inspector: operations.inspector, root: outputRoot, relative: temporaryVideo, resolved: publisher.temporaryResolved}
	renderer, err := ffmpeg.NewSequenceRenderer(
		operations.configuration.Tools.FFmpeg.Executable, operations.configuration.Tools.FFmpeg.Version,
		operations.configuration.ID, ffmpeg.WithProgress(operations.runner, spec.Operation, 1, 1, spec.Parameters.SourceTimeline.DurationMillis()), verifier, publisher, outputRoot,
	)
	if err != nil {
		return nil, nil, err
	}
	result, err := renderer.Render(ctx, ffmpeg.SequenceRequest{
		RenditionID: videoTarget.ID, MarkerArtifactID: markerTarget.ID, Timeline: *spec.Parameters.SourceTimeline,
		ResolvedInputPaths: resolvedInputs, HasAudio: hasAudio,
		TemporaryOutputPath: publisher.temporaryResolved, PublishedOutputPath: publisher.publishedResolved,
		ArtifactPath: videoTarget.RelativePath, TemporaryArtifactPath: temporaryVideo, OutputRoot: outputRoot,
		TemporaryMarkerPath: temporaryOutputPath(ctx, markerTarget.RelativePath), PublishedMarkerPath: markerTarget.RelativePath, Profile: profile,
	})
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]string{videoTarget.ID: result.Rendition.Artifact.Checksum, markerTarget.ID: result.MarkerArtifact.Checksum}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for _, target := range spec.Outputs {
		outputs = append(outputs, stepOutput(target, byID[target.ID]))
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (operations *mediaOperations) qcRendition(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != len(spec.Outputs) || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for index, input := range spec.Inputs {
		target := spec.Outputs[index]
		if (input.Kind != execution.OutputRendition && input.Kind != "") || target.Kind != execution.OutputQCAssessment {
			return nil, nil, ErrProductionExecution
		}
		profileReference := spec.ProfileReference
		if input.ProfileReference != "" {
			profileReference = input.ProfileReference
		}
		profile, err := organizationProfile(profileReference)
		if err != nil {
			return nil, nil, err
		}
		inputRoot := roots.MustRoot(input.StorageID)
		size, checksum, err := inputRoot.FileSHA256(input.RelativePath)
		if err != nil || checksum != input.Checksum {
			return nil, nil, ErrProductionExecution
		}
		origin := archive.Provenance{
			OriginalAssetID: input.OriginalAssetID, InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
			ProcessorVersion: operations.configuration.Tools.FFmpeg.Version, ProfileVersion: profile.Reference(),
			CreatedBy: operations.configuration.ID, CreatedAt: operations.now(),
		}
		if spec.Parameters.SourceTimeline != nil {
			origin.OriginalAssetID = ""
			origin.InputTimelineID = spec.Parameters.SourceTimeline.ID
			for _, clip := range spec.Parameters.SourceTimeline.Clips {
				origin.RootOriginalAssetIDs = append(origin.RootOriginalAssetIDs, clip.OriginalAssetID)
			}
		}
		artifactID := input.ArtifactID
		if artifactID == "" {
			artifactID = input.OriginalAssetID
		}
		rendition, err := rendering.NewRendition(archive.Artifact{
			ID: artifactID, Path: input.RelativePath, Size: size, Checksum: checksum, Origin: origin,
		}, profile)
		if err != nil {
			return nil, nil, err
		}
		observed, err := operations.inspector.InspectStorageOutput(ctx, inputRoot, input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		var assessment qc.QCAssessment
		if spec.Parameters.SourceTimeline != nil {
			expectedAudio, audioErr := timelineExpectedAudio(*spec.Parameters.SourceTimeline, spec.Parameters.SourceInputs)
			if audioErr != nil {
				return nil, nil, audioErr
			}
			assessment, err = qc.EvaluateTimeline(target.ID, rendition, profile, *spec.Parameters.SourceTimeline, expectedAudio, observed, qc.Policy{DurationToleranceMillis: 1_500}, operations.configuration.ID, operations.now())
		} else {
			source, sourceErr := operations.sourceInspection(input.OriginalAssetID, spec.Parameters.SourceInputs, roots)
			if sourceErr != nil {
				return nil, nil, sourceErr
			}
			assessment, err = qc.Evaluate(target.ID, rendition, profile, source, observed, qc.Policy{DurationToleranceMillis: 1_500}, operations.configuration.ID, operations.now())
		}
		if err != nil {
			return nil, nil, errors.Join(ErrProductionExecution, err)
		}
		if assessment.Outcome != qc.Passed {
			if failure := assessment.Failure(); failure != nil {
				return nil, nil, failure
			}
			return nil, nil, ErrProductionExecution
		}
		payload, err := json.Marshal(assessment)
		if err != nil {
			return nil, nil, err
		}
		outputRoot := roots.MustRoot(target.StorageID)
		checksum, err = publishPayload(ctx, outputRoot, target.RelativePath, payload)
		if err != nil {
			return nil, nil, err
		}
		outputs = append(outputs, stepOutput(target, checksum))
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func timelineExpectedAudio(timeline organization.SourceTimeline, sources []execution.InputReference) (bool, error) {
	byOriginal := make(map[string]execution.InputReference, len(sources))
	for _, source := range sources {
		byOriginal[source.OriginalAssetID] = source
	}
	for _, clip := range timeline.Clips {
		source, ok := byOriginal[clip.OriginalAssetID]
		if !ok {
			return false, ErrProductionExecution
		}
		if !source.HasAudio {
			return false, nil
		}
	}
	return true, nil
}

func (operations *mediaOperations) sourceInspection(originalAssetID string, sources []execution.InputReference, roots *workerconfig.StorageRoots) (mediaanalysis.MediaInspection, error) {
	for _, source := range sources {
		if source.OriginalAssetID != originalAssetID {
			continue
		}
		size, _, err := roots.MustRoot(source.StorageID).FileSHA256(source.RelativePath)
		if err != nil {
			return mediaanalysis.MediaInspection{}, err
		}
		inspection := mediaanalysis.MediaInspection{
			ID: source.InspectionID, OriginalAssetID: source.OriginalAssetID, OriginalChecksum: source.Checksum,
			Containers: []string{"source"}, DurationMillis: source.DurationMillis, SizeBytes: size, BitRate: source.VideoBitRateBPS,
			VideoStreams: []mediaanalysis.VideoStream{{
				Index: 0, Codec: source.VideoCodec, Width: source.Width, Height: source.Height,
				FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: source.VideoBitRateBPS,
			}},
			ProcessorVersion: operations.configuration.Tools.FFprobe.Version, ObservedBy: operations.configuration.ID, InspectedAt: operations.now(),
		}
		if source.HasAudio {
			inspection.AudioStreams = []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}}
		}
		return mediaanalysis.NewMediaInspection(inspection)
	}
	return mediaanalysis.MediaInspection{}, ErrProductionExecution
}

func (operations *mediaOperations) extractSpeechAudio(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != len(spec.Outputs) || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	profile, err := audio.KoreanSpeechEnhancementProfile(2)
	if err != nil || spec.ProfileReference != profile.Reference() {
		return nil, nil, ErrProductionExecution
	}
	reused := make(map[string]bool)
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for index, target := range spec.Outputs {
		input, ok := inputForOriginal(spec.Inputs, target.OriginalAssetID, execution.OutputRendition, "")
		if !ok || target.Kind != execution.OutputAudio {
			return nil, nil, ErrProductionExecution
		}
		inputPath, err := roots.ExternalProcessPath(input.StorageID, input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		outputRoot := roots.MustRoot(target.StorageID)
		temporaryRelative := temporaryOutputPath(ctx, target.RelativePath)
		publisher, err := newStoragePublisher(outputRoot, temporaryRelative, target.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		token, _ := ctx.Value(executionAttemptContextKey{}).(uint64)
		checksum, err := publisher.resume(spec, target, token)
		if err != nil {
			return nil, nil, err
		}
		if checksum != "" {
			reused[target.ID] = true
			outputs = append(outputs, stepOutput(target, checksum))
			continue
		}
		verifier := storageOutputVerifier{publication: publisher, ctx: ctx, inspector: operations.inspector, root: outputRoot, relative: temporaryRelative, resolved: publisher.temporaryResolved}
		extractor, err := ffmpeg.NewAudioExtractor(
			operations.configuration.Tools.FFmpeg.Executable, operations.configuration.Tools.FFmpeg.Version,
			operations.configuration.ID, ffmpeg.WithProgress(operations.runner, spec.Operation, index+1, len(spec.Outputs), input.DurationMillis), verifier, publisher,
		)
		if err != nil {
			return nil, nil, err
		}
		result, err := extractor.Extract(ctx, ffmpeg.AudioRequest{
			AudioArtifactID: target.ID, OriginalAssetID: input.OriginalAssetID, ResolvedInputPath: inputPath,
			TemporaryOutputPath: publisher.temporaryResolved, PublishedOutputPath: publisher.publishedResolved,
			ArtifactPath: target.RelativePath, TemporaryArtifactPath: temporaryRelative, OutputRoot: outputRoot, Profile: profile,
		})
		if err != nil {
			return nil, nil, err
		}
		if result.Artifact.Checksum != publisher.receipt.Checksum || result.Artifact.Size != publisher.receipt.Size {
			return nil, nil, errPublicationConflict
		}
		outputs = append(outputs, stepOutput(target, result.Artifact.Checksum))
	}
	return outputs, attachPublishReferences(verificationReferences(spec.Operation, outputs), outputs, spec, reused), nil
}

func (operations *mediaOperations) transcribeAudio(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	tool := operations.configuration.Tools.WhisperCPP
	if tool == nil || spec.ProfileReference != tool.Profile.Reference() || len(spec.Inputs) != len(spec.Outputs) {
		return nil, nil, ErrProductionExecution
	}
	modelPath, err := roots.ExternalProcessPath(tool.ModelStorageID, tool.ModelRelativePath)
	if err != nil {
		return nil, nil, err
	}
	audioProfile, err := audio.KoreanSpeechEnhancementProfile(2)
	if err != nil {
		return nil, nil, err
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for index, target := range spec.Outputs {
		input, ok := inputForOriginal(spec.Inputs, target.OriginalAssetID, execution.OutputAudio)
		if !ok || target.Kind != execution.OutputTranscript {
			return nil, nil, ErrProductionExecution
		}
		inputRoot := roots.MustRoot(input.StorageID)
		size, checksum, err := inputRoot.FileSHA256(input.RelativePath)
		if err != nil || checksum != input.Checksum {
			return nil, nil, ErrProductionExecution
		}
		audioArtifact, err := audio.NewAudioArtifact(archive.Artifact{
			ID: input.ArtifactID, Path: input.RelativePath, Size: size, Checksum: checksum,
			Origin: archive.Provenance{
				OriginalAssetID: input.OriginalAssetID, InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingAudioExtract,
				ProcessorVersion: operations.configuration.Tools.FFmpeg.Version, ProfileVersion: audioProfile.Reference(),
				CreatedBy: operations.configuration.ID, CreatedAt: operations.now(),
			},
		}, audioProfile)
		if err != nil {
			return nil, nil, err
		}
		audioPath, err := roots.ExternalProcessPath(input.StorageID, input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		outputRoot := roots.MustRoot(target.StorageID)
		temporaryRelative := temporaryOutputPath(ctx, target.RelativePath)
		publisher, err := newStoragePublisher(outputRoot, temporaryRelative, target.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		transcriber, err := whispercpp.NewTranscriber(tool.Executable, tool.Version, operations.configuration.ID, operations.runner, publisher)
		if err != nil {
			return nil, nil, err
		}
		result, err := transcriber.Transcribe(ctx, whispercpp.Request{
			InputIndex: uint32(index + 1), InputCount: uint32(len(spec.Outputs)),
			TranscriptID: target.ID, AudioArtifact: audioArtifact, InputRoot: inputRoot,
			ResolvedAudioPath: audioPath, ResolvedModelPath: modelPath,
			TemporaryOutputPath: publisher.temporaryResolved, PublishedOutputPath: publisher.publishedResolved,
			ArtifactPath: target.RelativePath, TemporaryArtifactPath: temporaryRelative, OutputRoot: outputRoot, Profile: tool.Profile,
		})
		if err != nil {
			return nil, nil, err
		}
		quality, err := transcription.AssessQuality(result.Content)
		if err != nil {
			return nil, nil, err
		}
		output := stepOutput(target, result.Artifact.Checksum)
		output.TranscriptQuality = &quality
		outputs = append(outputs, output)
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (operations *mediaOperations) writeSubtitle(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != len(spec.Outputs) || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	profile := subtitle.Profile{ID: "event-srt", Version: 1}
	if spec.ProfileReference != profile.Reference() {
		return nil, nil, ErrProductionExecution
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for _, target := range spec.Outputs {
		input, ok := inputForOriginal(spec.Inputs, target.OriginalAssetID, execution.OutputTranscript)
		if !ok || target.Kind != execution.OutputSubtitle {
			return nil, nil, ErrProductionExecution
		}
		root := roots.MustRoot(input.StorageID)
		payload, err := root.ReadFile(input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		transcript, err := operations.decodeStoredTranscript(input, payload)
		if err != nil {
			return nil, nil, ErrProductionExecution
		}
		outputRoot := roots.MustRoot(target.StorageID)
		publisher, err := subtitlefile.NewPublisher(outputRoot, "archive-subtitle-v1", operations.configuration.ID)
		if err != nil {
			return nil, nil, err
		}
		result, err := publisher.Publish(subtitlefile.Request{
			SubtitleID: target.ID, Transcript: transcript, Profile: profile,
			TemporaryPath: temporaryOutputPath(ctx, target.RelativePath), PublishedPath: target.RelativePath,
		})
		if err != nil {
			return nil, nil, err
		}
		outputs = append(outputs, stepOutput(target, result.Artifact.Checksum))
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (operations *mediaOperations) writeTranscriptText(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != len(spec.Outputs) || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for index, target := range spec.Outputs {
		input := spec.Inputs[index]
		if input.Kind != execution.OutputTranscript || target.Kind != execution.OutputTranscriptText || input.OriginalAssetID != target.OriginalAssetID {
			return nil, nil, ErrProductionExecution
		}
		root := roots.MustRoot(input.StorageID)
		if root != roots.MustRoot(target.StorageID) {
			return nil, nil, ErrProductionExecution
		}
		payload, err := root.ReadFile(input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		transcript, err := operations.decodeStoredTranscript(input, payload)
		if err != nil {
			return nil, nil, ErrProductionExecution
		}
		publisher, err := transcripttext.NewPublisher(root, operations.configuration.ID, operations.now)
		if err != nil {
			return nil, nil, err
		}
		artifact, err := publisher.Publish(transcripttext.Request{
			ID: target.ID, Transcript: transcript, TemporaryPath: temporaryOutputPath(ctx, target.RelativePath), PublishedPath: target.RelativePath,
		})
		if err != nil {
			return nil, nil, err
		}
		outputs = append(outputs, stepOutput(target, artifact.Checksum))
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (operations *mediaOperations) decodeStoredTranscript(input execution.InputReference, payload []byte) (transcription.Transcript, error) {
	var document struct {
		SchemaVersion    uint32                `json:"schema_version"`
		ProfileReference string                `json:"profile_reference"`
		Content          transcription.Content `json:"content"`
	}
	if json.Unmarshal(payload, &document) != nil || document.SchemaVersion != 1 {
		return transcription.Transcript{}, ErrProductionExecution
	}
	profileID, profileVersion, ok := splitProfileReference(document.ProfileReference)
	if !ok {
		return transcription.Transcript{}, ErrProductionExecution
	}
	transcript := transcription.Transcript{
		Artifact: archive.Artifact{
			ID: input.ArtifactID, Path: input.RelativePath, Size: int64(len(payload)), Checksum: input.Checksum,
			Origin: archive.Provenance{
				OriginalAssetID: input.OriginalAssetID, InputArtifactIDs: []string{"AUDIO-" + input.OriginalAssetID},
				ProcessingKind: vocabulary.ProcessingSTT, ProcessorVersion: "stored-transcript-v1",
				ProfileVersion: document.ProfileReference, CreatedBy: operations.configuration.ID, CreatedAt: operations.now(),
			},
		},
		ProfileID: profileID, ProfileVersion: profileVersion, Content: document.Content,
	}
	if transcript.Validate() != nil {
		return transcription.Transcript{}, ErrProductionExecution
	}
	return transcript, nil
}

func (operations *mediaOperations) reviseArchivePackage(_ context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Outputs) != 1 || spec.Outputs[0].Kind != execution.OutputArchiveRevision || len(spec.Inputs) != 3 ||
		spec.Parameters.ArchivePackageID == "" || spec.Parameters.ArchivePackageRevision == 0 {
		return nil, nil, ErrProductionExecution
	}
	var transcriptInput, textInput, subtitleInput execution.InputReference
	for _, input := range spec.Inputs {
		switch input.Kind {
		case execution.OutputTranscript:
			transcriptInput = input
		case execution.OutputTranscriptText:
			textInput = input
		case execution.OutputSubtitle:
			subtitleInput = input
		}
	}
	if transcriptInput.ArtifactID == "" || textInput.ArtifactID == "" || subtitleInput.ArtifactID == "" ||
		transcriptInput.StorageID != textInput.StorageID || transcriptInput.StorageID != subtitleInput.StorageID ||
		transcriptInput.StorageID != spec.Outputs[0].StorageID {
		return nil, nil, ErrProductionExecution
	}
	root := roots.MustRoot(spec.Outputs[0].StorageID)
	publisher, err := archivebundle.NewPublisher(root, root)
	if err != nil {
		return nil, nil, err
	}
	revised, err := publisher.ReviseStored(archivebundle.RevisionRequest{
		ExpectedMetadataPath: spec.Outputs[0].RelativePath,
		PackageID:            spec.Parameters.ArchivePackageID,
		BaseRevision:         spec.Parameters.ArchivePackageRevision,
		Transcript:           archivebundle.FileSource{Root: archivebundle.RootArchive, Path: transcriptInput.RelativePath, Checksum: transcriptInput.Checksum},
		TranscriptText:       archivebundle.FileSource{Root: archivebundle.RootArchive, Path: textInput.RelativePath, Checksum: textInput.Checksum},
		Subtitle:             archivebundle.FileSource{Root: archivebundle.RootArchive, Path: subtitleInput.RelativePath, Checksum: subtitleInput.Checksum},
		CreatedAt:            operations.now(),
	})
	if err != nil || revised.Revision != spec.Parameters.ArchivePackageRevision+1 || revised.MetadataPath != spec.Outputs[0].RelativePath {
		return nil, nil, errors.Join(ErrProductionExecution, err)
	}
	_, checksum, err := root.FileSHA256(revised.MetadataPath)
	if err != nil {
		return nil, nil, err
	}
	output := stepOutput(spec.Outputs[0], checksum)
	return []execution.StepOutput{output}, verificationReferences(spec.Operation, []execution.StepOutput{output}), nil
}

// reconcileArchivePublication is the idempotent Worker boundary used by a
// replacement recovery Job. It either confirms/revises the package sidecars
// and then publishes the catalog, or retries an already-materialized catalog
// publication. The source terminal Job is never changed.
func (operations *mediaOperations) reconcileArchivePublication(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Outputs) != 1 || spec.Outputs[0].Kind != execution.OutputArchiveCatalog ||
		spec.Outputs[0].RelativePath != "archive-catalog.csv" || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	catalogSpec := spec
	catalogSpec.Operation = execution.OperationWriteArchiveCatalog
	var transcriptInput, textInput, subtitleInput execution.InputReference
	for _, input := range spec.Inputs {
		switch input.Kind {
		case execution.OutputTranscript:
			transcriptInput = input
		case execution.OutputTranscriptText:
			textInput = input
		case execution.OutputSubtitle:
			subtitleInput = input
		}
	}
	if transcriptInput.ArtifactID != "" || textInput.ArtifactID != "" || subtitleInput.ArtifactID != "" {
		if transcriptInput.ArtifactID == "" || textInput.ArtifactID == "" || subtitleInput.ArtifactID == "" ||
			len(spec.Inputs) != 3 || spec.Parameters.ArchivePackageID == "" ||
			transcriptInput.StorageID != textInput.StorageID || transcriptInput.StorageID != subtitleInput.StorageID ||
			transcriptInput.StorageID != spec.Outputs[0].StorageID {
			return nil, nil, ErrProductionExecution
		}
		root := roots.MustRoot(spec.Outputs[0].StorageID)
		publisher, err := archivebundle.NewPublisher(root, root)
		if err != nil {
			return nil, nil, err
		}
		current, err := publisher.Current(spec.Parameters.ArchivePackageID)
		if err != nil {
			return nil, nil, err
		}
		request := archivebundle.RevisionRequest{
			PackageID:      spec.Parameters.ArchivePackageID,
			Transcript:     archivebundle.FileSource{Root: archivebundle.RootArchive, Path: transcriptInput.RelativePath, Checksum: transcriptInput.Checksum},
			TranscriptText: archivebundle.FileSource{Root: archivebundle.RootArchive, Path: textInput.RelativePath, Checksum: textInput.Checksum},
			Subtitle:       archivebundle.FileSource{Root: archivebundle.RootArchive, Path: subtitleInput.RelativePath, Checksum: subtitleInput.Checksum},
			CreatedAt:      operations.now(),
		}
		if current.Revision > 1 {
			request.BaseRevision = current.Revision - 1
			if replayed, replayErr := publisher.ReviseStored(request); replayErr == nil {
				current = replayed
			} else {
				if !errors.Is(replayErr, archivebundle.ErrPackageState) {
					return nil, nil, replayErr
				}
				request.BaseRevision = current.Revision
				current, err = publisher.ReviseStored(request)
				if err != nil {
					return nil, nil, err
				}
			}
		} else {
			request.BaseRevision = current.Revision
			current, err = publisher.ReviseStored(request)
			if err != nil {
				return nil, nil, err
			}
		}
		_, checksum, err := root.FileSHA256(current.MetadataPath)
		if err != nil {
			return nil, nil, err
		}
		catalogSpec.Inputs = []execution.InputReference{{
			ArtifactID: spec.Parameters.ArchivePackageID, StorageID: spec.Outputs[0].StorageID,
			RelativePath: current.MetadataPath, Checksum: checksum, Kind: execution.OutputArchiveRevision,
		}}
	}
	return operations.writeArchiveCatalog(ctx, catalogSpec, roots)
}

func (operations *mediaOperations) organizeMedia(_ context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Outputs) == 0 || len(spec.Parameters.SourceInputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	outputs := make([]execution.StepOutput, 0, len(spec.Outputs))
	for _, target := range spec.Outputs {
		if target.Kind != execution.OutputOrganizedMedia {
			return nil, nil, ErrProductionExecution
		}
		sourceInputs := sourceInputsForTarget(spec.Parameters.SourceInputs, target.OriginalAssetID)
		if len(sourceInputs) == 0 {
			return nil, nil, ErrProductionExecution
		}
		sourceRoot := roots.MustRoot(sourceInputs[0].StorageID)
		for _, source := range sourceInputs[1:] {
			if roots.MustRoot(source.StorageID) != sourceRoot {
				return nil, nil, ErrProductionExecution
			}
		}
		archiveRoot := roots.MustRoot(target.StorageID)
		publisher, err := archivebundle.NewPublisher(sourceRoot, archiveRoot)
		if err != nil {
			return nil, nil, err
		}
		video, ok := videoInputForTarget(spec.Inputs, target.OriginalAssetID)
		if !ok {
			return nil, nil, ErrProductionExecution
		}
		videoRoot := archivebundle.RootArchive
		if video.Kind == "" {
			videoRoot = archivebundle.RootSource
		}
		profileReference := spec.Parameters.SelectedProfileReference
		if spec.Parameters.Disposition == vocabulary.OrganizationAsIs && len(sourceInputs) == 1 && sourceInputs[0].ProfileReference != "" {
			profileReference = sourceInputs[0].ProfileReference
		}
		request := archivebundle.InitialRequest{
			LayoutVersion: archive.PackageLayoutCompactResultFolder,
			PackageID:     target.ID, WorkspaceDriveID: spec.Parameters.WorkspaceDriveID,
			DecisionID: spec.Parameters.OrganizationDecisionID, MediaFolder: spec.Parameters.MediaFolder,
			Disposition:      spec.Parameters.Disposition,
			ProfileReference: profileReference, CreatedAt: operations.now(),
			Video: archivebundle.FileSource{Root: videoRoot, Path: video.RelativePath, Checksum: video.Checksum},
		}
		for _, source := range sourceInputs {
			request.Sources = append(request.Sources, archivebundle.SourceReference{
				OriginalAssetID: source.OriginalAssetID, OriginalName: source.OriginalName,
				RelativePath: source.RelativePath, Checksum: source.Checksum, DurationMillis: source.DurationMillis,
				SourceCreatedAt: source.SourceCreatedAt, SizeBytes: source.SourceSizeBytes,
				Container: source.SourceContainer, BitRateBPS: source.SourceBitRateBPS,
				VideoCodec: source.VideoCodec, VideoProfile: source.VideoProfile,
				Width: source.Width, Height: source.Height,
				FrameRateNumerator: source.FrameRateNumerator, FrameRateDenominator: source.FrameRateDenominator,
				VideoPixelFormat: source.VideoPixelFormat, VideoBitRateBPS: source.VideoBitRateBPS,
				AudioCodec: source.AudioCodec, AudioProfile: source.AudioProfile,
				AudioBitRateBPS: source.AudioBitRateBPS, AudioSampleRateHz: source.AudioSampleRateHz,
				AudioChannels: source.AudioChannels, AudioChannelLayout: source.AudioChannelLayout,
			})
		}
		bundle, err := publisher.PublishInitialCompact(request)
		if err != nil || bundle.MetadataPath != target.RelativePath {
			return nil, nil, errors.Join(ErrProductionExecution, err)
		}
		_, checksum, err := archiveRoot.FileSHA256(bundle.MetadataPath)
		if err != nil {
			return nil, nil, err
		}
		outputs = append(outputs, stepOutput(target, checksum))
	}
	return outputs, verificationReferences(spec.Operation, outputs), nil
}

func (operations *mediaOperations) writeArchiveCatalog(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Outputs) != 1 || spec.Outputs[0].Kind != execution.OutputArchiveCatalog || len(spec.Inputs) == 0 {
		return nil, nil, ErrProductionExecution
	}
	packages := make([]archiveStoredPackage, 0, len(spec.Inputs))
	for _, input := range spec.Inputs {
		if input.Kind != execution.OutputOrganizedMedia && input.Kind != execution.OutputArchiveRevision {
			return nil, nil, ErrProductionExecution
		}
		payload, err := roots.MustRoot(input.StorageID).ReadFile(input.RelativePath)
		if err != nil {
			return nil, nil, err
		}
		var metadata archivebundle.Metadata
		actual := sha256.Sum256(payload)
		if input.StorageID != spec.Outputs[0].StorageID || hex.EncodeToString(actual[:]) != input.Checksum || json.Unmarshal(payload, &metadata) != nil || metadata.WorkspaceDriveID != spec.Parameters.WorkspaceDriveID || metadata.PackageID != input.ArtifactID {
			return nil, nil, ErrProductionExecution
		}
		packages = append(packages, archiveStoredPackage{metadata: metadata, path: input.RelativePath})
	}
	packageRows := make([]archivecatalog.PackageRow, 0, len(packages))
	allPackageRows := len(packages) > 0
	for _, stored := range packages {
		if stored.metadata.LayoutVersion != archive.PackageLayoutCompactResultFolder &&
			stored.metadata.LayoutVersion != archive.PackageLayoutFlatResultFolder {
			allPackageRows = false
			break
		}
		root := roots.MustRoot(spec.Outputs[0].StorageID)
		bundlePublisher, publisherErr := archivebundle.NewPublisher(root, root)
		if publisherErr != nil {
			return nil, nil, publisherErr
		}
		current, currentErr := bundlePublisher.Current(stored.metadata.PackageID)
		if currentErr != nil {
			return nil, nil, currentErr
		}
		if current.Revision != stored.metadata.Revision || current.MetadataPath != stored.path {
			return nil, nil, archivecatalog.ErrCatalogReconciliation
		}
		row, rowErr := archivecatalog.PackageRowFrom(stored.metadata, current)
		if rowErr != nil {
			return nil, nil, rowErr
		}
		packageRows = append(packageRows, row)
	}
	rows := make([]organization.ArchiveCatalogRow, 0, len(spec.Parameters.SourceInputs))
	for _, source := range spec.Parameters.SourceInputs {
		stored, ok := packageForOriginal(packages, source.OriginalAssetID)
		if !ok {
			return nil, nil, ErrProductionExecution
		}
		profileReference := source.ProfileReference
		if spec.Parameters.Disposition != vocabulary.OrganizationAsIs || profileReference == "" {
			profileReference = spec.Parameters.SelectedProfileReference
		}
		rows = append(rows, organization.ArchiveCatalogRow{
			SourceCreatedAt: source.SourceCreatedAt, SourceName: source.OriginalName,
			ArchiveDate: stored.metadata.ArchiveDate, OutputVideoName: stored.metadata.VideoFileName,
			WorkspaceDriveID: spec.Parameters.WorkspaceDriveID, MediaFolder: spec.Parameters.MediaFolder,
			OriginalAssetID: source.OriginalAssetID, SourceRelativePath: source.RelativePath,
			SourceChecksum: source.Checksum, SourceDurationMillis: source.DurationMillis,
			SourceSizeBytes: source.SourceSizeBytes, SourceContainer: source.SourceContainer,
			SourceBitRateBPS: source.SourceBitRateBPS, SourceVideoCodec: source.VideoCodec,
			SourceVideoProfile: source.VideoProfile, SourceWidth: source.Width, SourceHeight: source.Height,
			SourceFrameRateNumerator: source.FrameRateNumerator, SourceFrameRateDenominator: source.FrameRateDenominator,
			SourceVideoPixelFormat: source.VideoPixelFormat, SourceVideoBitRateBPS: source.VideoBitRateBPS,
			SourceAudioCodec: source.AudioCodec, SourceAudioProfile: source.AudioProfile,
			SourceAudioBitRateBPS: source.AudioBitRateBPS, SourceAudioSampleRateHz: source.AudioSampleRateHz,
			SourceAudioChannels: source.AudioChannels, SourceAudioChannelLayout: source.AudioChannelLayout,
			Disposition: spec.Parameters.Disposition, DecisionID: spec.Parameters.OrganizationDecisionID,
			ArchivePackageID: stored.metadata.PackageID, OutputMediaID: stored.metadata.PackageID,
			OutputVideoPath: stored.metadata.VideoPath, OutputProfile: profileReference,
			TranscriptPath: stored.metadata.TranscriptPath, TranscriptTextPath: stored.metadata.TranscriptTextPath,
			SubtitlePath: stored.metadata.SubtitlePath, Status: packageCatalogStatus(stored.metadata),
		})
	}
	target := spec.Outputs[0]
	root := roots.MustRoot(target.StorageID)
	publisher, err := archivecatalog.NewPublisher(root)
	if err != nil {
		return nil, nil, err
	}
	var result organization.ArchiveCatalog
	if allPackageRows {
		result, err = publisher.PublishPackageRowsMerged(packageRows, temporaryOutputPath(ctx, target.RelativePath), target.RelativePath)
	} else {
		result, err = publisher.PublishMerged(archivecatalog.Request{
			WorkspaceDriveID: spec.Parameters.WorkspaceDriveID, Rows: rows,
			TemporaryPath: temporaryOutputPath(ctx, target.RelativePath), PublishedPath: target.RelativePath,
		})
	}
	if err != nil {
		return nil, nil, err
	}
	output := stepOutput(target, result.Checksum)
	if allPackageRows && spec.Parameters.ArchiveRunID != "" {
		p := execution.CatalogPublication{ArchiveStorageID: target.StorageID, ArchiveRunID: spec.Parameters.ArchiveRunID, CatalogSHA256: result.Checksum}
		for _, row := range packageRows {
			p.Packages = append(p.Packages, execution.PublishedPackage{PackageID: row.PackageID, Revision: row.Revision, ManifestDigest: row.ManifestDigest})
		}
		output.CatalogPublication = &p
	}
	return []execution.StepOutput{output}, verificationReferences(spec.Operation, []execution.StepOutput{output}), nil
}

func (operations *mediaOperations) verifyArchiveCatalog(ctx context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != 0 || len(spec.Outputs) != 1 || spec.Outputs[0].Kind != execution.OutputArchiveCatalogVerification ||
		strings.TrimSpace(spec.Parameters.ArchiveStorageID) == "" {
		return nil, nil, ErrProductionExecution
	}
	archiveRoot := roots.MustRoot(spec.Parameters.ArchiveStorageID)
	reconciler, err := archivecatalog.NewReconciler(archiveRoot)
	if err != nil {
		return nil, nil, err
	}
	started := operations.now().UTC()
	verification, err := reconciler.Verify()
	if err != nil {
		return nil, nil, err
	}
	verification.ArchiveStorageID = spec.Parameters.ArchiveStorageID
	verification.ObservedAt = started
	verification.VolumeID = roots.VolumeID(spec.Parameters.ArchiveStorageID)
	payload, err := json.MarshalIndent(verification, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	payload = append(payload, '\n')
	target := spec.Outputs[0]
	checksum, err := publishPayload(ctx, roots.MustRoot(target.StorageID), target.RelativePath, payload)
	if err != nil {
		return nil, nil, err
	}
	output := stepOutput(target, checksum)
	summary := execution.CatalogVerification{ArchiveStorageID: verification.ArchiveStorageID, ReportID: target.ID, ReportSHA256: checksum, ObservedAt: verification.ObservedAt, CatalogSHA256: verification.CatalogSHA256, LegacyPackages: verification.Coverage.LegacyPackages, UnresolvedVideos: len(verification.Coverage.UnresolvedVideos)}
	for _, entry := range verification.Entries {
		if entry.State == archivecatalog.ConsistencyCurrent {
			summary.CurrentPackages++
		} else {
			summary.MismatchedPackages++
		}
	}
	switch {
	case verification.Coverage.State == "INCOMPLETE":
		summary.State, summary.NextCheck = "INCOMPLETE", "REVIEW_VERIFICATION_COVERAGE"
	case verification.Coverage.State == "EMPTY":
		summary.State, summary.NextCheck = "EMPTY", "VERIFY_AFTER_PUBLICATION"
	case verification.Current():
		summary.State, summary.NextCheck = "CURRENT", "NONE"
	default:
		summary.State, summary.NextCheck = "MISMATCH", "REVIEW_CATALOG_DIFFERENCES"
	}
	output.CatalogVerification = &summary
	return []execution.StepOutput{output}, []string{fmt.Sprintf("archive-catalog://verify/current=%t/entries=%d", verification.Current(), len(verification.Entries))}, nil
}

func (operations *mediaOperations) rebuildArchiveCatalog(_ context.Context, spec execution.StepExecutionSpec, roots *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error) {
	if len(spec.Inputs) != 0 || len(spec.Outputs) != 1 || spec.Outputs[0].Kind != execution.OutputArchiveCatalog ||
		spec.Outputs[0].StorageID != spec.Parameters.ArchiveStorageID || spec.Outputs[0].RelativePath != "archive-catalog.csv" {
		return nil, nil, ErrProductionExecution
	}
	reconciler, err := archivecatalog.NewReconciler(roots.MustRoot(spec.Parameters.ArchiveStorageID))
	if err != nil {
		return nil, nil, err
	}
	repair, err := reconciler.RebuildWithEvidence()
	if err != nil {
		return nil, nil, err
	}
	output := stepOutput(spec.Outputs[0], repair.Catalog.Checksum)
	refs := []string{fmt.Sprintf("archive-catalog://rebuild/rows=%d", repair.Catalog.RowCount), fmt.Sprintf("archive-catalog://rebuild/before=%s/after=%s/current=%t", repair.Before.CatalogSHA256, repair.After.CatalogSHA256, repair.After.Current())}
	for _, entry := range repair.Before.Entries {
		if entry.State != archivecatalog.ConsistencyCurrent {
			refs = append(refs, fmt.Sprintf("archive-catalog://repair/%s/%s/revision=%d-%d/manifest=%s-%s/fields=%s", url.PathEscape(entry.PackageID), entry.State, entry.CatalogRevision, entry.PackageRevision, entry.CatalogDigest, entry.PackageDigest, strings.Join(entry.Differences, ",")))
		}
	}
	return []execution.StepOutput{output}, refs, nil
}

func packageForOriginal(packages []archiveStoredPackage, originalAssetID string) (archiveStoredPackage, bool) {
	for _, stored := range packages {
		for _, source := range stored.metadata.Sources {
			if source.OriginalAssetID == originalAssetID {
				return stored, true
			}
		}
	}
	return archiveStoredPackage{}, false
}

func packageCatalogStatus(metadata archivebundle.Metadata) string {
	if metadata.TranscriptPath != "" && metadata.TranscriptTextPath != "" && metadata.SubtitlePath != "" {
		return "STT_COMPLETED"
	}
	return "STORED"
}

func publishPayload(ctx context.Context, root *archive.StorageRoot, publishedPath string, payload []byte) (string, error) {
	if root == nil {
		return "", ErrProductionExecution
	}
	if directory := path.Dir(publishedPath); directory != "." {
		if err := root.MkdirAll(directory, 0o755); err != nil {
			return "", err
		}
	}
	temporaryPath := temporaryOutputPath(ctx, publishedPath)
	defer root.Remove(temporaryPath)
	if err := root.WriteFile(temporaryPath, payload, 0o600); err != nil {
		return "", err
	}
	verified, err := root.ReadFile(temporaryPath)
	if err != nil || !bytes.Equal(verified, payload) {
		return "", errors.Join(ErrProductionExecution, err)
	}
	if err := root.Rename(temporaryPath, publishedPath); err != nil {
		return "", err
	}
	_, checksum, err := root.FileSHA256(publishedPath)
	return checksum, err
}

func organizationProfile(reference string) (rendering.Profile, error) {
	for _, target := range []int{6_000, 20_000} {
		profile, err := rendering.OrganizationProfile(target)
		if err == nil && profile.Reference() == reference {
			return profile, nil
		}
	}
	return rendering.Profile{}, ErrProductionExecution
}

func sequenceTargets(targets []execution.OutputTarget) (execution.OutputTarget, execution.OutputTarget, bool) {
	var video, markers execution.OutputTarget
	for _, target := range targets {
		switch target.Kind {
		case execution.OutputRendition:
			video = target
		case execution.OutputCutMarkers:
			markers = target
		}
	}
	return video, markers, video.ID != "" && markers.ID != ""
}

func stepOutput(target execution.OutputTarget, checksum string) execution.StepOutput {
	return execution.StepOutput{ID: target.ID, Kind: target.Kind, StorageID: target.StorageID, RelativePath: target.RelativePath, Checksum: checksum}
}

func verificationReferences(operation execution.Operation, outputs []execution.StepOutput) []string {
	references := make([]string, 0, len(outputs))
	for _, output := range outputs {
		references = append(references, fmt.Sprintf("verification://%s/%s/%s", strings.ToLower(string(operation)), output.ID, output.Checksum))
	}
	return references
}

func inputForOriginal(inputs []execution.InputReference, originalAssetID string, kinds ...execution.OutputKind) (execution.InputReference, bool) {
	allowed := make(map[execution.OutputKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	for _, input := range inputs {
		if input.OriginalAssetID == originalAssetID && allowed[input.Kind] {
			return input, true
		}
	}
	return execution.InputReference{}, false
}

func videoInputForTarget(inputs []execution.InputReference, originalAssetID string) (execution.InputReference, bool) {
	if originalAssetID == "" {
		for _, input := range inputs {
			if input.Kind == execution.OutputRendition {
				return input, true
			}
		}
		return execution.InputReference{}, false
	}
	return inputForOriginal(inputs, originalAssetID, execution.OutputRendition, "")
}

func sourceInputsForTarget(inputs []execution.InputReference, originalAssetID string) []execution.InputReference {
	if originalAssetID == "" {
		return append([]execution.InputReference(nil), inputs...)
	}
	for _, input := range inputs {
		if input.OriginalAssetID == originalAssetID {
			return []execution.InputReference{input}
		}
	}
	return nil
}

func splitProfileReference(reference string) (string, uint32, bool) {
	parts := strings.Split(reference, "@v")
	if len(parts) != 2 || parts[0] == "" {
		return "", 0, false
	}
	version, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || version == 0 {
		return "", 0, false
	}
	return parts[0], uint32(version), true
}

func bundleForOriginal(bundles []organization.OrganizedMediaBundle, originalAssetID string) (organization.OrganizedMediaBundle, bool) {
	for _, bundle := range bundles {
		for _, source := range bundle.Sources {
			if source.OriginalAssetID == originalAssetID {
				return bundle, true
			}
		}
	}
	return organization.OrganizedMediaBundle{}, false
}

func sortedOriginalIDs(inputs []execution.InputReference) []string {
	ids := make([]string, 0, len(inputs))
	for _, input := range inputs {
		ids = append(ids, input.OriginalAssetID)
	}
	sort.Strings(ids)
	return ids
}
