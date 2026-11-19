package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

var ErrInvalidStepExecutionSpec = errors.New("Step execution spec is incomplete or invalid")

const StepExecutionSpecVersion uint32 = 1

type Operation string

const (
	OperationInspectMedia                Operation = "INSPECT_MEDIA"
	OperationRenderRecording             Operation = "RENDER_RECORDING"
	OperationRenderSourceSequence        Operation = "RENDER_SOURCE_SEQUENCE"
	OperationCreateThumbnail             Operation = "CREATE_THUMBNAIL"
	OperationQCRendition                 Operation = "QC_RENDITION"
	OperationVerifyChecksum              Operation = "VERIFY_CHECKSUM"
	OperationDeviceIO                    Operation = "DEVICE_IO"
	OperationOrganizeMedia               Operation = "ORGANIZE_MEDIA"
	OperationExtractSpeechAudio          Operation = "EXTRACT_SPEECH_AUDIO"
	OperationTranscribeAudio             Operation = "TRANSCRIBE_AUDIO"
	OperationWriteSubtitle               Operation = "WRITE_SUBTITLE"
	OperationWriteTranscriptText         Operation = "WRITE_TRANSCRIPT_TEXT"
	OperationReviseArchivePackage        Operation = "REVISE_ARCHIVE_PACKAGE"
	OperationWriteArchiveCatalog         Operation = "WRITE_ARCHIVE_CATALOG"
	OperationVerifyArchiveCatalog        Operation = "VERIFY_ARCHIVE_CATALOG"
	OperationRebuildArchiveCatalog       Operation = "REBUILD_ARCHIVE_CATALOG"
	OperationReconcileArchivePublication Operation = "RECONCILE_ARCHIVE_PUBLICATION"
)

func (operation Operation) Valid() bool {
	switch operation {
	case OperationInspectMedia, OperationRenderRecording, OperationRenderSourceSequence, OperationCreateThumbnail,
		OperationQCRendition, OperationVerifyChecksum, OperationDeviceIO, OperationOrganizeMedia,
		OperationExtractSpeechAudio, OperationTranscribeAudio, OperationWriteSubtitle, OperationWriteTranscriptText,
		OperationReviseArchivePackage, OperationWriteArchiveCatalog,
		OperationVerifyArchiveCatalog, OperationRebuildArchiveCatalog, OperationReconcileArchivePublication:
		return true
	default:
		return false
	}
}

type OutputKind string

const (
	OutputMediaInspection            OutputKind = "MEDIA_INSPECTION"
	OutputRendition                  OutputKind = "RENDITION"
	OutputThumbnail                  OutputKind = "THUMBNAIL"
	OutputCutMarkers                 OutputKind = "CUT_MARKERS"
	OutputOrganizedMedia             OutputKind = "ORGANIZED_MEDIA"
	OutputQCAssessment               OutputKind = "QC_ASSESSMENT"
	OutputAudio                      OutputKind = "AUDIO"
	OutputTranscript                 OutputKind = "TRANSCRIPT"
	OutputSubtitle                   OutputKind = "SUBTITLE"
	OutputTranscriptText             OutputKind = "TRANSCRIPT_TEXT"
	OutputArchiveRevision            OutputKind = "ARCHIVE_PACKAGE_REVISION"
	OutputArchiveCatalog             OutputKind = "ARCHIVE_CATALOG"
	OutputArchiveCatalogVerification OutputKind = "ARCHIVE_CATALOG_VERIFICATION"
	OutputChecksum                   OutputKind = "CHECKSUM"
	OutputDeviceResult               OutputKind = "DEVICE_RESULT"
)

func (kind OutputKind) valid() bool {
	switch kind {
	case OutputMediaInspection, OutputRendition, OutputThumbnail, OutputCutMarkers, OutputOrganizedMedia,
		OutputQCAssessment, OutputAudio, OutputTranscript, OutputSubtitle, OutputTranscriptText, OutputArchiveRevision, OutputArchiveCatalog,
		OutputArchiveCatalogVerification,
		OutputChecksum, OutputDeviceResult:
		return true
	default:
		return false
	}
}

