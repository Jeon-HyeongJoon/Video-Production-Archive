package execution

import (
	"context"
	"errors"
)

var ErrInvalidProgress = errors.New("execution progress is invalid")

type ProgressStatus string

const (
	ProgressPlanned   ProgressStatus = "PLANNED"
	ProgressStarted   ProgressStatus = "STARTED"
	ProgressCompleted ProgressStatus = "COMPLETED"
	ProgressFailed    ProgressStatus = "FAILED"
)

// ProgressUpdate is a transport-safe observation emitted by an executing
// operation. It deliberately contains no command line, process output, host
// path, media content, credential, or free-form text.
type ProgressUpdate struct {
	Operation         Operation      `json:"operation"`
	Status            ProgressStatus `json:"status"`
	ChunkIndex        uint32         `json:"chunk_index"`
	ChunkCount        uint32         `json:"chunk_count"`
	SourceStartMillis int64          `json:"source_start_millis"`
	SourceEndMillis   int64          `json:"source_end_millis"`
	Progress          float64        `json:"progress"`
}

func (update ProgressUpdate) Validate() error {
	if !update.Operation.Valid() || update.ChunkCount == 0 || update.SourceStartMillis < 0 ||
		update.SourceEndMillis < update.SourceStartMillis || update.Progress < 0 || update.Progress > 1 {
		return ErrInvalidProgress
	}
	switch update.Status {
	case ProgressPlanned:
		if update.ChunkIndex != 0 || update.SourceStartMillis != 0 || update.Progress != 0 {
			return ErrInvalidProgress
		}
	case ProgressStarted, ProgressCompleted, ProgressFailed:
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
