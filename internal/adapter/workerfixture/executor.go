package workerfixture

import (
	"context"
	"strings"
	"time"

	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/transcription"
)

type Executor struct{ duration time.Duration }

func New() *Executor { return NewWithDuration(100 * time.Millisecond) }

func NewWithDuration(duration time.Duration) *Executor {
	if duration <= 0 {
		duration = 100 * time.Millisecond
	}
	return &Executor{duration: duration}
}

func (executor *Executor) Execute(ctx context.Context, assignment workerclient.Assignment) workerclient.ExecutionResult {
	select {
	case <-ctx.Done():
		return workerclient.ExecutionResult{Cancellation: &workerclient.CancellationExecutionResult{
			ProcessStopped: true, CleanupConfirmed: true, LogReference: "fixture://worker-cancelled-clean",
		}}
	case <-time.After(executor.duration):
		outputs := make([]execution.StepOutput, 0, len(assignment.ExecutionSpec.Outputs))
		for _, target := range assignment.ExecutionSpec.Outputs {
			output := execution.StepOutput{
				ID: target.ID, Kind: target.Kind, StorageID: target.StorageID,
				RelativePath: target.RelativePath, Checksum: strings.Repeat("f", 64),
			}
			if target.Kind == execution.OutputTranscript {
				output.TranscriptQuality = fixtureTranscriptQuality()
			}
			outputs = append(outputs, output)
		}
		return workerclient.ExecutionResult{
			Success: true, LogReference: "fixture://worker-protocol", Outputs: outputs,
			VerificationReferences: []string{"fixture://worker-output-verified"},
		}
	}
}

func fixtureTranscriptQuality() *transcription.QualityAssessment {
	return &transcription.QualityAssessment{
		PolicyReference: transcription.QualityPolicyReference, State: transcription.QualityPassed,
		DurationMillis: 1_000, SegmentCount: 1, TextCharacterCount: 5,
	}
}
