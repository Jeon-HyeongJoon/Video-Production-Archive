package workerexecutor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidRegistry    = errors.New("production Worker executor registry is invalid")
	ErrDuplicateOperation = errors.New("production Worker operation has more than one Adapter")
)

type Handler interface {
	Operation() execution.Operation
	Execute(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, error)
}

type detailedHandler interface {
	Handler
	ExecuteDetailed(context.Context, execution.StepExecutionSpec, *workerconfig.StorageRoots) ([]execution.StepOutput, []string, []execution.InspectedMedia, error)
}

type Registry struct {
	roots    *workerconfig.StorageRoots
	handlers map[execution.Operation]Handler
}

type executionAttemptContextKey struct{}

var _ workerclient.StepExecutor = (*Registry)(nil)

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
	if spec.Validate() != nil || spec.JobID != assignment.JobID || spec.StepID != assignment.StepID || assignment.FencingToken == 0 {
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
	attemptContext := context.WithValue(ctx, executionAttemptContextKey{}, assignment.FencingToken)
	if detailed, ok := handler.(detailedHandler); ok {
		outputs, verificationReferences, inspectedMedia, err = detailed.ExecuteDetailed(attemptContext, spec, registry.roots)
	} else {
		outputs, verificationReferences, err = handler.Execute(attemptContext, spec, registry.roots)
	}
	if err != nil {
		if ctx.Err() != nil {
			cleanupConfirmed, externalEffects := registry.cleanupCancelledOutputs(spec, assignment.FencingToken)
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
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureTransient, ErrorClass: "STEP_EXECUTION_FAILED", LogReference: "worker://operation-failed",
		}
	}
	validationWorkerID := "registry-validation"
	if len(inspectedMedia) > 0 {
		validationWorkerID = inspectedMedia[0].Observation.ObservedBy
	}
	if _, err := execution.NewStepOutputRecordWithInspections(
		spec, assignment.FencingToken, validationWorkerID, time.Now().UTC(), outputs, verificationReferences, inspectedMedia,
	); err != nil {
		return inputFailure("INVALID_ADAPTER_OUTPUT")
	}
	return workerclient.ExecutionResult{
		Success: true, Outputs: outputs, VerificationReferences: verificationReferences, InspectedMedia: inspectedMedia,
		LogReference: "worker://operation-completed",
	}
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
	if errors.Is(err, archive.ErrStorageFull) {
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureResource, WaitReason: vocabulary.WaitDisk,
			ErrorClass: "STORAGE_FULL", LogReference: "worker://storage-full",
		}, true
	}
	if errors.Is(err, archive.ErrStorageUnavailable) && storageID != "" {
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureResource, WaitReason: vocabulary.WaitDevice, WaitTargetStorageID: storageID,
			ErrorClass: "STORAGE_UNAVAILABLE", LogReference: "worker://storage-unavailable",
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
	return workerclient.ExecutionResult{FailureClass: workflow.FailureInput, ErrorClass: errorClass, LogReference: "worker://input-rejected"}
}
