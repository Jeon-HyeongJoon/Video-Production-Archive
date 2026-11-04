package execution

import (
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

func TestMaterializeArchiveTranscriptionUsesVerifiedOutputsAndArchivePackageRevision(t *testing.T) {
	parameters := StepExecutionParameters{
		OrganizationDecisionID: "DECISION-1", WorkspaceDriveID: "DRIVE-1", ArchiveStorageID: "ARCHIVE-1",
		MediaFolder: "recordings", InputRevision: 3, ArchivePackageID: "VP-20261031-0001", ArchivePackageRevision: 1,
		Disposition: vocabulary.OrganizationRenderRecordings, SelectedProfileReference: "archive-1080p-6mbps@v1",
		TranscriptionEligible: true,
		SourceInputs: []InputReference{{
			OriginalAssetID: "ASSET-1", OriginalName: "master.mov", StorageID: "SOURCE-1",
			RelativePath: "recordings/master.mov", Checksum: strings.Repeat("a", 64),
		}},
	}
	audio := mustSpec(t, StepExecutionSpec{
		JobID: "STT-JOB-1", StepID: "audio", TemplateName: "archive-transcription", Operation: OperationExtractSpeechAudio,
		Inputs: []InputReference{{
			OriginalAssetID: "ASSET-1", ArtifactID: "VIDEO-1", StorageID: "ARCHIVE-1",
			RelativePath: "packages/VP-20261031-0001/revisions/000001/video.mp4", Checksum: strings.Repeat("b", 64), Kind: OutputRendition,
		}},
		Outputs: []OutputTarget{{
			ID: "AUDIO-1", Kind: OutputAudio, StorageID: "ARCHIVE-1", RelativePath: "staging/STT-JOB-1/audio/AUDIO-1.wav", OriginalAssetID: "ASSET-1",
		}},
		ProfileReference: "speech-ko-enhanced-16k@v1", Parameters: parameters,
	})
	completed := []CompletedStep{completeSpec(t, audio, "c")}
	transcript := materialize(t, "transcript", completed)
	if transcript.Operation != OperationTranscribeAudio || transcript.Inputs[0].ArtifactID != "AUDIO-1" {
		t.Fatalf("transcript did not use the recorded Audio output: %+v", transcript)
	}
	completed = append(completed, completeSpec(t, transcript, "d"))
	subtitle := materialize(t, "subtitle", completed)
	text := materialize(t, "transcript_text", completed)
	if subtitle.Operation != OperationWriteSubtitle || text.Operation != OperationWriteTranscriptText ||
		subtitle.Inputs[0].ArtifactID != transcript.Outputs[0].ID || text.Inputs[0].ArtifactID != transcript.Outputs[0].ID {
		t.Fatalf("parallel transcript derivatives are invalid: subtitle=%+v text=%+v", subtitle, text)
	}
	completed = append(completed, completeSpec(t, subtitle, "e"), completeSpec(t, text, "f"))
	revision := materialize(t, "archive_package_revision", completed)
	if revision.Operation != OperationReviseArchivePackage || len(revision.Inputs) != 3 ||
		len(revision.Outputs) != 1 || revision.Outputs[0].Kind != OutputArchiveRevision ||
		revision.Outputs[0].StorageID != "ARCHIVE-1" ||
		revision.Outputs[0].RelativePath != "packages/VP-20261031-0001/revisions/000002/metadata.json" {
		t.Fatalf("archive package revision spec is invalid: %+v", revision)
	}
	completed = append(completed, completeSpec(t, revision, "1"))
	catalog := materialize(t, "archive_catalog", completed)
	if catalog.Operation != OperationWriteArchiveCatalog || catalog.Inputs[0].Kind != OutputArchiveRevision ||
		catalog.Outputs[0].StorageID != "ARCHIVE-1" || catalog.Outputs[0].RelativePath != "catalog/DRIVE-1.csv" {
		t.Fatalf("stage-two ArchiveCatalog spec is invalid: %+v", catalog)
	}
}

func materialize(t *testing.T, stepID string, completed []CompletedStep) StepExecutionSpec {
	t.Helper()
	spec, err := MaterializeDependentStepExecutionSpec(MaterializeRequest{
		JobID: "STT-JOB-1", TemplateName: "archive-transcription", StepID: stepID, Completed: completed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func mustSpec(t *testing.T, source StepExecutionSpec) StepExecutionSpec {
	t.Helper()
	spec, err := NewStepExecutionSpec(source)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func completeSpec(t *testing.T, spec StepExecutionSpec, checksumByte string) CompletedStep {
	t.Helper()
	outputs := make([]StepOutput, 0, len(spec.Outputs))
	for _, target := range spec.Outputs {
		output := StepOutput{
			ID: target.ID, Kind: target.Kind, StorageID: target.StorageID,
			RelativePath: target.RelativePath, Checksum: strings.Repeat(checksumByte, 64),
		}
		if target.Kind == OutputTranscript {
			output.TranscriptQuality = &transcription.QualityAssessment{
				PolicyReference: transcription.QualityPolicyReference, State: transcription.QualityPassed,
				DurationMillis: 1_000, SegmentCount: 1, TextCharacterCount: 5,
			}
		}
		outputs = append(outputs, output)
	}
	record, err := NewStepOutputRecord(spec, 1, "WORKER-1", time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC), outputs, []string{"evidence://verified"})
	if err != nil {
		t.Fatal(err)
	}
	return CompletedStep{StepID: spec.StepID, Spec: spec, OutputRecord: record}
}