type InputReference struct {
	OriginalAssetID      string     `json:"original_asset_id,omitempty"`
	OriginalName         string     `json:"original_name,omitempty"`
	ArtifactID           string     `json:"artifact_id,omitempty"`
	InspectionID         string     `json:"inspection_id,omitempty"`
	StorageID            string     `json:"storage_id"`
	RelativePath         string     `json:"relative_path"`
	Checksum             string     `json:"checksum"`
	DurationMillis       int64      `json:"duration_millis,omitempty"`
	SourceCreatedAt      time.Time  `json:"source_created_at,omitempty"`
	SourceSizeBytes      int64      `json:"source_size_bytes,omitempty"`
	SourceContainer      string     `json:"source_container,omitempty"`
	SourceBitRateBPS     int64      `json:"source_bitrate_bps,omitempty"`
	VideoCodec           string     `json:"video_codec,omitempty"`
	VideoProfile         string     `json:"video_profile,omitempty"`
	Width                int        `json:"width,omitempty"`
	Height               int        `json:"height,omitempty"`
	FrameRateNumerator   int64      `json:"frame_rate_numerator,omitempty"`
	FrameRateDenominator int64      `json:"frame_rate_denominator,omitempty"`
	VideoPixelFormat     string     `json:"video_pixel_format,omitempty"`
	VideoBitRateBPS      int64      `json:"video_bitrate_bps,omitempty"`
	HasAudio             bool       `json:"has_audio,omitempty"`
	AudioCodec           string     `json:"audio_codec,omitempty"`
	AudioProfile         string     `json:"audio_profile,omitempty"`
	AudioBitRateBPS      int64      `json:"audio_bitrate_bps,omitempty"`
	AudioSampleRateHz    int        `json:"audio_sample_rate_hz,omitempty"`
	AudioChannels        int        `json:"audio_channels,omitempty"`
	AudioChannelLayout   string     `json:"audio_channel_layout,omitempty"`
	ProfileReference     string     `json:"profile_reference,omitempty"`
	Kind                 OutputKind `json:"kind,omitempty"`
}

type OutputTarget struct {
	ID              string     `json:"id"`
	Kind            OutputKind `json:"kind"`
	StorageID       string     `json:"storage_id"`
	RelativePath    string     `json:"relative_path"`
	OriginalAssetID string     `json:"original_asset_id,omitempty"`
}

type StepExecutionParameters struct {
	ArchiveRunID               string                             `json:"archive_run_id,omitempty"`
	ArchiveIntakeSessionID     string                             `json:"archive_intake_session_id,omitempty"`
	StartIdempotencyKey        string                             `json:"start_idempotency_key,omitempty"`
	SourceStorageID            string                             `json:"source_storage_id,omitempty"`
	FolderIDs                  []string                           `json:"folder_ids,omitempty"`
	RequestedBy                string                             `json:"requested_by,omitempty"`
	RequestedAt                time.Time                          `json:"requested_at,omitempty"`
	OrganizationDecisionID     string                             `json:"organization_decision_id"`
	WorkspaceDriveID           string                             `json:"workspace_drive_id"`
	ArchiveStorageID           string                             `json:"archive_storage_id,omitempty"`
	MediaFolder                string                             `json:"media_folder,omitempty"`
	InputRevision              uint64                             `json:"input_revision"`
	ArchiveDate                string                             `json:"archive_date,omitempty"`
	ArchivePackageID           string                             `json:"archive_package_id,omitempty"`
	ArchivePackageRevision     uint64                             `json:"archive_package_revision,omitempty"`
	ArchivePackageMetadataPath string                             `json:"archive_package_metadata_path,omitempty"`
	Disposition                vocabulary.OrganizationDisposition `json:"disposition,omitempty"`
	SelectedProfileReference   string                             `json:"selected_profile_reference,omitempty"`
	TranscriptionEligible      bool                               `json:"transcription_eligible,omitempty"`
	SourceInputs               []InputReference                   `json:"source_inputs,omitempty"`
	SourceTimeline             *organization.SourceTimeline       `json:"source_timeline,omitempty"`
}

