package workerexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	osexec "os/exec"
	"path"
	"sort"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/archivecatalog"
	"videoarchive/internal/adapter/ffmpeg"
	"videoarchive/internal/adapter/ffprobe"
	"videoarchive/internal/adapter/organizedmedia"
	"videoarchive/internal/adapter/subtitlefile"
	"videoarchive/internal/adapter/transcripttext"
	"videoarchive/internal/adapter/whispercpp"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidRegistry    = errors.New("production MediaOperation registry is invalid")
	ErrDuplicateOperation = errors.New("production MediaOperation has more than one Adapter")
)

type Handler interface {
	Operation() execution.Operation
	Execute(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error)
}

type detailedHandler interface {
	Handler
	ExecuteDetailed(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, []execution.InspectedMedia, error)
}

// MediaOperationPort is the protocol-independent execution boundary shared by
// direct CLI and managed Runner Adapters.
type MediaOperationPort interface {
	ExecuteOperation(context.Context, execution.StepExecutionSpec, uint64) workerclient.ExecutionResult
}

type Registry struct {
	roots    *workerconfig.StorageRoots
	handlers map[execution.Operation]Handler
}

type executionAttemptContextKey struct{}

var _ workerclient.StepExecutor = (*Registry)(nil)
var _ MediaOperationPort = (*Registry)(nil)

func New(roots *workerconfig.StorageRoots, handlers ...Handler) (*Registry, error) {
	if roots == nil || len(handlers) == 0 {
		return nil, ErrInvalidRegistry
	}
	registry := &Registry{roots: roots, handlers: make(map[execution.Operation]Handler, len(handlers))}
	for _, handler := range handlers {
		if handler == nil || !handler.Operation().Valid() {
			return nil, ErrInvalidRegistry
		}
		if _, exists := registry.handlers[handler.Operation()]; exists {
			return nil, ErrDuplicateOperation
		}
		registry.handlers[handler.Operation()] = handler
	}
	return registry, nil
}

func (registry *Registry) Execute(ctx context.Context, assignment workerclient.Assignment) workerclient.ExecutionResult {
	spec := assignment.ExecutionSpec
	if spec.JobID != assignment.JobID || spec.StepID != assignment.StepID {
		return inputFailure("INVALID_STEP_EXECUTION_SPEC")
	}
	return registry.ExecuteOperation(ctx, spec, assignment.FencingToken)
}

