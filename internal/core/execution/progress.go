package execution

import (
	"context"
	"errors"
	"math"
	"time"
)

var ErrInvalidProgress = errors.New("execution progress is invalid")

type ProgressStatus string

const (
	ProgressCheckingInputs  ProgressStatus = "CHECKING_INPUTS"
	ProgressExecuting       ProgressStatus = "EXECUTING"
	ProgressCheckingOutputs ProgressStatus = "CHECKING_OUTPUTS"
	ProgressProcessing      ProgressStatus = "PROCESSING"
	ProgressPosition        ProgressStatus = "OUTPUT_POSITION"
	ProgressPlanned         ProgressStatus = "PLANNED"
	ProgressStarted         ProgressStatus = "STARTED"
	ProgressCompleted       ProgressStatus = "COMPLETED"
	ProgressFailed          ProgressStatus = "FAILED"
)

// ProgressUpdate is a transport-safe observation emitted by an executing
// operation. It deliberately contains no command line, process output, host
// path, media content, credential, or free-form text.
type ProgressUpdate struct {
	Operation         Operation      `json:"operation"`
	Status            ProgressStatus `json:"status"`
	WorkerObservedAt  time.Time      `json:"worker_observed_at,omitzero"`
	OutputMillis      int64          `json:"output_millis,omitempty"`
	ProcessPercent    uint32         `json:"process_percent,omitempty"`
	InputIndex        uint32         `json:"input_index,omitempty"`
	InputCount        uint32         `json:"input_count,omitempty"`
	ChunkIndex        uint32         `json:"chunk_index"`
	ChunkCount        uint32         `json:"chunk_count"`
	SourceStartMillis int64          `json:"source_start_millis"`
	SourceEndMillis   int64          `json:"source_end_millis"`
	Progress          float64        `json:"progress"`
}

func (status ProgressStatus) IsPhase() bool {
	return status == ProgressCheckingInputs || status == ProgressExecuting || status == ProgressCheckingOutputs
}

// StepFraction weights each input equally. The reported fraction itself remains local to its input.
func (update ProgressUpdate) StepFraction() float64 {
	if update.InputCount == 0 {
		return update.Progress
	}
	return (float64(update.InputIndex-1) + update.Progress) / float64(update.InputCount)
}

func (update ProgressUpdate) Validate() error {
	if !update.Operation.Valid() || math.IsNaN(update.Progress) || update.Progress < 0 || update.Progress > 1 {
		return ErrInvalidProgress
	}
	if update.Status.IsPhase() {
		if update != (ProgressUpdate{Operation: update.Operation, Status: update.Status, WorkerObservedAt: update.WorkerObservedAt}) {
			return ErrInvalidProgress
		}
		return nil
	}
	if (update.InputIndex == 0) != (update.InputCount == 0) || update.InputIndex > update.InputCount {
		return ErrInvalidProgress
	}
	if update.Status == ProgressPosition {
		switch update.Operation {
		case OperationRenderRecording, OperationRenderSourceSequence, OperationExtractSpeechAudio:
		default:
			return ErrInvalidProgress
		}
		if update.OutputMillis <= 0 || update.ChunkIndex != 0 || update.ChunkCount != 0 || update.SourceStartMillis != 0 || update.SourceEndMillis != 0 || update.ProcessPercent != 0 {
			return ErrInvalidProgress
		}
		return nil
	}
	if update.OutputMillis != 0 || update.ChunkCount == 0 || update.SourceStartMillis < 0 || update.SourceEndMillis < update.SourceStartMillis {
		return ErrInvalidProgress
	}
	if update.Status == ProgressProcessing && (update.Operation != OperationTranscribeAudio || update.ProcessPercent == 0) {
		return ErrInvalidProgress
	}
	if update.ProcessPercent > 100 || (update.ProcessPercent != 0 && update.Status != ProgressProcessing && update.Status != ProgressFailed) {
		return ErrInvalidProgress
	}
	switch update.Status {
	case ProgressPlanned:
		if update.ChunkIndex != 0 || update.SourceStartMillis != 0 || update.Progress != 0 {
			return ErrInvalidProgress
		}
	case ProgressProcessing, ProgressStarted, ProgressCompleted, ProgressFailed:
		if update.ChunkIndex == 0 || update.ChunkIndex > update.ChunkCount || update.SourceEndMillis <= update.SourceStartMillis {
			return ErrInvalidProgress
		}
	default:
		return ErrInvalidProgress
	}
	return nil
}

type ProgressReporter func(context.Context, ProgressUpdate) error

type progressReporterKey struct{}

func WithProgressReporter(ctx context.Context, reporter ProgressReporter) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, progressReporterKey{}, reporter)
}

func ReportProgress(ctx context.Context, update ProgressUpdate) error {
	if update.Validate() != nil {
		return ErrInvalidProgress
	}
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(progressReporterKey{}).(ProgressReporter)
	if reporter == nil {
		return nil
	}
	return reporter(ctx, update)
}