type StepExecutionSpec struct {
	Version          uint32                  `json:"version"`
	Digest           string                  `json:"digest"`
	JobID            string                  `json:"job_id"`
	StepID           string                  `json:"step_id"`
	TemplateName     string                  `json:"template_name"`
	Operation        Operation               `json:"operation"`
	Inputs           []InputReference        `json:"inputs"`
	Outputs          []OutputTarget          `json:"outputs"`
	ProfileReference string                  `json:"profile_reference,omitempty"`
	Parameters       StepExecutionParameters `json:"parameters"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func NewStepExecutionSpec(source StepExecutionSpec) (StepExecutionSpec, error) {
	spec := source
	spec.Version = StepExecutionSpecVersion
	spec.Digest = ""
	spec.JobID = strings.TrimSpace(source.JobID)
	spec.StepID = strings.TrimSpace(source.StepID)
	spec.TemplateName = strings.TrimSpace(source.TemplateName)
	spec.ProfileReference = strings.TrimSpace(source.ProfileReference)
	spec.Parameters.ArchiveRunID = strings.TrimSpace(source.Parameters.ArchiveRunID)
	spec.Parameters.ArchiveIntakeSessionID = strings.TrimSpace(source.Parameters.ArchiveIntakeSessionID)
	spec.Parameters.StartIdempotencyKey = strings.TrimSpace(source.Parameters.StartIdempotencyKey)
	spec.Parameters.SourceStorageID = strings.TrimSpace(source.Parameters.SourceStorageID)
	spec.Parameters.RequestedBy = strings.TrimSpace(source.Parameters.RequestedBy)
	spec.Parameters.FolderIDs = append([]string(nil), source.Parameters.FolderIDs...)
	for index := range spec.Parameters.FolderIDs {
		spec.Parameters.FolderIDs[index] = normalizeRelative(spec.Parameters.FolderIDs[index])
	}
	spec.Parameters.OrganizationDecisionID = strings.TrimSpace(source.Parameters.OrganizationDecisionID)
	spec.Parameters.WorkspaceDriveID = strings.TrimSpace(source.Parameters.WorkspaceDriveID)
	spec.Parameters.ArchiveStorageID = strings.TrimSpace(source.Parameters.ArchiveStorageID)
	spec.Parameters.ArchiveDate = strings.TrimSpace(source.Parameters.ArchiveDate)
	spec.Parameters.ArchivePackageID = strings.TrimSpace(source.Parameters.ArchivePackageID)
	if strings.TrimSpace(source.Parameters.MediaFolder) == "" {
		spec.Parameters.MediaFolder = ""
	} else {
		spec.Parameters.MediaFolder = normalizeRelative(source.Parameters.MediaFolder)
	}
	spec.Parameters.SelectedProfileReference = strings.TrimSpace(source.Parameters.SelectedProfileReference)
	spec.Inputs = append([]InputReference(nil), source.Inputs...)
	spec.Parameters.SourceInputs = append([]InputReference(nil), source.Parameters.SourceInputs...)
	spec.Outputs = append([]OutputTarget(nil), source.Outputs...)
	normalizeInputs := func(inputs []InputReference) {
		for index := range inputs {
			input := &inputs[index]
			input.OriginalAssetID = strings.TrimSpace(input.OriginalAssetID)
			input.OriginalName = strings.TrimSpace(input.OriginalName)
			input.ArtifactID = strings.TrimSpace(input.ArtifactID)
			input.InspectionID = strings.TrimSpace(input.InspectionID)
			input.StorageID = strings.TrimSpace(input.StorageID)
			input.RelativePath = normalizeRelative(input.RelativePath)
			input.Checksum = strings.ToLower(strings.TrimSpace(input.Checksum))
			if !input.SourceCreatedAt.IsZero() {
				input.SourceCreatedAt = input.SourceCreatedAt.UTC()
			}
			input.SourceContainer = strings.ToLower(strings.TrimSpace(input.SourceContainer))
			input.VideoCodec = strings.ToLower(strings.TrimSpace(input.VideoCodec))
			input.VideoProfile = strings.TrimSpace(input.VideoProfile)
			input.VideoPixelFormat = strings.ToLower(strings.TrimSpace(input.VideoPixelFormat))
			input.AudioCodec = strings.ToLower(strings.TrimSpace(input.AudioCodec))
			input.AudioProfile = strings.TrimSpace(input.AudioProfile)
			input.AudioChannelLayout = strings.ToLower(strings.TrimSpace(input.AudioChannelLayout))
			input.ProfileReference = strings.TrimSpace(input.ProfileReference)
		}
	}
	normalizeInputs(spec.Inputs)
	normalizeInputs(spec.Parameters.SourceInputs)
	for index := range spec.Outputs {
		output := &spec.Outputs[index]
		output.ID = strings.TrimSpace(output.ID)
		output.StorageID = strings.TrimSpace(output.StorageID)
		output.RelativePath = normalizeRelative(output.RelativePath)
		output.OriginalAssetID = strings.TrimSpace(output.OriginalAssetID)
	}
	if spec.Parameters.SourceTimeline != nil {
		timeline := *spec.Parameters.SourceTimeline
		timeline.Clips = append([]organization.SourceClip(nil), timeline.Clips...)
		timeline.Markers = append([]organization.CutMarker(nil), timeline.Markers...)
		spec.Parameters.SourceTimeline = &timeline
	}
	if !spec.validWithoutDigest() {
		return StepExecutionSpec{}, ErrInvalidStepExecutionSpec
	}
	digest, err := specDigest(spec)
	if err != nil {
		return StepExecutionSpec{}, err
	}
	spec.Digest = digest
	return spec, nil
}

func (spec StepExecutionSpec) Validate() error {
	if spec.Version != StepExecutionSpecVersion || !sha256Pattern.MatchString(spec.Digest) || !spec.validWithoutDigest() {
		return ErrInvalidStepExecutionSpec
	}
	digest, err := specDigest(spec)
	if err != nil || digest != spec.Digest {
		return ErrInvalidStepExecutionSpec
	}
	return nil
}

func (spec StepExecutionSpec) validWithoutDigest() bool {
	remoteInspection := spec.Operation == OperationInspectMedia && spec.TemplateName == "archive-intake-inspection"
	catalogMaintenance := spec.Operation == OperationVerifyArchiveCatalog || spec.Operation == OperationRebuildArchiveCatalog
	if spec.JobID == "" || spec.StepID == "" || spec.TemplateName == "" || !spec.Operation.Valid() ||
		len(spec.Outputs) == 0 ||
		spec.Parameters.WorkspaceDriveID == "" || spec.Parameters.InputRevision == 0 {
		return false
	}
	if !remoteInspection && !catalogMaintenance && (len(spec.Inputs) == 0 || spec.Parameters.OrganizationDecisionID == "") {
		return false
	}
	if spec.Parameters.SourceTimeline != nil && spec.Parameters.SourceTimeline.Validate() != nil {
		return false
	}
	seenInputs := make(map[string]bool, len(spec.Inputs))
	for _, input := range spec.Inputs {
		identity := input.OriginalAssetID + "\x00" + input.ArtifactID
		if (input.OriginalAssetID == "" && input.ArtifactID == "") || input.StorageID == "" || !safeRelative(input.RelativePath) ||
			!sha256Pattern.MatchString(input.Checksum) || (input.Kind != "" && !input.Kind.valid()) || seenInputs[identity] {
			return false
		}
		seenInputs[identity] = true
	}
	for _, input := range spec.Parameters.SourceInputs {
		if input.OriginalAssetID == "" || input.StorageID == "" || !safeRelative(input.RelativePath) || !sha256Pattern.MatchString(input.Checksum) {
			return false
		}
		if input.SourceSizeBytes < 0 || input.SourceBitRateBPS < 0 || input.VideoBitRateBPS < 0 || input.AudioBitRateBPS < 0 ||
			input.AudioSampleRateHz < 0 || input.AudioChannels < 0 ||
			((input.FrameRateNumerator == 0) != (input.FrameRateDenominator == 0)) || input.FrameRateNumerator < 0 || input.FrameRateDenominator < 0 {
			return false
		}
	}
	if spec.Parameters.MediaFolder != "" && !safeRelative(spec.Parameters.MediaFolder) {
		return false
	}
	if spec.Parameters.Disposition != "" && !spec.Parameters.Disposition.Valid() {
		return false
	}
	seenOutputs := make(map[string]bool, len(spec.Outputs))
	for _, output := range spec.Outputs {
		if output.ID == "" || !output.Kind.valid() || output.StorageID == "" || !safeRelative(output.RelativePath) || seenOutputs[output.ID] {
			return false
		}
		seenOutputs[output.ID] = true
	}
	switch spec.Operation {
	case OperationVerifyArchiveCatalog:
		return len(spec.Inputs) == 0 && len(spec.Outputs) == 1 &&
			spec.Outputs[0].Kind == OutputArchiveCatalogVerification && spec.Parameters.ArchiveStorageID != "" &&
			spec.Parameters.SourceTimeline == nil
	case OperationRebuildArchiveCatalog:
		return len(spec.Inputs) == 0 && len(spec.Outputs) == 1 &&
			spec.Outputs[0].Kind == OutputArchiveCatalog && spec.Outputs[0].StorageID == spec.Parameters.ArchiveStorageID &&
			spec.Parameters.SourceTimeline == nil
	case OperationRenderRecording:
		if len(spec.Inputs) != len(spec.Outputs) || spec.ProfileReference == "" || spec.Parameters.SourceTimeline != nil {
			return false
		}
		for index, output := range spec.Outputs {
			if output.Kind != OutputRendition || output.OriginalAssetID != spec.Inputs[index].OriginalAssetID {
				return false
			}
		}
		return true
	case OperationRenderSourceSequence:
		if len(spec.Inputs) < 2 || spec.ProfileReference == "" || spec.Parameters.SourceTimeline == nil ||
			len(spec.Parameters.SourceTimeline.Clips) != len(spec.Inputs) {
			return false
		}
		for index, clip := range spec.Parameters.SourceTimeline.Clips {
			if clip.OriginalAssetID != spec.Inputs[index].OriginalAssetID {
				return false
			}
		}
		return true
	case OperationInspectMedia:
		if !remoteInspection {
			return spec.Parameters.SourceTimeline == nil
		}
		if len(spec.Inputs) != 0 || len(spec.Outputs) != 1 || spec.Outputs[0].Kind != OutputMediaInspection ||
			spec.Parameters.ArchiveRunID == "" || spec.Parameters.ArchiveIntakeSessionID == "" ||
			spec.Parameters.StartIdempotencyKey == "" ||
			spec.Parameters.SourceStorageID == "" || spec.Parameters.ArchiveStorageID == "" ||
			spec.Parameters.RequestedBy == "" || spec.Parameters.RequestedAt.IsZero() || len(spec.Parameters.FolderIDs) == 0 ||
			spec.Parameters.SourceTimeline != nil {
			return false
		}
		seenFolders := make(map[string]bool, len(spec.Parameters.FolderIDs))
		for _, folderID := range spec.Parameters.FolderIDs {
			if !safeRelative(folderID) || seenFolders[folderID] {
				return false
			}
			seenFolders[folderID] = true
		}
		return true
	case OperationCreateThumbnail, OperationVerifyChecksum, OperationDeviceIO:
		return spec.Parameters.SourceTimeline == nil
	default:
		return true
	}
}

func specDigest(spec StepExecutionSpec) (string, error) {
	spec.Digest = ""
	payload, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeRelative(value string) string {
	return path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
}

func safeRelative(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.HasPrefix(value, "../") &&
		!strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") &&
		!(len(value) >= 3 && value[1] == ':' && value[2] == '/')
}
