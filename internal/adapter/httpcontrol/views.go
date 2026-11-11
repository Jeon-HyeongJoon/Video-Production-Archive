package httpcontrol

import (
	"time"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

type summaryView struct {
	ID               string          `json:"id"`
	Revision         uint64          `json:"revision"`
	OriginalAssetID  string          `json:"original_asset_id"`
	TranscriptID     string          `json:"transcript_id"`
	ProfileReference string          `json:"profile_reference"`
	Language         string          `json:"language"`
	Content          summary.Content `json:"content"`
	AuthorType       string          `json:"author_type"`
	AuthorID         string          `json:"author_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

type jobSummaryView struct {
	JobID                  string      `json:"job_id"`
	Revision               uint64      `json:"revision"`
	TemplateName           string      `json:"template_name"`
	TargetTitle            string      `json:"target_title"`
	CurrentStepID          string      `json:"current_step_id"`
	StepState              string      `json:"step_state"`
	Progress               float64     `json:"progress"`
	ExecutorKind           string      `json:"executor_kind"`
	ActorID                string      `json:"actor_id,omitempty"`
	WorkerID               string      `json:"worker_id,omitempty"`
	ArchivistAction        string      `json:"archivist_action,omitempty"`
	ToolCallStatus         string      `json:"tool_call_status,omitempty"`
	WaitReason             string      `json:"wait_reason,omitempty"`
	ErrorClass             string      `json:"error_class,omitempty"`
	LastEventAt            time.Time   `json:"last_event_at"`
	LogReference           string      `json:"log_reference,omitempty"`
	NeedsHumanIntervention bool        `json:"needs_human_intervention"`
	InterventionReason     string      `json:"intervention_reason,omitempty"`
	Worker                 *workerView `json:"worker,omitempty"`
}

type workerView struct {
	WorkerID             string    `json:"worker_id"`
	CPUPercent           float64   `json:"cpu_percent"`
	GPUPercent           float64   `json:"gpu_percent"`
	AvailableMemoryBytes uint64    `json:"available_memory_bytes"`
	AvailableVRAMBytes   uint64    `json:"available_vram_bytes"`
	ScratchFreeBytes     uint64    `json:"scratch_free_bytes"`
	HeavyRunning         int       `json:"heavy_running"`
	AcceptingWork        bool      `json:"accepting_work"`
	AccessibleStorageIDs []string  `json:"accessible_storage_ids"`
	HeartbeatAt          time.Time `json:"heartbeat_at"`
}

type workerStatusView struct {
	WorkerID                     string        `json:"worker_id"`
	State                        string        `json:"state"`
	LastHeartbeat                time.Time     `json:"last_heartbeat,omitempty"`
	SnapshotID                   uint64        `json:"snapshot_id"`
	ObservedAt                   time.Time     `json:"observed_at,omitempty"`
	CPUAverage5m                 float64       `json:"cpu_average_5m"`
	GPUAverage5m                 float64       `json:"gpu_average_5m"`
	IdleStableMillis             int64         `json:"idle_stable_millis"`
	AvailableMemoryBytes         uint64        `json:"available_memory_bytes"`
	AvailableVRAMBytes           uint64        `json:"available_vram_bytes"`
	ScratchFreeBytes             uint64        `json:"scratch_free_bytes"`
	AcceptingWork                bool          `json:"accepting_work"`
	HeavyRunning                 int           `json:"heavy_running"`
	LightRunning                 int           `json:"light_running"`
	AccessibleStorageIDs         []string      `json:"accessible_storage_ids"`
	ReservedMemoryBytes          uint64        `json:"reserved_memory_bytes"`
	ReservedScratchBytes         uint64        `json:"reserved_scratch_bytes"`
	CapacityAvailableMemoryBytes uint64        `json:"capacity_available_memory_bytes"`
	CapacityScratchFreeBytes     uint64        `json:"capacity_scratch_free_bytes"`
	HeavySlotsUsed               int           `json:"heavy_slots_used"`
	HeavySlotsMaximum            int           `json:"heavy_slots_maximum"`
	LightSlotsUsed               int           `json:"light_slots_used"`
	LightSlotsMaximum            int           `json:"light_slots_maximum"`
	AdditionalWorkPossible       bool          `json:"additional_work_possible"`
	HeavyAdmission               admissionView `json:"heavy_admission"`
	LightAdmission               admissionView `json:"light_admission"`
}

type admissionView struct {
	Available  bool   `json:"available"`
	WaitReason string `json:"wait_reason,omitempty"`
	Code       string `json:"code,omitempty"`
}

func projectAdmission(value worker.Admission) admissionView {
	return admissionView{Available: value.Available, WaitReason: string(value.WaitReason), Code: value.Code}
}

type archivistHostStatusView struct {
	ArchivistHostID string    `json:"archivist_host_id"`
	State           string    `json:"state"`
	LastHeartbeat   time.Time `json:"last_heartbeat,omitempty"`
	DetectedCLIIDs  []string  `json:"detected_cli_ids"`
}

type driveView struct {
	WorkspaceDriveID           string                   `json:"workspace_drive_id"`
	ConnectionState            string                   `json:"connection_state"`
	ConnectionObservedAt       time.Time                `json:"connection_observed_at,omitempty"`
	ConnectionWorkerID         string                   `json:"connection_worker_id,omitempty"`
	ArchiveRunID               string                   `json:"archive_run_id"`
	ArchiveRunRevision         uint64                   `json:"archive_run_revision"`
	CandidateRevision          uint64                   `json:"candidate_revision"`
	FolderDiscoveryRevision    uint64                   `json:"folder_discovery_revision"`
	FolderDiscoveryState       string                   `json:"folder_discovery_state"`
	SessionID                  string                   `json:"session_id"`
	SelectionSessionID         string                   `json:"selection_session_id"`
	WorkerID                   string                   `json:"worker_id"`
	SourceVolumeID             string                   `json:"source_volume_id"`
	SourceStorageID            string                   `json:"source_storage_id"`
	ArchiveStorageID           string                   `json:"archive_storage_id"`
	State                      string                   `json:"state"`
	ArchiveRunState            string                   `json:"archive_run_state"`
	InputRevision              uint64                   `json:"input_revision"`
	FileCount                  int                      `json:"file_count"`
	FolderCount                int                      `json:"folder_count"`
	StageOneRemaining          int                      `json:"stage_one_remaining"`
	StageTwoRemaining          int                      `json:"stage_two_remaining"`
	HoldCount                  int                      `json:"hold_count"`
	EligibleTranscriptionCount int                      `json:"eligible_transcription_count"`
	CatalogRowCount            int                      `json:"catalog_row_count"`
	CatalogUpdatedAt           time.Time                `json:"catalog_updated_at,omitempty"`
	Folders                    []driveFolderView        `json:"folders"`
	DiscoveredFileCount        int                      `json:"discovered_file_count"`
	DiscoveredFolderCount      int                      `json:"discovered_folder_count"`
	DiscoveredFolders          []driveFolderView        `json:"discovered_folders"`
	StorageCandidates          []driveStorageView       `json:"storage_candidates"`
	CompletedWork              []driveCompletedWorkView `json:"completed_work"`
}

type driveCompletedWorkView struct {
	ArchiveRunID         string    `json:"archive_run_id"`
	Folder               string    `json:"folder"`
	JobID                string    `json:"job_id"`
	Disposition          string    `json:"disposition"`
	ProfileReference     string    `json:"profile_reference,omitempty"`
	SourceVideoCount     int       `json:"source_video_count"`
	ArchiveStorageID     string    `json:"archive_storage_id"`
	ArchivePackageID     string    `json:"archive_package_id"`
	ArchiveDate          string    `json:"archive_date"`
	ArchiveDisplayName   string    `json:"archive_display_name"`
	ArchiveVideoFileName string    `json:"archive_video_file_name"`
	ArchiveRelativePath  string    `json:"archive_relative_path"`
	ArchiveState         string    `json:"archive_state"`
	CompletedAt          time.Time `json:"completed_at"`
}

type driveStorageView struct {
	StorageID        string `json:"storage_id"`
	Role             string `json:"role"`
	WorkspaceDriveID string `json:"workspace_drive_id"`
	AccessKind       string `json:"access_kind"`
	Accessible       bool   `json:"accessible"`
	Model            string `json:"model,omitempty"`
	Label            string `json:"label,omitempty"`
	FileSystem       string `json:"file_system,omitempty"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	FreeBytes        uint64 `json:"free_bytes"`
}

type driveFolderView struct {
	Folder               string                  `json:"folder"`
	DecisionID           string                  `json:"decision_id"`
	InputRevision        uint64                  `json:"input_revision"`
	DecisionSequence     uint64                  `json:"decision_sequence"`
	Disposition          string                  `json:"disposition"`
	ProfileReference     string                  `json:"profile_reference,omitempty"`
	HoldReasons          []string                `json:"hold_reasons"`
	WorkOptions          []driveWorkOptionView   `json:"work_options"`
	Videos               []driveSourceVideoView  `json:"videos"`
	VideoCount           int                     `json:"video_count"`
	EstimatedOutputBytes uint64                  `json:"estimated_output_bytes"`
	JobID                string                  `json:"job_id,omitempty"`
	TranscriptionJobID   string                  `json:"transcription_job_id,omitempty"`
	CurrentStepID        string                  `json:"current_step_id"`
	StepState            string                  `json:"step_state"`
	Stages               []driveStageView        `json:"stages"`
	ArchivePackageID     string                  `json:"archive_package_id,omitempty"`
	ArchiveDate          string                  `json:"archive_date,omitempty"`
	ArchiveDisplayName   string                  `json:"archive_display_name,omitempty"`
	ArchiveVideoFileName string                  `json:"archive_video_file_name,omitempty"`
	ArchiveState         string                  `json:"archive_state,omitempty"`
	CanAddTranscription  bool                    `json:"can_add_transcription"`
	TranscriptQualities  []transcriptQualityView `json:"transcript_qualities"`
}

type driveSourceVideoView struct {
	RelativePath string    `json:"relative_path"`
	FileName     string    `json:"file_name"`
	SizeBytes    uint64    `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at,omitempty"`
}

type driveWorkOptionView struct {
	Disposition            string `json:"disposition"`
	TargetVideoBitRateKbps int    `json:"target_video_bitrate_kbps,omitempty"`
}

type transcriptQualityView struct {
	TranscriptID       string   `json:"transcript_id"`
	OriginalAssetID    string   `json:"original_asset_id,omitempty"`
	State              string   `json:"state"`
	Reasons            []string `json:"reasons"`
	DurationMillis     int64    `json:"duration_millis"`
	SegmentCount       int      `json:"segment_count"`
	TextCharacterCount int      `json:"text_character_count"`
}

type driveStageView struct {
	ID                  string `json:"stage_id"`
	State               string `json:"state"`
	WaitReason          string `json:"wait_reason,omitempty"`
	WaitTargetStorageID string `json:"wait_target_storage_id,omitempty"`
	CompletedCount      int    `json:"completed_count"`
	TotalCount          int    `json:"total_count"`
}

type archivistSessionView struct {
	SessionID       string              `json:"session_id"`
	Name            string              `json:"name"`
	ArchivistHostID string              `json:"archivist_host_id"`
	Backend         string              `json:"backend"`
	CLIID           string              `json:"cli_id"`
	WorkspaceRootID string              `json:"workspace_root_id"`
	JobID           string              `json:"job_id,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	Turns           []archivistTurnView `json:"turns"`
	ActionCards     []actionCardView    `json:"action_cards"`
}

type archivistTurnView struct {
	TurnID         string                                  `json:"turn_id"`
	RequestID      string                                  `json:"request_id"`
	State          string                                  `json:"state"`
	Prompt         string                                  `json:"prompt"`
	SubmittedBy    string                                  `json:"submitted_by"`
	CreatedAt      time.Time                               `json:"created_at"`
	UpdatedAt      time.Time                               `json:"updated_at"`
	FinalResponse  *archivistruntime.FinalResponse         `json:"final_response,omitempty"`
	Entries        []archivistruntime.TurnEntry            `json:"entries"`
	ToolCalls      []archivistToolCallView                 `json:"tool_calls"`
	DriveSelection *archivistruntime.DriveSelection        `json:"drive_selection,omitempty"`
	WorkReference  *archivistruntime.WorkReference         `json:"work_reference,omitempty"`
	WorkContext    *archivistruntime.ArchiveRunWorkContext `json:"work_context,omitempty"`
}

type archivistToolCallView struct {
	ToolCallID string    `json:"tool_call_id"`
	ToolName   string    `json:"tool_name"`
	TargetID   string    `json:"target_id,omitempty"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type actionCardView struct {
	ActionCardID     string           `json:"action_card_id"`
	ArchivistTurnID  string           `json:"archivist_turn_id"`
	CommandType      string           `json:"command_type"`
	TargetID         string           `json:"target_id"`
	ExpectedRevision uint64           `json:"expected_revision"`
	State            string           `json:"state"`
	ApprovalRequired bool             `json:"approval_required"`
	Scope            *actionCardScope `json:"scope,omitempty"`
	RejectedBy       string           `json:"rejected_by,omitempty"`
	RejectedAt       time.Time        `json:"rejected_at,omitempty"`
	RejectionReason  string           `json:"rejection_reason,omitempty"`
}

type actionCardScope struct {
	WorkspaceDriveID string `json:"workspace_drive_id,omitempty"`
	ArchiveStorageID string `json:"archive_storage_id,omitempty"`
	FolderCount      int    `json:"folder_count,omitempty"`
}

type stepView struct {
	ID            string    `json:"id"`
	State         string    `json:"state"`
	Progress      float64   `json:"progress"`
	ExecutorKind  string    `json:"executor_kind"`
	ActorID       string    `json:"actor_id,omitempty"`
	WorkerID      string    `json:"worker_id,omitempty"`
	WaitReason    string    `json:"wait_reason,omitempty"`
	ErrorClass    string    `json:"error_class,omitempty"`
	LogReference  string    `json:"log_reference,omitempty"`
	LastEventAt   time.Time `json:"last_event_at"`
	DependencyIDs []string  `json:"dependency_ids"`
}

type archivistActionView struct {
	Action         string    `json:"action"`
	ToolCallID     string    `json:"tool_call_id"`
	ToolCallStatus string    `json:"tool_call_status"`
	OccurredAt     time.Time `json:"occurred_at"`
}

type timelineView struct {
	Kind       string    `json:"kind"`
	Reference  string    `json:"reference"`
	Summary    string    `json:"summary"`
	OccurredAt time.Time `json:"occurred_at"`
}

type jobDetailView struct {
	Summary          jobSummaryView        `json:"summary"`
	Steps            []stepView            `json:"steps"`
	ArchivistActions []archivistActionView `json:"archivist_actions"`
	Workers          []workerView          `json:"workers"`
	Timeline         []timelineView        `json:"timeline"`
}

type workSummaryView struct {
	WorkID            string    `json:"work_id"`
	Kind              string    `json:"kind"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	LastEventAt       time.Time `json:"last_event_at"`
	WorkerID          string    `json:"worker_id,omitempty"`
	SourceVolumeID    string    `json:"source_volume_id,omitempty"`
	SourceStorageID   string    `json:"source_storage_id,omitempty"`
	ArchiveStorageID  string    `json:"archive_storage_id,omitempty"`
	FolderCount       int       `json:"folder_count"`
	VideoCount        int       `json:"video_count"`
	HoldCount         int       `json:"hold_count"`
	ResultCount       int       `json:"result_count"`
	ChildJobCount     int       `json:"child_job_count"`
	Progress          float64   `json:"progress"`
	Title             string    `json:"title,omitempty"`
	ReplacementWorkID string    `json:"replacement_work_id,omitempty"`
}

type workChildJobView struct {
	Role   string         `json:"role"`
	Folder string         `json:"folder,omitempty"`
	Job    jobSummaryView `json:"job"`
}

type workDetailView struct {
	Summary       workSummaryView          `json:"summary"`
	FolderIDs     []string                 `json:"folder_ids"`
	Children      []workChildJobView       `json:"children"`
	CompletedWork []driveCompletedWorkView `json:"completed_work"`
	Failures      []workFailureView        `json:"failures"`
}

type workFailureView struct {
	JobID        string `json:"job_id"`
	Folder       string `json:"folder,omitempty"`
	StepID       string `json:"step_id"`
	ErrorClass   string `json:"error_class"`
	LogReference string `json:"log_reference,omitempty"`
}

type jobVideoInfoView struct {
	OriginalAssetID      string    `json:"original_asset_id"`
	OriginalName         string    `json:"original_name"`
	RelativePath         string    `json:"relative_path"`
	Checksum             string    `json:"checksum"`
	SourceCreatedAt      time.Time `json:"source_created_at"`
	DurationMillis       int64     `json:"duration_millis"`
	SizeBytes            int64     `json:"size_bytes"`
	Container            string    `json:"container"`
	BitRateBPS           int64     `json:"bitrate_bps"`
	VideoCodec           string    `json:"video_codec"`
	VideoProfile         string    `json:"video_profile"`
	Width                int       `json:"width"`
	Height               int       `json:"height"`
	FrameRateNumerator   int64     `json:"frame_rate_numerator"`
	FrameRateDenominator int64     `json:"frame_rate_denominator"`
	VideoPixelFormat     string    `json:"video_pixel_format"`
	VideoBitRateBPS      int64     `json:"video_bitrate_bps"`
	AudioCodec           string    `json:"audio_codec"`
	AudioProfile         string    `json:"audio_profile"`
	AudioBitRateBPS      int64     `json:"audio_bitrate_bps"`
	AudioSampleRateHz    int       `json:"audio_sample_rate_hz"`
	AudioChannels        int       `json:"audio_channels"`
	AudioChannelLayout   string    `json:"audio_channel_layout"`
	ProfileReference     string    `json:"profile_reference"`
}

func projectJobVideoInfo(value archivework.VideoInfo) jobVideoInfoView {
	return jobVideoInfoView{
		OriginalAssetID: value.OriginalAssetID, OriginalName: value.OriginalName, RelativePath: value.RelativePath,
		Checksum: value.Checksum, SourceCreatedAt: value.SourceCreatedAt, DurationMillis: value.DurationMillis,
		SizeBytes: value.SizeBytes, Container: value.Container, BitRateBPS: value.BitRateBPS,
		VideoCodec: value.VideoCodec, VideoProfile: value.VideoProfile, Width: value.Width, Height: value.Height,
		FrameRateNumerator: value.FrameRateNumerator, FrameRateDenominator: value.FrameRateDenominator,
		VideoPixelFormat: value.VideoPixelFormat, VideoBitRateBPS: value.VideoBitRateBPS,
		AudioCodec: value.AudioCodec, AudioProfile: value.AudioProfile, AudioBitRateBPS: value.AudioBitRateBPS,
		AudioSampleRateHz: value.AudioSampleRateHz, AudioChannels: value.AudioChannels,
		AudioChannelLayout: value.AudioChannelLayout, ProfileReference: value.ProfileReference,
	}
}

func projectWorkSummary(value archivework.Summary) workSummaryView {
	return workSummaryView{
		WorkID: value.WorkID, Kind: string(value.Kind), State: string(value.State), CreatedAt: value.CreatedAt,
		LastEventAt: value.LastEventAt, WorkerID: value.WorkerID, SourceVolumeID: value.SourceVolumeID,
		SourceStorageID: value.SourceStorageID, ArchiveStorageID: value.ArchiveStorageID,
		FolderCount: value.FolderCount, VideoCount: value.VideoCount, HoldCount: value.HoldCount,
		ResultCount: value.ResultCount, ChildJobCount: value.ChildJobCount, Progress: value.Progress, Title: value.Title,
		ReplacementWorkID: value.ReplacementWorkID,
	}
}

func projectStandaloneWork(value archiveworkspace.JobSummary) workSummaryView {
	state := archivework.StateQueued
	if value.StepState == workflow.Failed || value.ErrorClass != "" {
		state = archivework.StateFailed
	} else if value.NeedsHumanIntervention {
		state = archivework.StateAttention
	} else {
		switch value.StepState {
		case workflow.Reserved, workflow.Running, workflow.Verifying:
			state = archivework.StateActive
		case workflow.Completed, workflow.Cancelled:
			state = archivework.StateCompleted
		}
	}
	return projectWorkSummary(archivework.Summary{
		WorkID: value.JobID, Kind: archivework.StandaloneJob, State: state, CreatedAt: value.LastEventAt,
		LastEventAt: value.LastEventAt, WorkerID: value.WorkerID, ChildJobCount: 1, Progress: value.Progress, Title: value.TargetTitle,
	})
}

func projectWorkDetail(value archivework.View) workDetailView {
	result := workDetailView{Summary: projectWorkSummary(value.Summary), FolderIDs: append([]string(nil), value.FolderIDs...)}
	for _, child := range value.Children {
		result.Children = append(result.Children, workChildJobView{Role: string(child.Role), Folder: child.Folder, Job: projectJobSummary(child.Job)})
	}
	for _, completed := range value.CompletedWork {
		result.CompletedWork = append(result.CompletedWork, driveCompletedWorkView{
			ArchiveRunID: completed.ArchiveRunID, Folder: completed.Folder, JobID: completed.JobID,
			Disposition: string(completed.Disposition), ProfileReference: completed.ProfileReference,
			SourceVideoCount: completed.SourceVideoCount, ArchiveStorageID: completed.ArchiveStorageID,
			ArchivePackageID: completed.ArchivePackageID, ArchiveDate: completed.ArchiveDate,
			ArchiveDisplayName: completed.ArchiveDisplayName, ArchiveVideoFileName: completed.ArchiveVideoFileName,
			ArchiveRelativePath: completed.ArchiveRelativePath,
			ArchiveState:        string(completed.ArchiveState), CompletedAt: completed.CompletedAt,
		})
	}
	for _, failure := range value.Failures {
		result.Failures = append(result.Failures, workFailureView{
			JobID: failure.JobID, Folder: failure.Folder, StepID: failure.StepID,
			ErrorClass: failure.ErrorClass, LogReference: failure.LogReference,
		})
	}
	return result
}

func projectJobSummary(value archiveworkspace.JobSummary) jobSummaryView {
	result := jobSummaryView{
		JobID: value.JobID, Revision: value.Revision, TemplateName: value.TemplateName, TargetTitle: value.TargetTitle,
		CurrentStepID: value.CurrentStepID, StepState: string(value.StepState),
		Progress: value.Progress, ExecutorKind: string(value.ExecutorKind), ActorID: value.ActorID, WorkerID: value.WorkerID,
		ArchivistAction: value.ArchivistAction, ToolCallStatus: string(value.ToolCallStatus), WaitReason: string(value.WaitReason),
		ErrorClass: value.ErrorClass, LastEventAt: value.LastEventAt, LogReference: value.LogReference,
		NeedsHumanIntervention: value.NeedsHumanIntervention, InterventionReason: value.InterventionReason,
	}
	if value.Worker != nil {
		worker := projectWorker(*value.Worker)
		result.Worker = &worker
	}
	return result
}

func projectJobDetail(value archiveworkspace.JobView) jobDetailView {
	result := jobDetailView{Summary: projectJobSummary(value.Summary)}
	for _, step := range value.Steps {
		result.Steps = append(result.Steps, stepView{
			ID: step.ID, State: string(step.State), Progress: step.Progress, ExecutorKind: string(step.ExecutorKind),
			ActorID: step.ActorID, WorkerID: step.WorkerID, WaitReason: string(step.WaitReason), ErrorClass: step.ErrorClass,
			LogReference: step.LogReference, LastEventAt: step.LastEventAt,
			DependencyIDs: append([]string(nil), step.DependencyIDs...),
		})
	}
	for _, action := range value.ArchivistActions {
		result.ArchivistActions = append(result.ArchivistActions, archivistActionView{
			Action: action.Action, ToolCallID: action.ToolCallID,
			ToolCallStatus: string(action.ToolCallStatus), OccurredAt: action.OccurredAt,
		})
	}
	for _, worker := range value.Workers {
		result.Workers = append(result.Workers, projectWorker(worker))
	}
	for _, item := range value.Timeline {
		result.Timeline = append(result.Timeline, timelineView{
			Kind: item.Kind, Reference: item.Reference, Summary: item.Summary, OccurredAt: item.OccurredAt,
		})
	}
	return result
}

func projectWorker(value archiveworkspace.WorkerResource) workerView {
	return workerView{
		WorkerID: value.WorkerID, CPUPercent: value.CPUPercent, GPUPercent: value.GPUPercent,
		AvailableMemoryBytes: value.AvailableMemoryBytes, AvailableVRAMBytes: value.AvailableVRAMBytes,
		ScratchFreeBytes: value.ScratchFreeBytes, HeavyRunning: value.HeavyRunning, AcceptingWork: value.AcceptingWork,
		AccessibleStorageIDs: append([]string(nil), value.AccessibleStorageIDs...), HeartbeatAt: value.HeartbeatAt,
	}
}
