package archiveintakeworkspace

import (
	"context"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

const ClassifyHoldCommand = "organization.hold.classify"

type FolderView struct {
	SourceRole           *organization.SourceRoleEvidence
	Folder               string
	DecisionID           string
	InputRevision        uint64
	DecisionSequence     uint64
	Disposition          vocabulary.OrganizationDisposition
	ProfileReference     string
	HoldReasons          []vocabulary.OrganizationHoldReason
	WorkOptions          []WorkOptionView
	ClassificationAssets []ClassificationAssetView
	Videos               []SourceVideoView
	VideoCount           int
	EstimatedOutputBytes uint64
	ExpectedResult       archive.FlatPackagePreview
	JobID                string
	TranscriptionJobID   string
	CurrentStepID        string
	StepState            workflow.StepState
	Stages               []StageView
	ArchivePackageID     string
	ArchiveDate          string
	ArchiveDisplayName   string
	ArchiveVideoFileName string
	ArchiveState         archive.PackageState
	CanAddTranscription  bool
	TranscriptQualities  []TranscriptQualityView
}

type SourceMeasurement struct {
	InspectionID   string `json:"inspection_id"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	DurationMillis int64  `json:"duration_millis"`
	VideoBitRate   int64  `json:"video_bitrate_bps,omitempty"`
}

type SourceVideoView struct {
	Measurement     *SourceMeasurement
	MatchedOriginal string
	MetadataHint    string
	RelativePath    string
	FileName        string
	SizeBytes       uint64
	ModifiedAt      time.Time
}

type WorkOptionView struct {
	Disposition            vocabulary.OrganizationDisposition
	TargetVideoBitRateKbps int
}

type TranscriptQualityView struct {
	TranscriptID       string
	OriginalAssetID    string
	State              transcription.QualityState
	Reasons            []transcription.QualityReason
	DurationMillis     int64
	SegmentCount       int
	TextCharacterCount int
}

type ClassificationAssetView struct {
	MatchedOriginal string                      `json:"matched_original,omitempty"`
	InspectionID    string                      `json:"inspection_id,omitempty"`
	MetadataHint    string                      `json:"metadata_hint,omitempty"`
	OriginalAssetID string                      `json:"original_asset_id"`
	RelativePath    string                      `json:"relative_path"`
	Checksum        string                      `json:"checksum"`
	DurationMillis  int64                       `json:"duration_millis"`
	SizeBytes       int64                       `json:"size_bytes"`
	BitRate         int64                       `json:"bitrate_bps"`
	Containers      []string                    `json:"containers"`
	VideoStreams    []mediaanalysis.VideoStream `json:"video_streams"`
	AudioStreams    []mediaanalysis.AudioStream `json:"audio_streams"`
}

type StorageView struct {
	StorageID        string
	Role             vocabulary.StorageRole
	WorkspaceDriveID string
	AccessKind       vocabulary.StorageAccessKind
	Accessible       bool
	Model            string
	Label            string
	FileSystem       string
	CapacityBytes    uint64
	FreeBytes        uint64
}

type StageView struct {
	ID                  string
	State               workflow.StepState
	WaitReason          vocabulary.WaitReason
	WaitTargetStorageID string
	CompletedCount      int
	TotalCount          int
}

// CompletedWorkView is one immutable ArchivePackage output from a completed
// stage-one Folder Job. It is retained independently of the latest ArchiveRun
// so a later intake does not hide work that has already been stored.
type CompletedWorkView struct {
	ArchiveRunID         string
	Folder               string
	JobID                string
	Disposition          vocabulary.OrganizationDisposition
	ProfileReference     string
	SourceVideoCount     int
	ArchiveStorageID     string
	ArchivePackageID     string
	ArchiveDate          string
	ArchiveDisplayName   string
	ArchiveVideoFileName string
	ArchiveRelativePath  string
	ArchiveState         archive.PackageState
	CompletedAt          time.Time
}

type DriveView struct {
	WorkspaceDriveID           string
	ConnectionState            vocabulary.VolumeConnectionState
	ConnectionObservedAt       time.Time
	ConnectionWorkerID         string
	ArchiveRunID               string
	ArchiveRunRevision         uint64
	CandidateRevision          uint64
	FolderDiscoveryRevision    uint64
	DiscoveryGap               *worker.DiscoveryGap `json:"discovery_gap,omitempty"`
	FolderDiscoveryState       vocabulary.FolderDiscoveryState
	SessionID                  string
	SelectionSessionID         string
	WorkerID                   string
	SourceVolumeID             string
	SourceStorageID            string
	ArchiveStorageID           string
	State                      string
	ArchiveRunState            string
	InputRevision              uint64
	FileCount                  int
	FolderCount                int
	StageOneRemaining          int
	StageTwoRemaining          int
	HoldCount                  int
	EligibleTranscriptionCount int
	CatalogRowCount            int
	CatalogUpdatedAt           time.Time
	Folders                    []FolderView
	DiscoveredFileCount        int
	DiscoveredFolderCount      int
	DiscoveredFolders          []FolderView
	StorageCandidates          []StorageView
	CompletedWork              []CompletedWorkView
}

type OverrideCommand struct {
	CommandID              string
	DecisionID             string
	IdempotencyKey         string
	ArchiveRunID           string
	WorkspaceDriveID       string
	Folder                 string
	ExpectedInputRevision  uint64
	Disposition            vocabulary.OrganizationDisposition
	TargetVideoBitRateKbps int
	Actor                  command.Actor
	Reason                 string
	Basis                  []string
	OccurredAt             time.Time
}

type Workspace interface {
	Projection(context.Context) ([]DriveView, error)
	Override(context.Context, OverrideCommand) (organization.OrganizationDecision, error)
}

type ResolvedFolderStarter interface {
	StartResolvedFolder(context.Context, string, string, archiverun.State, organization.OrganizationDecision, time.Time) (string, error)
}
