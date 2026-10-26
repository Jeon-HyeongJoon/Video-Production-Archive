// Package vocabulary contains the shared value types used across Core domains.
// The values mirror the decided enums in ontology/terms.json. Domain packages
// may expose aliases, but must not declare competing string types.
package vocabulary

type ActorType string

const (
	ActorHuman  ActorType = "HUMAN"
	ActorAgent  ActorType = "AGENT"
	ActorWorker ActorType = "WORKER"
	ActorSystem ActorType = "SYSTEM"
)

func (value ActorType) Valid() bool {
	switch value {
	case ActorHuman, ActorAgent, ActorWorker, ActorSystem:
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
)

func (value ProcessingKind) Valid() bool {
	switch value {
	case ProcessingInspect, ProcessingRender, ProcessingThumbnail, ProcessingAudioExtract,
		ProcessingSTT, ProcessingSummary, ProcessingQC, ProcessingChecksum, ProcessingDevice:
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

type ExecutorKind string

const (
	ExecutorWorker       ExecutorKind = "WORKER"
	ExecutorAgentGateway ExecutorKind = "AGENT_GATEWAY"
	ExecutorControl      ExecutorKind = "CONTROL"
	ExecutorHuman        ExecutorKind = "HUMAN"
)

func (value ExecutorKind) Valid() bool {
	switch value {
	case ExecutorWorker, ExecutorAgentGateway, ExecutorControl, ExecutorHuman:
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

type ContentReviewDecision string

const (
	ContentAccepted         ContentReviewDecision = "ACCEPTED"
	ContentChangesRequested ContentReviewDecision = "CHANGES_REQUESTED"
)

func (value ContentReviewDecision) Valid() bool {
	return value == ContentAccepted || value == ContentChangesRequested
}