func (registry *Registry) ExecuteOperation(ctx context.Context, spec execution.StepExecutionSpec, executionToken uint64) workerclient.ExecutionResult {
	if spec.Validate() != nil || executionToken == 0 {
		return inputFailure("INVALID_STEP_EXECUTION_SPEC")
	}
	handler, exists := registry.handlers[spec.Operation]
	if !exists {
		return inputFailure("UNSUPPORTED_STEP_OPERATION")
	}
	for _, input := range spec.Inputs {
		root, registered := registry.roots.Root(input.StorageID)
		if !registered {
			return inputFailure("UNREGISTERED_STORAGE")
		}
		size, checksum, err := root.FileSHA256(input.RelativePath)
		if err != nil {
			if failure, classified := storageFailure(err, input.StorageID); classified {
				return failure
			}
			return inputFailure("INPUT_CHECKSUM_UNAVAILABLE")
		}
		if size <= 0 || checksum != input.Checksum {
			return inputFailure("INPUT_CHECKSUM_MISMATCH")
		}
	}
	for _, output := range spec.Outputs {
		if _, registered := registry.roots.Root(output.StorageID); !registered {
			return inputFailure("UNREGISTERED_STORAGE")
		}
	}
	var outputs []execution.StepOutput
	var verificationReferences []string
	var inspectedMedia []execution.InspectedMedia
	var err error
	attemptContext := context.WithValue(ctx, executionAttemptContextKey{}, executionToken)
	releaseOutputLocks, lockErr := registry.acquireOutputLocks(attemptContext, spec)
	if lockErr != nil {
		if ctx.Err() != nil {
			return workerclient.ExecutionResult{
				ErrorClass: "STEP_EXECUTION_CANCELLED", LogReference: "media://output-lock-cancelled",
				Cancellation: &workerclient.CancellationExecutionResult{
					ProcessStopped: true, CleanupConfirmed: true, LogReference: "media://output-lock-cancelled",
				},
			}
		}
		if failure, classified := storageFailure(lockErr, operationStorageID(spec)); classified {
			return failure
		}
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureTransient, ErrorClass: "OUTPUT_LOCK_UNAVAILABLE",
			CauseCode: "OUTPUT_LOCK_UNAVAILABLE", ErrorSummary: "The media output lock could not be acquired",
			LogReference: "media://output-lock-failed",
		}
	}
	defer releaseOutputLocks()
	if detailed, ok := handler.(detailedHandler); ok {
		outputs, verificationReferences, inspectedMedia, err = detailed.ExecuteDetailed(attemptContext, spec, registry.roots)
	} else {
		outputs, verificationReferences, err = handler.Execute(attemptContext, spec, registry.roots)
	}
	if err != nil {
		if ctx.Err() != nil {
			cleanupConfirmed, externalEffects := registry.cleanupCancelledOutputs(spec, executionToken)
			return workerclient.ExecutionResult{
				ErrorClass: "STEP_EXECUTION_CANCELLED", LogReference: "worker://operation-cancelled",
				Cancellation: &workerclient.CancellationExecutionResult{
					ProcessStopped: true, CleanupConfirmed: cleanupConfirmed,
					ExternalEffectReferences: externalEffects, LogReference: "worker://operation-cancelled",
				},
			}
		}
		if failure, classified := storageFailure(err, operationStorageID(spec)); classified {
			return failure
		}
		causeCode, summary, exitCode := operationFailureDetails(spec.Operation, err)
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED",
			CauseCode: causeCode, ErrorSummary: summary, ProcessExitCode: exitCode,
			LogReference: "worker://operation-failed",
		}
	}
	validationWorkerID := "registry-validation"
	if len(inspectedMedia) > 0 {
		validationWorkerID = inspectedMedia[0].Observation.ObservedBy
	}
	if _, err := execution.NewStepOutputRecordWithInspections(
		spec, executionToken, validationWorkerID, time.Now().UTC(), outputs, verificationReferences, inspectedMedia,
	); err != nil {
		return inputFailure("INVALID_ADAPTER_OUTPUT")
	}
	return workerclient.ExecutionResult{
		Success: true, Outputs: outputs, VerificationReferences: verificationReferences, InspectedMedia: inspectedMedia,
		LogReference: "worker://operation-completed",
	}
}

// ExecuteDirect is the protocol-independent MediaOperation entry point used by
// the standalone CLI. Managed runners call Execute with their lease token; both
// paths reach the same output locking and Adapter registry.
func (registry *Registry) ExecuteDirect(ctx context.Context, spec execution.StepExecutionSpec, executionToken uint64) workerclient.ExecutionResult {
	return registry.ExecuteOperation(ctx, spec, executionToken)
}

func (registry *Registry) acquireOutputLocks(ctx context.Context, spec execution.StepExecutionSpec) (func(), error) {
	type target struct {
		key       string
		storageID string
		lockPath  string
	}
	unique := make(map[string]target, len(spec.Outputs))
	for _, output := range spec.Outputs {
		key := output.StorageID + "\x00" + output.RelativePath
		digest := sha256.Sum256([]byte(key))
		unique[key] = target{
			key: key, storageID: output.StorageID,
			lockPath: path.Join(".archive-locks", "media-operations", hex.EncodeToString(digest[:])+".lock"),
		}
	}
	targets := make([]target, 0, len(unique))
	for _, candidate := range unique {
		targets = append(targets, candidate)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].key < targets[j].key })
	locks := make([]*archive.StorageLock, 0, len(targets))
	release := func() {
		for index := len(locks) - 1; index >= 0; index-- {
			_ = locks[index].Close()
		}
	}
	for _, candidate := range targets {
		root, registered := registry.roots.Root(candidate.storageID)
		if !registered {
			release()
			return func() {}, ErrInvalidRegistry
		}
		if err := root.MkdirAll(path.Dir(candidate.lockPath), 0o755); err != nil {
			release()
			return func() {}, err
		}
		lock, err := root.AcquireExclusiveLock(ctx, candidate.lockPath)
		if err != nil {
			release()
			return func() {}, err
		}
		locks = append(locks, lock)
	}
	return release, nil
}

