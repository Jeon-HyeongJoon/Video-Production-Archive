package execution

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/transcription"
)

var ErrInvalidStepOutputRecord = errors.New("Step output record is incomplete or does not match its execution spec")

type StepOutput struct {
	CatalogPublication  *CatalogPublication              `json:"catalog_publication,omitempty"`
	CatalogVerification *CatalogVerification             `json:"catalog_verification,omitempty"`
	ID                  string                           `json:"id"`
	Kind                OutputKind                       `json:"kind"`
	StorageID           string                           `json:"storage_id"`
	RelativePath        string                           `json:"relative_path"`
	Checksum            string                           `json:"checksum"`
	TranscriptQuality   *transcription.QualityAssessment `json:"transcript_quality,omitempty"`
}

func (output StepOutput) VerificationReference(operation Operation) string {
	return "verification://" + strings.ToLower(string(operation)) + "/" + output.ID + "/" + output.Checksum
}

type StepOutputRecord struct {
	CatalogPublication     *CatalogPublication `json:"catalog_publication,omitempty"`
	SpecDigest             string              `json:"spec_digest"`
	FencingToken           uint64              `json:"fencing_token"`
	WorkerID               string              `json:"worker_id"`
	RecordedAt             time.Time           `json:"recorded_at"`
	Outputs                []StepOutput        `json:"outputs"`
	VerificationReferences []string            `json:"verification_references"`
	InspectedMedia         []InspectedMedia    `json:"inspected_media,omitempty"`
}

// InspectedMedia is the normalized, path-safe result of one Worker-side
// Archive Intake probe. No mount root or executable path crosses this record.
type InspectedMedia struct {
	Folder      string                        `json:"folder"`
	Observation intake.InputFileObservation   `json:"observation"`
	Original    asset.OriginalAsset           `json:"original"`
	Inspection  mediaanalysis.MediaInspection `json:"inspection"`
}

func NewStepOutputRecord(
	spec StepExecutionSpec,
	fencingToken uint64,
	workerID string,
	recordedAt time.Time,
	outputs []StepOutput,
	verificationReferences []string,
) (StepOutputRecord, error) {
	return NewStepOutputRecordWithInspections(spec, fencingToken, workerID, recordedAt, outputs, verificationReferences, nil)
}

func NewStepOutputRecordWithInspections(
	spec StepExecutionSpec,
	fencingToken uint64,
	workerID string,
	recordedAt time.Time,
	outputs []StepOutput,
	verificationReferences []string,
	inspectedMedia []InspectedMedia,
) (StepOutputRecord, error) {
	if spec.Validate() != nil || fencingToken == 0 || strings.TrimSpace(workerID) == "" || recordedAt.IsZero() ||
		len(outputs) != len(spec.Outputs) || len(verificationReferences) == 0 {
		return StepOutputRecord{}, ErrInvalidStepOutputRecord
	}
	record := StepOutputRecord{
		SpecDigest: spec.Digest, FencingToken: fencingToken, WorkerID: strings.TrimSpace(workerID), RecordedAt: recordedAt.UTC(),
		Outputs: append([]StepOutput(nil), outputs...), VerificationReferences: append([]string(nil), verificationReferences...),
		InspectedMedia: append([]InspectedMedia(nil), inspectedMedia...),
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
		if p := output.CatalogPublication; p != nil {
			if record.CatalogPublication != nil || (spec.Operation != OperationWriteArchiveCatalog && spec.Operation != OperationReconcileArchivePublication) || !p.Matches(spec.Parameters.ArchiveRunID, spec.Parameters.ArchiveStorageID, *output) {
				return StepOutputRecord{}, ErrInvalidStepOutputRecord
			}
			clone := *p
			clone.Packages = append([]PublishedPackage(nil), p.Packages...)
			output.CatalogPublication = nil
			record.CatalogPublication = &clone
		}
		if v := output.CatalogVerification; v != nil {
			if spec.Operation != OperationVerifyArchiveCatalog || !v.Matches(spec.Parameters.ArchiveStorageID, *output) {
				return StepOutputRecord{}, ErrInvalidStepOutputRecord
			}
			clone := *v
			output.CatalogVerification = &clone
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
	if spec.Operation == OperationInspectMedia && spec.TemplateName == "archive-intake-inspection" {
		if len(record.InspectedMedia) == 0 {
			return StepOutputRecord{}, ErrInvalidStepOutputRecord
		}
		seenPaths := make(map[string]bool, len(record.InspectedMedia))
		for index := range record.InspectedMedia {
			item := &record.InspectedMedia[index]
			item.Folder = normalizeRelative(item.Folder)
			observation, observationErr := intake.NewInputFileObservation(item.Observation)
			inspection, inspectionErr := mediaanalysis.NewMediaInspection(item.Inspection)
			if !safeRelative(item.Folder) || observationErr != nil || item.Original.Validate() != nil || inspectionErr != nil ||
				pathFolder(observation.RelativePath) != item.Folder || seenPaths[observation.RelativePath] ||
				item.Original.RegisteredFromObservationID != observation.ID || item.Original.Checksum != observation.Checksum ||
				inspection.OriginalAssetID != item.Original.ID || inspection.OriginalChecksum != item.Original.Checksum ||
				observation.StorageID != spec.Parameters.SourceStorageID || observation.WorkspaceDriveID != spec.Parameters.WorkspaceDriveID ||
				observation.ObservedBy != record.WorkerID || inspection.ObservedBy != record.WorkerID {
				return StepOutputRecord{}, ErrInvalidStepOutputRecord
			}
			seenPaths[observation.RelativePath] = true
			item.Observation = observation
			item.Inspection = inspection
		}
	} else if len(record.InspectedMedia) != 0 {
		return StepOutputRecord{}, ErrInvalidStepOutputRecord
	}
	return record, nil
}

// ReportedOutputs restores the Worker representation when Control settles a record.
func (record StepOutputRecord) ReportedOutputs() []StepOutput {
	if record.CatalogPublication == nil || len(record.Outputs) != 1 {
		return record.Outputs
	}
	outputs := append([]StepOutput(nil), record.Outputs...)
	outputs[0].CatalogPublication = record.CatalogPublication
	return outputs
}

func (record StepOutputRecord) ValidateFor(spec StepExecutionSpec) error {
	normalized, err := NewStepOutputRecordWithInspections(
		spec, record.FencingToken, record.WorkerID, record.RecordedAt, record.ReportedOutputs(), record.VerificationReferences, record.InspectedMedia,
	)
	if err != nil || record.SpecDigest != spec.Digest || !reflect.DeepEqual(record, normalized) {
		return ErrInvalidStepOutputRecord
	}
	return nil
}

func pathFolder(relativePath string) string {
	index := strings.LastIndex(relativePath, "/")
	if index < 0 {
		return "root"
	}
	return relativePath[:index]
}

// PublicationReference identifies the verified output of a fresh or resumed file publication.
func PublicationReference(specDigest string, output StepOutput, reused bool) string {
	scheme := "publish"
	if reused {
		scheme = "publish-reused"
	}
	return scheme + "://" + specDigest + "/" + output.ID + "/" + output.Checksum
}
