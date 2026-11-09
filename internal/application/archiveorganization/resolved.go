package archiveorganization

import (
	"context"
	"strings"
	"time"

	appintake "videoarchive/internal/application/archiveintake"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
)

type ResolvedFolderService struct {
	jobs JobBus
}

func NewResolvedFolderService(jobs JobBus) (*ResolvedFolderService, error) {
	if jobs == nil {
		return nil, ErrInvalidRequest
	}
	return &ResolvedFolderService{jobs: jobs}, nil
}

func (service *ResolvedFolderService) StartResolvedFolder(
	_ context.Context,
	intakeSessionID string,
	idempotencyKey string,
	archiveRun archiverun.State,
	decision organization.OrganizationDecision,
	occurredAt time.Time,
) (string, error) {
	intakeSessionID = strings.TrimSpace(intakeSessionID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if service == nil || service.jobs == nil || intakeSessionID == "" || idempotencyKey == "" || occurredAt.IsZero() ||
		archiveRun.ID == "" || archiveRun.SourceStorageID == "" || archiveRun.SourcePhysicalDeviceID == "" ||
		archiveRun.ArchiveStorageID == "" || archiveRun.FolderDiscoveryRevision == 0 ||
		decision.Disposition == vocabulary.OrganizationHold || decision.InputRevision != archiveRun.FolderDiscoveryRevision ||
		decision.WorkspaceDriveID != archiveRun.SourceVolumeID || !archiveRunContainsFolder(archiveRun, decision.Folder) {
		return "", ErrInvalidRequest
	}
	coordinator := &Coordinator{jobs: service.jobs}
	return coordinator.createFolderJob(Request{
		ArchiveRunID: archiveRun.ID, IdempotencyKey: idempotencyKey,
		InputRevision: decision.InputRevision, ArchiveStorageID: archiveRun.ArchiveStorageID,
		Session: &appintake.SessionAccess{Session: coreintake.Session{
			ID: intakeSessionID, Revision: 1, VolumeID: archiveRun.SourceVolumeID, WorkspaceDriveID: archiveRun.SourceVolumeID,
			PhysicalDeviceID: archiveRun.SourcePhysicalDeviceID, StorageID: archiveRun.SourceStorageID,
			WorkerID: archiveRun.WorkerID, CandidateRevision: archiveRun.FolderDiscoveryRevision,
			SelectedByType: archiveRun.SelectedBy.Type, SelectedBy: archiveRun.SelectedBy.ID, SelectedAt: archiveRun.CreatedAt,
		}},
		StartedAt: occurredAt.UTC(), ExecutionPriority: resource.Immediate,
	}, decision)
}

func archiveRunContainsFolder(archiveRun archiverun.State, folder string) bool {
	folder = strings.TrimSpace(folder)
	for _, candidate := range archiveRun.FolderIDs {
		if candidate == folder {
			return true
		}
	}
	return false
}
