package workerexecutor

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/execution"
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

type Registry struct {
	roots    *workerconfig.StorageRoots
	handlers map[execution.Operation]Handler
}

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
		if err != nil || size <= 0 || checksum != input.Checksum {
			return inputFailure("INPUT_CHECKSUM_MISMATCH")
		}
	}
	for _, output := range spec.Outputs {
		if _, registered := registry.roots.Root(output.StorageID); !registered {
			return inputFailure("UNREGISTERED_STORAGE")
		}
	}
	outputs, verificationReferences, err := handler.Execute(ctx, spec, registry.roots)
	if err != nil {
		return workerclient.ExecutionResult{
			FailureClass: workflow.FailureInput, ErrorClass: "STEP_EXECUTION_FAILED", LogReference: "worker://operation-failed",
		}
	}
	if _, err := execution.NewStepOutputRecord(
		spec, assignment.FencingToken, "registry-validation", time.Now().UTC(), outputs, verificationReferences,
	); err != nil {
		return inputFailure("INVALID_ADAPTER_OUTPUT")
	}
	return workerclient.ExecutionResult{
		Success: true, Outputs: outputs, VerificationReferences: verificationReferences,
		LogReference: "worker://operation-completed",
	}
}

func inputFailure(errorClass string) workerclient.ExecutionResult {
	return workerclient.ExecutionResult{FailureClass: workflow.FailureInput, ErrorClass: errorClass, LogReference: "worker://input-rejected"}
}