func operationStorageID(spec execution.StepExecutionSpec) string {
	if len(spec.Outputs) > 0 {
		return spec.Outputs[0].StorageID
	}
	if len(spec.Inputs) > 0 {
		return spec.Inputs[0].StorageID
	}
	return ""
}

func storageFailure(err error, storageID string) (workerclient.ExecutionResult, bool) {
	if errors.Is(err, archive.ErrStorageFull) && storageID != "" {
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureResource, WaitReason: vocabulary.WaitDevice, WaitTargetStorageID: storageID,
			ErrorClass: "STORAGE_FULL", CauseCode: "STORAGE_FULL", ErrorSummary: "The target Storage has insufficient free space",
			LogReference: "worker://storage-full",
		}, true
	}
	if errors.Is(err, archive.ErrStorageUnavailable) && storageID != "" {
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureResource, WaitReason: vocabulary.WaitDevice, WaitTargetStorageID: storageID,
			ErrorClass: "STORAGE_UNAVAILABLE", CauseCode: "STORAGE_UNAVAILABLE", ErrorSummary: "The required Storage is unavailable",
			LogReference: "worker://storage-unavailable",
		}, true
	}
	return workerclient.ExecutionResult{}, false
}

func (registry *Registry) cleanupCancelledOutputs(spec execution.StepExecutionSpec, fencingToken uint64) (bool, []string) {
	cleanupConfirmed := true
	externalEffects := make([]string, 0)
	for _, output := range spec.Outputs {
		root, registered := registry.roots.Root(output.StorageID)
		if !registered {
			cleanupConfirmed = false
			continue
		}
		partial := temporaryOutputPathForToken(output.RelativePath, fencingToken)
		if exists, err := root.Exists(partial); err != nil {
			cleanupConfirmed = false
		} else if exists && root.RemoveAll(partial) != nil {
			cleanupConfirmed = false
		}
		if exists, err := root.Exists(output.RelativePath); err != nil {
			cleanupConfirmed = false
		} else if exists {
			externalEffects = append(externalEffects, output.StorageID+":"+output.RelativePath)
			cleanupConfirmed = false
		}
	}
	return cleanupConfirmed, externalEffects
}

func temporaryOutputPath(ctx context.Context, published string) string {
	fencingToken, _ := ctx.Value(executionAttemptContextKey{}).(uint64)
	return temporaryOutputPathForToken(published, fencingToken)
}

func temporaryOutputPathForToken(published string, fencingToken uint64) string {
	if fencingToken == 0 {
		return published + ".partial"
	}
	return fmt.Sprintf("%s.partial.f%d", published, fencingToken)
}

func inputFailure(errorClass string) workerclient.ExecutionResult {
	return workerclient.ExecutionResult{
		FailureClass: workflow.FailureInput, ErrorClass: errorClass, CauseCode: errorClass,
		ErrorSummary: "The Worker rejected the typed execution input", LogReference: "worker://input-rejected",
	}
}

