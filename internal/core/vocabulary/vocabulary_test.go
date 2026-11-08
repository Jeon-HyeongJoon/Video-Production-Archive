package vocabulary

import "testing"

func TestEveryDecidedVocabularyValueIsValid(t *testing.T) {
	actorTypes := []ActorType{ActorHuman, ActorArchivist, ActorWorker, ActorSystem}
	for _, value := range actorTypes {
		if !value.Valid() {
			t.Fatalf("ActorType %q is not valid", value)
		}
	}
	roles := []UserRole{RoleOperator, RoleAdmin}
	for _, value := range roles {
		if !value.Valid() {
			t.Fatalf("UserRole %q is not valid", value)
		}
	}
	waitReasons := []WaitReason{WaitIdle, WaitMemory, WaitScratch, WaitSlot, WaitDisk, WaitApproval, WaitInput, WaitDevice, WaitPriority}
	for _, value := range waitReasons {
		if !value.Valid() {
			t.Fatalf("WaitReason %q is not valid", value)
		}
	}
	processingKinds := []ProcessingKind{
		ProcessingInspect, ProcessingRender, ProcessingThumbnail, ProcessingAudioExtract,
		ProcessingSTT, ProcessingSummary, ProcessingQC, ProcessingChecksum, ProcessingDevice,
	}
	for _, value := range processingKinds {
		if !value.Valid() {
			t.Fatalf("ProcessingKind %q is not valid", value)
		}
	}
	resourceClasses := []ResourceClass{ResourceHeavy, ResourceLight, ResourceControl}
	for _, value := range resourceClasses {
		if !value.Valid() {
			t.Fatalf("ResourceClass %q is not valid", value)
		}
	}
	storageRoles := []StorageRole{
		StorageSourceMedia, StorageScratch, StorageWorking, StorageTransferMedia,
		StorageArchivePrimary, StorageArchiveMirror, StorageBackupOffsite,
	}
	for _, value := range storageRoles {
		if !value.Valid() {
			t.Fatalf("StorageRole %q is not valid", value)
		}
	}
	storageAccessKinds := []StorageAccessKind{
		StorageAccessLocal, StorageAccessMountedNetwork, StorageAccessAttachedVolume,
		StorageAccessFTP, StorageAccessSFTP,
	}
	for _, value := range storageAccessKinds {
		if !value.Valid() {
			t.Fatalf("StorageAccessKind %q is not valid", value)
		}
	}
	volumeConnectionStates := []VolumeConnectionState{VolumeConnected, VolumeDisconnected, VolumeUnknown}
	for _, value := range volumeConnectionStates {
		if !value.Valid() {
			t.Fatalf("VolumeConnectionState %q is not valid", value)
		}
	}
	executorKinds := []ExecutorKind{ExecutorWorker, ExecutorArchivistGateway, ExecutorControl, ExecutorHuman}
	for _, value := range executorKinds {
		if !value.Valid() {
			t.Fatalf("ExecutorKind %q is not valid", value)
		}
	}
	approvalDecisions := []ApprovalDecision{ApprovalApproved, ApprovalRejected}
	for _, value := range approvalDecisions {
		if !value.Valid() {
			t.Fatalf("ApprovalDecision %q is not valid", value)
		}
	}
	reviewDecisions := []ContentReviewDecision{ContentAccepted, ContentChangesRequested}
	for _, value := range reviewDecisions {
		if !value.Valid() {
			t.Fatalf("ContentReviewDecision %q is not valid", value)
		}
	}
	runtimeBackends := []RuntimeBackend{RuntimeEmbeddedCore, RuntimeOrcaCLI}
	for _, value := range runtimeBackends {
		if !value.Valid() {
			t.Fatalf("RuntimeBackend %q is not valid", value)
		}
	}
	archivistHostStates := []ArchivistHostState{ArchivistHostConnecting, ArchivistHostOnline, ArchivistHostBusy, ArchivistHostOffline, ArchivistHostDisabled}
	for _, value := range archivistHostStates {
		if !value.Valid() {
			t.Fatalf("ArchivistHostState %q is not valid", value)
		}
	}
	archivistTurnStates := []ArchivistTurnState{
		ArchivistTurnQueued, ArchivistTurnLeased, ArchivistTurnSubmitted, ArchivistTurnRunning,
		ArchivistTurnWaiting, ArchivistTurnCompleted, ArchivistTurnFailed, ArchivistTurnCancelled,
	}
	for _, value := range archivistTurnStates {
		if !value.Valid() {
			t.Fatalf("ArchivistTurnState %q is not valid", value)
		}
	}

	if ActorType("UNKNOWN").Valid() || UserRole("UNKNOWN").Valid() || WaitReason("UNKNOWN").Valid() ||
		ProcessingKind("UNKNOWN").Valid() || ResourceClass("UNKNOWN").Valid() || StorageRole("UNKNOWN").Valid() ||
		StorageAccessKind("UNKNOWN").Valid() || VolumeConnectionState("INVALID").Valid() ||
		ExecutorKind("UNKNOWN").Valid() ||
		ApprovalDecision("UNKNOWN").Valid() || ContentReviewDecision("UNKNOWN").Valid() ||
		RuntimeBackend("UNKNOWN").Valid() || ArchivistHostState("UNKNOWN").Valid() || ArchivistTurnState("UNKNOWN").Valid() {
		t.Fatal("unknown vocabulary value was accepted")
	}
}
