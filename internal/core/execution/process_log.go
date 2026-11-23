package execution

import (
	"context"
	"errors"
	"strings"
)

// WhisperLongOutputOpenFailed identifies an observed failure opening the exact
// requested JSON on a Windows path of at least 260 UTF-16 code units.
const WhisperLongOutputOpenFailed = "WHISPER_LONG_OUTPUT_OPEN_FAILED"

type ProcessLogState string

const (
	ProcessLogAvailable          ProcessLogState = "AVAILABLE"
	ProcessLogCaptureFailed      ProcessLogState = "CAPTURE_FAILED"
	ProcessLogExpired            ProcessLogState = "EXPIRED"
	ProcessLogLegacyUnresolvable ProcessLogState = "LEGACY_UNRESOLVABLE"
)

func (state ProcessLogState) Valid() bool {
	return state == ProcessLogAvailable || state == ProcessLogCaptureFailed || state == ProcessLogExpired || state == ProcessLogLegacyUnresolvable
}

// ProcessLogArtifact is the transport-safe part of a Worker-local process
// log. Raw stdout and stderr never cross the Worker boundary.
type ProcessLogArtifact struct {
	ID            string          `json:"id"`
	Reference     string          `json:"reference"`
	State         ProcessLogState `json:"state"`
	Digest        string          `json:"digest,omitempty"`
	CapturedBytes int64           `json:"captured_bytes"`
	TotalBytes    int64           `json:"total_bytes"`
	Truncated     bool            `json:"truncated"`
	SanitizedTail string          `json:"sanitized_tail,omitempty"`
}

func (artifact ProcessLogArtifact) Validate() error {
	if !artifact.State.Valid() || !validEvidenceIdentifier(artifact.ID) ||
		artifact.Reference != "evidence://process-log/"+artifact.ID || artifact.CapturedBytes < 0 ||
		artifact.TotalBytes < artifact.CapturedBytes || artifact.Truncated != (artifact.TotalBytes > artifact.CapturedBytes) ||
		len(artifact.SanitizedTail) > 16<<10 || strings.ContainsAny(artifact.SanitizedTail, "\x00") {
		return ErrInvalidProcessLogArtifact
	}
	if artifact.State == ProcessLogAvailable && (strings.TrimSpace(artifact.Digest) == "" || artifact.CapturedBytes == 0) {
		return ErrInvalidProcessLogArtifact
	}
	if artifact.State != ProcessLogAvailable && artifact.Digest != "" {
		return ErrInvalidProcessLogArtifact
	}
	return nil
}

func validEvidenceIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

var ErrInvalidProcessLogArtifact = errors.New("process log artifact is invalid")

type ProcessContext struct {
	JobID        string
	StepID       string
	Operation    Operation
	FencingToken uint64
}

func (value ProcessContext) Valid() bool {
	return strings.TrimSpace(value.JobID) != "" && strings.TrimSpace(value.StepID) != "" && value.Operation.Valid() && value.FencingToken != 0
}

type processContextKey struct{}

func WithProcessContext(ctx context.Context, value ProcessContext) context.Context {
	return context.WithValue(ctx, processContextKey{}, value)
}

func ProcessContextFrom(ctx context.Context) (ProcessContext, bool) {
	value, ok := ctx.Value(processContextKey{}).(ProcessContext)
	return value, ok && value.Valid()
}

// ProcessFailure preserves a transport-safe artifact while normal error
// wrapping keeps the Adapter's typed failure classification intact.
type ProcessFailure struct {
	Cause    error
	Artifact ProcessLogArtifact
}

func (failure *ProcessFailure) Error() string {
	if failure == nil || failure.Cause == nil {
		return "process execution failed"
	}
	return failure.Cause.Error()
}

func (failure *ProcessFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}