func operationFailureDetails(operation execution.Operation, err error) (string, string, *int) {
	code := string(operation) + "_FAILED"
	summary := map[execution.Operation]string{
		execution.OperationInspectMedia:          "Media inspection operation failed",
		execution.OperationRenderRecording:       "Recording render operation failed",
		execution.OperationRenderSourceSequence:  "Source sequence render operation failed",
		execution.OperationQCRendition:           "Rendition quality verification failed",
		execution.OperationOrganizeMedia:         "Archive media organization operation failed",
		execution.OperationExtractSpeechAudio:    "Speech audio extraction operation failed",
		execution.OperationTranscribeAudio:       "Speech transcription operation failed",
		execution.OperationWriteSubtitle:         "Subtitle publication operation failed",
		execution.OperationWriteTranscriptText:   "Transcript text publication operation failed",
		execution.OperationReviseArchivePackage:  "Archive package revision operation failed",
		execution.OperationWriteArchiveCatalog:   "Archive Catalog publication operation failed",
		execution.OperationVerifyArchiveCatalog:  "Archive Catalog verification operation failed",
		execution.OperationRebuildArchiveCatalog: "Archive Catalog rebuild operation failed",
	}[operation]
	if summary == "" {
		code = "MEDIA_OPERATION_FAILED"
		summary = "Media operation failed"
	}
	var exitError *osexec.ExitError
	if errors.As(err, &exitError) {
		exitCode := exitError.ExitCode()
		return "PROCESS_EXIT_NONZERO", summary, &exitCode
	}
	if causeCode, causeSummary, classified := typedFailureCause(err); classified {
		return causeCode, causeSummary, nil
	}
	return code, summary, nil
}

func typedFailureCause(err error) (string, string, bool) {
	type classification struct {
		target  error
		code    string
		summary string
	}
	classifications := []classification{
		{archivebundle.ErrPackageExists, "ARCHIVE_PACKAGE_ALREADY_EXISTS", "The Archive Package already exists"},
		{archivebundle.ErrPackageState, "ARCHIVE_PACKAGE_STATE_INVALID", "The Archive Package state does not permit this operation"},
		{archivebundle.ErrInvalidRequest, "ARCHIVE_PACKAGE_REQUEST_INVALID", "The Archive Package request failed validation"},
		{archivecatalog.ErrCatalogReconciliation, "ARCHIVE_CATALOG_RECONCILIATION_FAILED", "Archive Catalog reconciliation failed"},
		{archivecatalog.ErrInvalidRequest, "ARCHIVE_CATALOG_REQUEST_INVALID", "The Archive Catalog request failed validation"},
		{archivecatalog.ErrPublishFailed, "ARCHIVE_CATALOG_PUBLISH_FAILED", "Archive Catalog publication failed"},
		{organizedmedia.ErrInvalidRequest, "ORGANIZED_MEDIA_REQUEST_INVALID", "The organized media request failed validation"},
		{organizedmedia.ErrPublishFailed, "ORGANIZED_MEDIA_PUBLISH_FAILED", "Organized media publication failed"},
		{ffmpeg.ErrInvalidRequest, "FFMPEG_REQUEST_INVALID", "The FFmpeg request failed validation"},
		{ffmpeg.ErrRenderFailed, "FFMPEG_RENDER_FAILED", "FFmpeg rendering failed"},
		{ffmpeg.ErrAudioExtractionFailed, "FFMPEG_AUDIO_EXTRACTION_FAILED", "FFmpeg audio extraction failed"},
		{ffprobe.ErrInvalidRequest, "FFPROBE_REQUEST_INVALID", "The FFprobe request failed validation"},
		{ffprobe.ErrProbeFailed, "FFPROBE_EXECUTION_FAILED", "FFprobe execution failed"},
		{ffprobe.ErrInvalidOutput, "FFPROBE_OUTPUT_INVALID", "FFprobe returned invalid structured output"},
		{whispercpp.ErrInvalidRequest, "WHISPER_REQUEST_INVALID", "The speech transcription request failed validation"},
		{whispercpp.ErrTranscriptionFailed, "WHISPER_TRANSCRIPTION_FAILED", "Speech transcription failed"},
		{subtitlefile.ErrInvalidRequest, "SUBTITLE_REQUEST_INVALID", "The subtitle request failed validation"},
		{subtitlefile.ErrPublishFailed, "SUBTITLE_PUBLISH_FAILED", "Subtitle publication failed"},
		{transcripttext.ErrInvalidRequest, "TRANSCRIPT_TEXT_REQUEST_INVALID", "The transcript text request failed validation"},
		{transcripttext.ErrPublishFailed, "TRANSCRIPT_TEXT_PUBLISH_FAILED", "Transcript text publication failed"},
	}
	for _, candidate := range classifications {
		if errors.Is(err, candidate.target) {
			return candidate.code, candidate.summary, true
		}
	}
	return "", "", false
}
