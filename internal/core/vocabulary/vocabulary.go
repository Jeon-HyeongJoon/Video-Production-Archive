// Package vocabulary contains the shared value types used across Core domains.
// The values mirror the decided enums in ontology/terms.json. Domain packages
// may expose aliases, but must not declare competing string types.
package vocabulary

type ActorType string

const (
	ActorHuman     ActorType = "HUMAN"
	ActorArchivist ActorType = "ARCHIVIST"
	ActorWorker    ActorType = "WORKER"
	ActorSystem    ActorType = "SYSTEM"
)

func (value ActorType) Valid() bool {
	switch value {
	case ActorHuman, ActorArchivist, ActorWorker, ActorSystem:
		return true
	default:
		return false
	}
}

type UserRole string

const (
	RoleOperator UserRole = "OPERATOR"
	RoleAdmin    UserRole = "ADMIN"
)

func (value UserRole) Valid() bool { return value == RoleOperator || value == RoleAdmin }

type WaitReason string

const (
	WaitIdle     WaitReason = "IDLE"
	WaitMemory   WaitReason = "MEMORY"
	WaitScratch  WaitReason = "SCRATCH"
	WaitSlot     WaitReason = "SLOT"
	WaitDisk     WaitReason = "DISK"
	WaitApproval WaitReason = "APPROVAL"
	WaitInput    WaitReason = "INPUT"
	WaitDevice   WaitReason = "DEVICE"
	WaitPriority WaitReason = "PRIORITY"
)

func (value WaitReason) Valid() bool {
	switch value {
	case WaitIdle, WaitMemory, WaitScratch, WaitSlot, WaitDisk, WaitApproval, WaitInput, WaitDevice, WaitPriority:
		return true
	default:
		return false
	}
}

type ProcessingKind string

const (
	ProcessingInspect      ProcessingKind = "INSPECT"
	ProcessingRender       ProcessingKind = "RENDER"
	ProcessingThumbnail    ProcessingKind = "THUMBNAIL"
	ProcessingAudioExtract ProcessingKind = "AUDIO_EXTRACT"
	ProcessingSTT          ProcessingKind = "STT"
	ProcessingSummary      ProcessingKind = "SUMMARY"
	ProcessingQC           ProcessingKind = "QC"
	ProcessingChecksum     ProcessingKind = "CHECKSUM"
	ProcessingDevice       ProcessingKind = "DEVICE"
	ProcessingOrganize     ProcessingKind = "ORGANIZE"
)

func (value ProcessingKind) Valid() bool {
	switch value {
	case ProcessingInspect, ProcessingRender, ProcessingThumbnail, ProcessingAudioExtract,
		ProcessingSTT, ProcessingSummary, ProcessingQC, ProcessingChecksum, ProcessingDevice, ProcessingOrganize:
		return true
	default:
		return false
	}
}

type OrganizationDisposition string

const (
	OrganizationAsIs                  OrganizationDisposition = "ORGANIZE_AS_IS"
	OrganizationRenderRecordings      OrganizationDisposition = "RENDER_RECORDINGS"
	OrganizationCompileSourceSequence OrganizationDisposition = "COMPILE_SOURCE_SEQUENCE"
	OrganizationExcludeFromRun        OrganizationDisposition = "EXCLUDE_FROM_RUN"
	OrganizationHold                  OrganizationDisposition = "HOLD"
)

func (value OrganizationDisposition) ProducesMedia() bool {
	switch value {
	case OrganizationAsIs, OrganizationRenderRecordings, OrganizationCompileSourceSequence:
		return true
	default:
		return false
	}
}

func (value OrganizationDisposition) Valid() bool {
	return value.ProducesMedia() || value == OrganizationHold || value == OrganizationExcludeFromRun
}

type OrganizationHoldReason string

