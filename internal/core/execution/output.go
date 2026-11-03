package execution

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"videoarchive/internal/core/transcription"
)

var ErrInvalidStepOutputRecord = errors.New("Step output record is incomplete or does not match its execution spec")

type StepOutput struct {
	ID                string                           `json:"id"`
	Kind              OutputKind                       `json:"kind"`
	StorageID         string                           `json:"storage_id"`
	RelativePath      string                           `json:"relative_path"`
	Checksum          string                           `json:"checksum"`
	TranscriptQuality *transcription.QualityAssessment `json:"transcript_quality,omitempty"`
}

type StepOutputRecord struct {
	SpecDigest             string       `json:"spec_digest"`
	FencingToken           uint64       `json:"fencing_token"`
	WorkerID               string       `json:"worker_id"`
	RecordedAt             time.Time    `json:"recorded_at"`
	Outputs                []StepOutput `json:"outputs"`
	VerificationReferences []string     `json:"verification_references"`
}

func NewStepOutputRecord(
	spec StepExecutionSpec,
	fencingToken uint64,
	workerID string,
	recordedAt time.Time,
	outputs []StepOutput,
	verificationReferences []string,
) (StepOutputRecord, error) {
	if spec.Validate() != nil || fencingToken == 0 || strings.TrimSpace(workerID) == "" || recordedAt.IsZero() ||
		len(outputs) != len(spec.Outputs) || len(verificationReferences) == 0 {
		return StepOutputRecord{}, ErrInvalidStepOutputRecord
	}
	record := StepOutputRecord{
		SpecDigest: spec.Digest, FencingToken: fencingToken, WorkerID: strings.TrimSpace(workerID), RecordedAt: recordedAt.UTC(),
		Outputs: append([]StepOutput(nil), outputs...), VerificationReferences: append([]string(nil), verificationReferences...),
	}
	for index := range record.Outputs {
		output := &record.Outputs[index]
		output.ID = strings.TrimSpace(output.ID)
		output.StorageID = strings.TrimSpace(output.StorageID)
		output.RelativePath = normalizeRelative(output.RelativePath)
		output.Checksum = strings.ToLower(strings.TrimSpace(output.Checksum))
		target := spec.Outputs[index]
		if output.ID != target.ID || output.Kind != target.Kind || output.StorageID != target.StorageID ||
			output.RelativePath != target.RelativePath || !sha256Pattern.MatchString(output.Checksum) {
			return StepOutputRecord{}, ErrInvalidStepOutputRecord
		}
		if output.Kind == OutputTranscript {
			if output.TranscriptQuality == nil || output.TranscriptQuality.Validate() != nil {
				return StepOutputRecord{}, ErrInvalidStepOutputRecord
			}
			quality := *output.TranscriptQuality
			quality.Reasons = append([]transcription.QualityReason(nil), output.TranscriptQuality.Reasons...)
			output.TranscriptQuality = &quality
		} else if output.TranscriptQuality != nil {
			return StepOutputRecord{}, ErrInvalidStepOutputRecord
		}
	}
	seenReferences := make(map[string]bool, len(record.VerificationReferences))
	for index, raw := range record.VerificationReferences {
		reference := strings.TrimSpace(raw)
		if reference == "" || seenReferences[reference] {
			return StepOutputRecord{}, ErrInvalidStepOutputRecord
		}
		seenReferences[reference] = true
		record.VerificationReferences[index] = reference
	}
	return record, nil
}

func (record StepOutputRecord) ValidateFor(spec StepExecutionSpec) error {
	normalized, err := NewStepOutputRecord(
		spec, record.FencingToken, record.WorkerID, record.RecordedAt, record.Outputs, record.VerificationReferences,
	)
	if err != nil || record.SpecDigest != spec.Digest || !reflect.DeepEqual(record, normalized) {
		return ErrInvalidStepOutputRecord
	}
	return nil
}
