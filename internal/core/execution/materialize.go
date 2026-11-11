package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"sort"
	"strings"

	"videoarchive/internal/core/archive"
)

var ErrCannotMaterializeStepExecutionSpec = errors.New("dependent Step execution spec cannot be materialized")

type CompletedStep struct {
	StepID       string
	Spec         StepExecutionSpec
	OutputRecord StepOutputRecord
}

type MaterializeRequest struct {
	JobID        string
	TemplateName string
	StepID       string
	Completed    []CompletedStep
}

func MaterializeDependentStepExecutionSpec(request MaterializeRequest) (StepExecutionSpec, error) {
	request.JobID = strings.TrimSpace(request.JobID)
	request.TemplateName = strings.TrimSpace(request.TemplateName)
	request.StepID = strings.TrimSpace(request.StepID)
	if request.JobID == "" || request.TemplateName == "" || request.StepID == "" ||
		!(strings.HasPrefix(request.TemplateName, "archive-intake-") || request.TemplateName == "archive-transcription") || len(request.Completed) == 0 {
		return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
	}
	completed := append([]CompletedStep(nil), request.Completed...)
	sort.Slice(completed, func(i, j int) bool { return completed[i].StepID < completed[j].StepID })
	var parameters StepExecutionParameters
	profileReference := ""
	var allOutputs []StepOutput
	outputOriginals := make(map[string]string)
	for index, value := range completed {
		if strings.TrimSpace(value.StepID) == "" || value.Spec.Validate() != nil || value.OutputRecord.ValidateFor(value.Spec) != nil ||
			value.Spec.JobID != request.JobID || value.Spec.TemplateName != request.TemplateName || value.Spec.StepID != value.StepID {
			return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
		}
		if index == 0 {
			parameters = value.Spec.Parameters
			profileReference = parameters.SelectedProfileReference
		} else if value.Spec.Parameters.OrganizationDecisionID != parameters.OrganizationDecisionID ||
			value.Spec.Parameters.WorkspaceDriveID != parameters.WorkspaceDriveID ||
			value.Spec.Parameters.InputRevision != parameters.InputRevision {
			return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
		}
		allOutputs = append(allOutputs, value.OutputRecord.Outputs...)
		for _, target := range value.Spec.Outputs {
			if target.OriginalAssetID != "" {
				outputOriginals[target.ID] = target.OriginalAssetID
			}
		}
	}
	originalInputs := append([]InputReference(nil), parameters.SourceInputs...)
	if len(originalInputs) == 0 {
		return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
	}
	sort.Slice(originalInputs, func(i, j int) bool { return originalInputs[i].OriginalAssetID < originalInputs[j].OriginalAssetID })
	sort.Slice(allOutputs, func(i, j int) bool {
		if allOutputs[i].Kind != allOutputs[j].Kind {
			return allOutputs[i].Kind < allOutputs[j].Kind
		}
		return allOutputs[i].ID < allOutputs[j].ID
	})

	var operation Operation
	var inputs []InputReference
	var outputs []OutputTarget
	switch request.StepID {
	case "qc":
		operation = OperationQCRendition
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputRendition)
		for _, input := range inputs {
			id := stableExecutionID("QC", parameters.OrganizationDecisionID, input.ArtifactID)
			outputs = append(outputs, OutputTarget{ID: id, Kind: OutputQCAssessment, StorageID: input.StorageID, RelativePath: path.Join("staging", request.JobID, "qc", id+".json")})
		}
	case "organize":
		operation = OperationOrganizeMedia
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputRendition)
		if len(inputs) == 0 {
			inputs = append(inputs, originalInputs...)
		}
		if parameters.SourceTimeline != nil {
			packageID := stableArchivePackageID(parameters.ArchiveDate, parameters.OrganizationDecisionID, parameters.SourceTimeline.ID)
			directory, err := flatPackageDirectory(packageID, parameters.MediaFolder, originalInputs)
			if err != nil {
				return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
			}
			outputs = append(outputs, OutputTarget{ID: packageID, Kind: OutputOrganizedMedia, StorageID: parameters.ArchiveStorageID, RelativePath: path.Join(directory, ".archive-package.json")})
		} else {
			for _, original := range originalInputs {
				packageID := stableArchivePackageID(parameters.ArchiveDate, parameters.OrganizationDecisionID, original.OriginalAssetID)
				directory, err := flatPackageDirectory(packageID, parameters.MediaFolder, []InputReference{original})
				if err != nil {
					return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
				}
				outputs = append(outputs, OutputTarget{ID: packageID, Kind: OutputOrganizedMedia, StorageID: parameters.ArchiveStorageID, RelativePath: path.Join(directory, ".archive-package.json"), OriginalAssetID: original.OriginalAssetID})
			}
		}
	case "audio":
		operation = OperationExtractSpeechAudio
		profileReference = "speech-ko-enhanced-16k@v1"
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputRendition)
		if len(inputs) == 0 {
			inputs = append(inputs, originalInputs...)
		}
		for _, input := range inputs {
			id := stableExecutionID("AUDIO", parameters.OrganizationDecisionID, input.ArtifactID)
			outputs = append(outputs, OutputTarget{ID: id, Kind: OutputAudio, StorageID: input.StorageID, RelativePath: path.Join("staging", request.JobID, "audio", id+".wav"), OriginalAssetID: input.OriginalAssetID})
		}
	case "transcript":
		operation = OperationTranscribeAudio
		profileReference = "event-ko@v1"
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputAudio)
		for _, input := range inputs {
			id := stableExecutionID("TRANSCRIPT", parameters.OrganizationDecisionID, input.ArtifactID)
			outputs = append(outputs, OutputTarget{ID: id, Kind: OutputTranscript, StorageID: input.StorageID, RelativePath: path.Join("staging", request.JobID, "transcript", id+".json"), OriginalAssetID: input.OriginalAssetID})
		}
	case "subtitle":
		operation = OperationWriteSubtitle
		profileReference = "event-srt@v1"
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputTranscript)
		for _, input := range inputs {
			id := stableExecutionID("SUBTITLE", parameters.OrganizationDecisionID, input.ArtifactID)
			outputs = append(outputs, OutputTarget{ID: id, Kind: OutputSubtitle, StorageID: input.StorageID, RelativePath: path.Join("staging", request.JobID, "subtitle", id+".srt"), OriginalAssetID: input.OriginalAssetID})
		}
	case "transcript_text":
		operation = OperationWriteTranscriptText
		profileReference = "plain-text@v1"
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputTranscript)
		for _, input := range inputs {
			id := stableExecutionID("TRANSCRIPT-TEXT", parameters.OrganizationDecisionID, input.ArtifactID)
			outputs = append(outputs, OutputTarget{ID: id, Kind: OutputTranscriptText, StorageID: input.StorageID, RelativePath: path.Join("staging", request.JobID, "transcript_text", id+".txt"), OriginalAssetID: input.OriginalAssetID})
		}
	case "archive_package_revision":
		operation = OperationReviseArchivePackage
		inputs = append(inputs, inputsFromOutputs(allOutputs, outputOriginals, OutputTranscript)...)
		inputs = append(inputs, inputsFromOutputs(allOutputs, outputOriginals, OutputTranscriptText)...)
		inputs = append(inputs, inputsFromOutputs(allOutputs, outputOriginals, OutputSubtitle)...)
		if parameters.ArchivePackageID != "" && parameters.ArchivePackageRevision > 0 && len(inputs) == 3 {
			directory, err := flatPackageDirectory(parameters.ArchivePackageID, parameters.MediaFolder, originalInputs)
			if err != nil {
				return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
			}
			relativePath := path.Join(directory, ".archive-package.json")
			for _, candidate := range allOutputs {
				if candidate.ID == parameters.ArchivePackageID && candidate.Kind == OutputOrganizedMedia && candidate.RelativePath != "" {
					relativePath = candidate.RelativePath
					break
				}
			}
			outputs = []OutputTarget{{
				ID: parameters.ArchivePackageID, Kind: OutputArchiveRevision, StorageID: parameters.ArchiveStorageID,
				RelativePath: relativePath,
			}}
		}
	case "archive_catalog":
		operation = OperationWriteArchiveCatalog
		inputs = inputsFromOutputs(allOutputs, outputOriginals, OutputOrganizedMedia, OutputArchiveRevision)
		if len(inputs) > 0 {
			id := stableExecutionID("CATALOG", parameters.ArchiveStorageID, parameters.OrganizationDecisionID)
			outputs = []OutputTarget{{ID: id, Kind: OutputArchiveCatalog, StorageID: parameters.ArchiveStorageID, RelativePath: "archive-catalog.csv"}}
		}
	default:
		return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
	}
	if len(inputs) == 0 || len(outputs) == 0 {
		return StepExecutionSpec{}, ErrCannotMaterializeStepExecutionSpec
	}
	return NewStepExecutionSpec(StepExecutionSpec{
		JobID: request.JobID, StepID: request.StepID, TemplateName: request.TemplateName, Operation: operation,
		Inputs: inputs, Outputs: outputs, ProfileReference: profileReference, Parameters: parameters,
	})
}