const (
	OrganizationHoldNoMedia                 OrganizationHoldReason = "NO_MEDIA"
	OrganizationHoldMissingInspection       OrganizationHoldReason = "MISSING_INSPECTION"
	OrganizationHoldAmbiguousMultipleVideos OrganizationHoldReason = "AMBIGUOUS_MULTIPLE_VIDEOS"
	OrganizationHoldMixedReadyState         OrganizationHoldReason = "MIXED_READY_STATE"
	OrganizationHoldNonconsecutiveSequence  OrganizationHoldReason = "NONCONSECUTIVE_SEQUENCE"
	OrganizationHoldIncompatibleSequence    OrganizationHoldReason = "INCOMPATIBLE_SEQUENCE"
	OrganizationHoldSourceRole              OrganizationHoldReason = "SOURCE_ROLE_REVIEW"
	OrganizationHoldInvalidOverride         OrganizationHoldReason = "INVALID_OVERRIDE"
)

func (value OrganizationHoldReason) Valid() bool {
	switch value {
	case OrganizationHoldNoMedia, OrganizationHoldMissingInspection, OrganizationHoldAmbiguousMultipleVideos,
		OrganizationHoldMixedReadyState, OrganizationHoldNonconsecutiveSequence,
		OrganizationHoldIncompatibleSequence, OrganizationHoldSourceRole, OrganizationHoldInvalidOverride:
		return true
	default:
		return false
	}
}

type ResourceClass string

const (
	ResourceHeavy   ResourceClass = "HEAVY"
	ResourceLight   ResourceClass = "LIGHT"
	ResourceControl ResourceClass = "CONTROL"
)

func (value ResourceClass) Valid() bool {
	return value == ResourceHeavy || value == ResourceLight || value == ResourceControl
}

type StorageRole string

const (
	StorageSourceMedia    StorageRole = "SOURCE_MEDIA"
	StorageScratch        StorageRole = "SCRATCH"
	StorageWorking        StorageRole = "WORKING"
	StorageTransferMedia  StorageRole = "TRANSFER_MEDIA"
	StorageArchivePrimary StorageRole = "ARCHIVE_PRIMARY"
	StorageArchiveMirror  StorageRole = "ARCHIVE_MIRROR"
	StorageBackupOffsite  StorageRole = "BACKUP_OFFSITE"
)

func (value StorageRole) Valid() bool {
	switch value {
	case StorageSourceMedia, StorageScratch, StorageWorking, StorageTransferMedia,
		StorageArchivePrimary, StorageArchiveMirror, StorageBackupOffsite:
		return true
	default:
		return false
	}
}

// StorageAccessKind describes how a Worker reaches a Storage. It is separate
// from StorageRole: the same archive role can be backed by a local root, a
// mounted share, an attached volume, or a remote file-service Adapter.
type StorageAccessKind string

const (
	StorageAccessLocal          StorageAccessKind = "LOCAL"
	StorageAccessMountedNetwork StorageAccessKind = "MOUNTED_NETWORK"
	StorageAccessAttachedVolume StorageAccessKind = "ATTACHED_VOLUME"
	StorageAccessFTP            StorageAccessKind = "FTP"
	StorageAccessSFTP           StorageAccessKind = "SFTP"
)

func (value StorageAccessKind) Valid() bool {
	switch value {
	case StorageAccessLocal, StorageAccessMountedNetwork, StorageAccessAttachedVolume,
		StorageAccessFTP, StorageAccessSFTP:
		return true
	default:
		return false
	}
}

// VolumeConnectionState is the current observability of a logical data
// volume from its Worker. It is deliberately separate from archive progress:
// a completed historical run can remain addressable after its source volume
// has been detached.
type VolumeConnectionState string

const (
	VolumeConnected    VolumeConnectionState = "CONNECTED"
	VolumeDisconnected VolumeConnectionState = "DISCONNECTED"
	VolumeUnknown      VolumeConnectionState = "UNKNOWN"
)

func (value VolumeConnectionState) Valid() bool {
	return value == VolumeConnected || value == VolumeDisconnected || value == VolumeUnknown
}

// FolderDiscoveryState reports whether a Worker's source-folder inventory is
// still partial, complete and revisioned, or unavailable because discovery
// failed. SCANNING observations are display-only and cannot define a run.
type FolderDiscoveryState string

