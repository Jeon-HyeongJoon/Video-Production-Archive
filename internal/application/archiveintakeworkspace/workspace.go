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
	"videoarchive/internal/core/workflow"
)

const ClassifyHoldCommand = "organization.hold.classify"

type FolderView struct {
	Folder               string
	DecisionID           string
	InputRevision        uint64
	DecisionSequence     uint64
	Disposition          vocabulary.OrganizationDisposition
	ProfileReference     string
	HoldReasons          []vocabulary.OrganizationHoldReason
	ClassificationAssets []ClassificationAssetView
	VideoCount           int
	EstimatedOutputBytes uint64
	JobID                string
	TranscriptionJobID   string
	CurrentStepID        string
	StepState            workflow.StepState
	Stages               []StageView
	ArchivePackageID     string
	ArchiveState         archive.PackageState
	CanAddTranscription  bool
	TranscriptQualities  []TranscriptQualityView
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

type DriveView struct {
	WorkspaceDriveID           string
	ArchiveRunID               string
	ArchiveRunRevision         uint64
	CandidateRevision          uint64
	FolderDiscoveryRevision    uint64
	SessionID                  string
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
}

type OverrideCommand struct {
	CommandID              string
	DecisionID             string
	IdempotencyKey         string
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
