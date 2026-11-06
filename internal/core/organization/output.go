package organization

import (
	"time"

	"videoarchive/internal/core/vocabulary"
)

type BundleSource struct {
	OriginalAssetID string `json:"original_asset_id"`
	OriginalName    string `json:"original_name"`
	RelativePath    string `json:"relative_path"`
	Checksum        string `json:"checksum"`
}

type OrganizedMediaBundle struct {
	WorkspaceDriveID string                             `json:"workspace_drive_id"`
	MediaID          string                             `json:"media_id"`
	DecisionID       string                             `json:"organization_decision_id"`
	Disposition      vocabulary.OrganizationDisposition `json:"organization_disposition"`
	Sources          []BundleSource                     `json:"sources"`
	VideoPath        string                             `json:"video_path"`
	VideoChecksum    string                             `json:"video_checksum"`
	TranscriptPath   string                             `json:"transcript_path,omitempty"`
	SubtitlePath     string                             `json:"subtitle_path,omitempty"`
	MetadataPath     string                             `json:"metadata_path"`
}

type ArchiveCatalogRow struct {
	SourceCreatedAt            time.Time
	SourceName                 string
	ArchiveDate                string
	OutputVideoName            string
	WorkspaceDriveID           string
	MediaFolder                string
	OriginalAssetID            string
	SourceRelativePath         string
	SourceChecksum             string
	SourceDurationMillis       int64
	SourceSizeBytes            int64
	SourceContainer            string
	SourceBitRateBPS           int64
	SourceVideoCodec           string
	SourceVideoProfile         string
	SourceWidth                int
	SourceHeight               int
	SourceFrameRateNumerator   int64
	SourceFrameRateDenominator int64
	SourceVideoPixelFormat     string
	SourceVideoBitRateBPS      int64
	SourceAudioCodec           string
	SourceAudioProfile         string
	SourceAudioBitRateBPS      int64
	SourceAudioSampleRateHz    int
	SourceAudioChannels        int
	SourceAudioChannelLayout   string
	Disposition                vocabulary.OrganizationDisposition
	DecisionID                 string
	ArchivePackageID           string
	OutputMediaID              string
	OutputVideoPath            string
	OutputProfile              string
	TranscriptPath             string
	TranscriptTextPath         string
	SubtitlePath               string
	Status                     string
	HoldReason                 string
}

type ArchiveCatalog struct {
	WorkspaceDriveID string
	Path             string
	RowCount         int
	Checksum         string
}

type CoordinatedFile struct {
	RelativePath    string `json:"relative_path"`
	ObservationID   string `json:"observation_id"`
	OriginalAssetID string `json:"original_asset_id"`
	InspectionID    string `json:"inspection_id"`
}

type CoordinatedFolder struct {
	Folder   string               `json:"folder"`
	Decision OrganizationDecision `json:"decision"`
	JobID    string               `json:"job_id,omitempty"`
}

type ArchiveOrganizationReceipt struct {
	ID               string              `json:"id"`
	IdempotencyKey   string              `json:"idempotency_key"`
	InputDigest      string              `json:"input_digest"`
	SessionID        string              `json:"session_id"`
	InputRevision    uint64              `json:"input_revision"`
	WorkspaceDriveID string              `json:"workspace_drive_id"`
	WorkerID         string              `json:"worker_id"`
	Files            []CoordinatedFile   `json:"files"`
	Folders          []CoordinatedFolder `json:"folders"`
	CompletedAt      time.Time           `json:"completed_at"`
}