const (
	FolderDiscoveryScanning FolderDiscoveryState = "SCANNING"
	FolderDiscoveryReady    FolderDiscoveryState = "READY"
	FolderDiscoveryFailed   FolderDiscoveryState = "FAILED"
)

func (value FolderDiscoveryState) Valid() bool {
	return value == FolderDiscoveryScanning || value == FolderDiscoveryReady || value == FolderDiscoveryFailed
}

type ExecutorKind string

const (
	ExecutorWorker           ExecutorKind = "WORKER"
	ExecutorArchivistGateway ExecutorKind = "ARCHIVIST_GATEWAY"
	ExecutorControl          ExecutorKind = "CONTROL"
	ExecutorHuman            ExecutorKind = "HUMAN"
)

func (value ExecutorKind) Valid() bool {
	switch value {
	case ExecutorWorker, ExecutorArchivistGateway, ExecutorControl, ExecutorHuman:
		return true
	default:
		return false
	}
}

type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "APPROVED"
	ApprovalRejected ApprovalDecision = "REJECTED"
)

func (value ApprovalDecision) Valid() bool {
	return value == ApprovalApproved || value == ApprovalRejected
}

type ActionClass string

const (
	ActionRead        ActionClass = "READ"
	ActionProcess     ActionClass = "PROCESS"
	ActionGated       ActionClass = "GATED"
	ActionDestructive ActionClass = "DESTRUCTIVE"
	ActionManage      ActionClass = "MANAGE"
)

func (value ActionClass) Valid() bool {
	switch value {
	case ActionRead, ActionProcess, ActionGated, ActionDestructive, ActionManage:
		return true
	default:
		return false
	}
}

type ContentReviewDecision string

const (
	ContentAccepted         ContentReviewDecision = "ACCEPTED"
	ContentChangesRequested ContentReviewDecision = "CHANGES_REQUESTED"
)

func (value ContentReviewDecision) Valid() bool {
	return value == ContentAccepted || value == ContentChangesRequested
}

type RuntimeBackend string

const (
	RuntimeEmbeddedCore RuntimeBackend = "EMBEDDED_CORE"
	RuntimeOrcaCLI      RuntimeBackend = "ORCA_CLI"
)

func (value RuntimeBackend) Valid() bool {
	return value == RuntimeEmbeddedCore || value == RuntimeOrcaCLI
}

type ArchivistHostState string

const (
	ArchivistHostConnecting ArchivistHostState = "CONNECTING"
	ArchivistHostOnline     ArchivistHostState = "ONLINE"
	ArchivistHostBusy       ArchivistHostState = "BUSY"
	ArchivistHostOffline    ArchivistHostState = "OFFLINE"
	ArchivistHostDisabled   ArchivistHostState = "DISABLED"
)

func (value ArchivistHostState) Valid() bool {
	switch value {
	case ArchivistHostConnecting, ArchivistHostOnline, ArchivistHostBusy, ArchivistHostOffline, ArchivistHostDisabled:
		return true
	default:
		return false
	}
}

type ArchivistTurnState string

const (
	ArchivistTurnQueued    ArchivistTurnState = "QUEUED"
	ArchivistTurnLeased    ArchivistTurnState = "LEASED"
	ArchivistTurnSubmitted ArchivistTurnState = "SUBMITTED"
	ArchivistTurnRunning   ArchivistTurnState = "RUNNING"
	ArchivistTurnWaiting   ArchivistTurnState = "WAITING"
	ArchivistTurnCompleted ArchivistTurnState = "COMPLETED"
	ArchivistTurnFailed    ArchivistTurnState = "FAILED"
	ArchivistTurnCancelled ArchivistTurnState = "CANCELLED"
)

func (value ArchivistTurnState) Valid() bool {
	switch value {
	case ArchivistTurnQueued, ArchivistTurnLeased, ArchivistTurnSubmitted, ArchivistTurnRunning,
		ArchivistTurnWaiting, ArchivistTurnCompleted, ArchivistTurnFailed, ArchivistTurnCancelled:
		return true
	default:
		return false
	}
}