func flatPackageDirectory(packageID, mediaFolder string, inputs []InputReference) (string, error) {
	names := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.OriginalName) == "" {
			return "", ErrCannotMaterializeStepExecutionSpec
		}
		names = append(names, input.OriginalName)
	}
	presentation, err := archive.PresentFlatPackage(packageID, mediaFolder, names)
	if err != nil {
		return "", ErrCannotMaterializeStepExecutionSpec
	}
	return presentation.PackageDirectory, nil
}

func inputsFromOutputs(outputs []StepOutput, originalAssetIDs map[string]string, kinds ...OutputKind) []InputReference {
	allowed := make(map[OutputKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	inputs := make([]InputReference, 0, len(outputs))
	seen := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		if (len(allowed) > 0 && !allowed[output.Kind]) || seen[output.ID] {
			continue
		}
		seen[output.ID] = true
		inputs = append(inputs, InputReference{
			OriginalAssetID: originalAssetIDs[output.ID], ArtifactID: output.ID,
			StorageID: output.StorageID, RelativePath: output.RelativePath, Checksum: output.Checksum, Kind: output.Kind,
		})
	}
	return inputs
}

func stableExecutionID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

func stableArchivePackageID(archiveDate, decisionID, discriminator string) string {
	archiveDate = strings.TrimSpace(archiveDate)
	if len(archiveDate) != 8 {
		archiveDate = "00000000"
	}
	digest := sha256.Sum256([]byte(decisionID + "\x00" + discriminator))
	return "VP-" + archiveDate + "-" + hex.EncodeToString(digest[:6])
}
