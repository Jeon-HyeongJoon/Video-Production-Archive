package archiveintakeworkspace_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

// Requirement: DEV-010
func TestWorkerInventoryProjectsUnclassifiedFoldersAndRoleCapacityStorageCandidates(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	candidates := candidateInventory{{
		VolumeID: "SOURCE-VOLUME", Revision: 3, PhysicalDeviceID: "SOURCE-PHYSICAL", WorkerID: "WORKER-WINDOWS",
		StorageID: "source-main", Connected: true, ObservedAt: now,
	}, {
		VolumeID: "ZZ-DETACHED-VOLUME", Revision: 4, PhysicalDeviceID: "DETACHED-PHYSICAL", WorkerID: "WORKER-WINDOWS",
		StorageID: "source-detached", Connected: false, ObservedAt: now,
	}}
	storages := storageInventory{
		{StorageID: "archive-local", Role: vocabulary.StorageArchivePrimary, Root: filepath.Join(root, "archive"), WorkspaceDriveID: "ARCHIVE-VOLUME", PhysicalDeviceID: "ARCHIVE-PHYSICAL"},
		{StorageID: "archive-nas", Role: vocabulary.StorageArchivePrimary, Root: filepath.Join(root, "nas"), WorkspaceDriveID: "NAS-VOLUME", PhysicalDeviceID: "NAS-ENDPOINT"},
		{StorageID: "system-scratch", Role: vocabulary.StorageScratch, Root: filepath.Join(root, "system"), WorkspaceDriveID: "SYSTEM-VOLUME", PhysicalDeviceID: "SYSTEM-PHYSICAL"},
	}
	statuses := statusInventory{{
		WorkerID: "WORKER-WINDOWS", State: "ONLINE", Metrics: worker.Metrics{
			ObservedAt:           now,
			AccessibleStorageIDs: map[string]bool{"source-main": true, "archive-local": true, "archive-nas": true, "system-scratch": true},
			AttachedVolumes: []worker.AttachedVolumeObservation{{
				PhysicalDeviceID: "SOURCE-PHYSICAL", VolumeID: "SOURCE-VOLUME", StorageID: "source-main", ArchiveStorageID: "archive-source-volume",
				Model: "WDC EXTERNAL", Label: "2TB 하드", FileSystem: "NTFS", CapacityBytes: 1_000, FreeBytes: 300,
			}, {
				PhysicalDeviceID: "SOURCE-PHYSICAL", VolumeID: "NEW-ARCHIVE-VOLUME", StorageID: "source-new-archive-drive", ArchiveStorageID: "new-archive-drive",
				Model: "TOSHIBA EXTERNAL", Label: "새 아카이브", FileSystem: "NTFS", CapacityBytes: 4_000, FreeBytes: 3_500,
			}},
			StorageObservations: []worker.StorageObservation{
				{StorageID: "source-main", Connected: true, AccessKind: vocabulary.StorageAccessAttachedVolume, DiscoveryState: vocabulary.FolderDiscoveryReady, CapacityBytes: 1_000, FreeBytes: 300, DiscoveryRevision: 41,
					Folders: []worker.MediaFolderObservation{{Folder: "event-a", VideoCount: 12, InputBytes: 600, Videos: []worker.MediaFileObservation{{RelativePath: "event-a/C0001.MP4", FileName: "C0001.MP4", SizeBytes: 400, ModifiedAt: now.Add(-time.Hour)}}}, {Folder: "event-b", VideoCount: 4, InputBytes: 200}}},
				{StorageID: "archive-local", Connected: true, AccessKind: vocabulary.StorageAccessLocal, DiscoveryState: vocabulary.FolderDiscoveryReady, CapacityBytes: 2_000, FreeBytes: 1_400, DiscoveryRevision: 1},
				{StorageID: "archive-nas", Connected: false, AccessKind: vocabulary.StorageAccessMountedNetwork, DiscoveryState: vocabulary.FolderDiscoveryFailed, CapacityBytes: 8_000, FreeBytes: 7_000},
				{StorageID: "system-scratch", Connected: true, AccessKind: vocabulary.StorageAccessLocal, DiscoveryState: vocabulary.FolderDiscoveryReady, CapacityBytes: 500, FreeBytes: 100, DiscoveryRevision: 1},
			},
		},
	}}
	sessions := sessionInventory{
		{ID: "INTAKE-CURRENT", Revision: 1, CandidateRevision: 3, VolumeID: "SOURCE-VOLUME", WorkspaceDriveID: "SOURCE-VOLUME", PhysicalDeviceID: "SOURCE-PHYSICAL", WorkerID: "WORKER-WINDOWS", StorageID: "source-main", SelectedBy: "operator", SelectedAt: now.Add(-time.Minute)},
		{ID: "INTAKE-STALE-NEWER", Revision: 1, CandidateRevision: 2, VolumeID: "SOURCE-VOLUME", WorkspaceDriveID: "SOURCE-VOLUME", PhysicalDeviceID: "SOURCE-PHYSICAL", WorkerID: "WORKER-WINDOWS", StorageID: "source-main", SelectedBy: "operator", SelectedAt: now},
	}
	service, err := archiveintakeworkspace.New(workstationStore{}, workstationJobs{},
		archiveintakeworkspace.WithIntakeSessions(sessions),
		archiveintakeworkspace.WithInventory(candidates, storages, statuses, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	views, err := service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("unclassified drive missing: %+v", views)
	}
	view := views[0]
	if view.State != "UNCLASSIFIED" || view.ArchiveRunID != "" || view.WorkspaceDriveID != "SOURCE-VOLUME" ||
		view.SessionID != "INTAKE-CURRENT" || view.SelectionSessionID != "INTAKE-CURRENT" ||
		view.WorkerID != "WORKER-WINDOWS" || view.CandidateRevision != 3 || view.FolderDiscoveryRevision != 41 ||
		view.ConnectionState != vocabulary.VolumeConnected || view.ConnectionWorkerID != "WORKER-WINDOWS" || !view.ConnectionObservedAt.Equal(now) ||
		view.FolderDiscoveryState != vocabulary.FolderDiscoveryReady ||
		view.InputRevision != 41 || view.FolderCount != 2 || view.FileCount != 16 || len(view.StorageCandidates) != 4 {
		t.Fatalf("pre-run source inventory was not projected: %+v", view)
	}
	if view.Folders[0].Folder != "event-a" || view.Folders[0].VideoCount != 12 || view.Folders[0].EstimatedOutputBytes != 600 ||
		len(view.Folders[0].Videos) != 1 || view.Folders[0].Videos[0].RelativePath != "event-a/C0001.MP4" ||
		view.Folders[0].Videos[0].FileName != "C0001.MP4" || view.Folders[0].Videos[0].SizeBytes != 400 ||
		view.Folders[1].Folder != "event-b" || view.Folders[1].VideoCount != 4 {
		t.Fatalf("pre-run folder inventory mismatch: %+v", view.Folders)
	}
	local := storageView(t, view, "archive-local")
	network := storageView(t, view, "archive-nas")
	attached := storageView(t, view, "new-archive-drive")
	source := storageView(t, view, "source-main")
	if local.Role != vocabulary.StorageArchivePrimary || local.AccessKind != vocabulary.StorageAccessLocal || !local.Accessible || local.FreeBytes != 1_400 ||
		network.AccessKind != vocabulary.StorageAccessMountedNetwork || network.Accessible || network.CapacityBytes != 8_000 ||
		attached.Role != vocabulary.StorageArchivePrimary || attached.AccessKind != vocabulary.StorageAccessAttachedVolume ||
		!attached.Accessible || attached.Label != "새 아카이브" || attached.WorkspaceDriveID != "NEW-ARCHIVE-VOLUME" ||
		source.Role != vocabulary.StorageSourceMedia || source.Label != "2TB 하드" || source.Model != "WDC EXTERNAL" ||
		source.FileSystem != "NTFS" || source.CapacityBytes != 1_000 || source.AccessKind != vocabulary.StorageAccessAttachedVolume {
		t.Fatalf("source or archive role, identity, access, or capacity missing: source=%+v local=%+v network=%+v attached=%+v", source, local, network, attached)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), root) || strings.Contains(string(payload), "system-scratch") {
		t.Fatalf("projection exposed a root or selectable system storage: %s", payload)
	}
	detached := views[1]
	if detached.WorkspaceDriveID != "ZZ-DETACHED-VOLUME" || detached.ConnectionState != vocabulary.VolumeDisconnected ||
		detached.ConnectionWorkerID != "WORKER-WINDOWS" || !detached.ConnectionObservedAt.Equal(now) {
		t.Fatalf("detached source was not synchronized from the current Worker snapshot: %+v", detached)
	}
	detachedSource := storageView(t, detached, "source-detached")
	if detachedSource.Accessible || detachedSource.Role != vocabulary.StorageSourceMedia ||
		detachedSource.AccessKind != vocabulary.StorageAccessAttachedVolume {
		t.Fatalf("detached source remained accessible: %+v", detachedSource)
	}

	decision := organization.OrganizationDecision{
		ID: "DECISION-EVENT-A", Sequence: 1, WorkspaceDriveID: "SOURCE-VOLUME", Folder: "event-a", InputRevision: 41,
		Disposition: vocabulary.OrganizationHold, HoldReasons: []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
	}
	sessions = append(sessions, archiveintake.Session{
		ID: "INTAKE-1", Revision: 1, CandidateRevision: 1, VolumeID: "SOURCE-VOLUME", WorkspaceDriveID: "SOURCE-VOLUME",
		PhysicalDeviceID: "SOURCE-PHYSICAL", WorkerID: "WORKER-WINDOWS", StorageID: "source-main", SelectedBy: "operator", SelectedAt: now.Add(-2 * time.Hour),
	})
	service, err = archiveintakeworkspace.New(workstationStore{
		receipts: []organization.ArchiveOrganizationReceipt{{
			ID: "ARCHIVE-RUN-1", SessionID: "INTAKE-1", InputRevision: 41, WorkspaceDriveID: "SOURCE-VOLUME", WorkerID: "WORKER-WINDOWS",
			Files:   []organization.CoordinatedFile{{RelativePath: "event-a/a.mov"}},
			Folders: []organization.CoordinatedFolder{{Folder: "event-a", Decision: decision}}, CompletedAt: now,
		}}, decisions: map[string]organization.OrganizationDecision{"event-a": decision},
	}, workstationJobs{"ARCHIVE-RUN-1": archiveRunSnapshot(t, archiverun.State{
		ID: "ARCHIVE-RUN-1", Revision: 1, WorkerID: "WORKER-WINDOWS", SourceVolumeID: "SOURCE-VOLUME",
		SourcePhysicalDeviceID: "SOURCE-PHYSICAL", SourceStorageID: "source-main", ArchiveStorageID: "archive-local",
		FolderDiscoveryRevision: 41, FolderIDs: []string{"event-a"},
		SelectedBy:      command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"},
		StageOwnerships: []archiverun.StageOwnership{{Kind: archiverun.OrganizationStage}, {Kind: archiverun.TranscriptionStage}}, CreatedAt: now,
	})}, archiveintakeworkspace.WithIntakeSessions(sessions), archiveintakeworkspace.WithInventory(candidates, storages, statuses, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	views, err = service.Projection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view = views[0]
	if view.SessionID != "INTAKE-1" || view.SelectionSessionID != "INTAKE-CURRENT" ||
		view.FolderCount != 1 || view.FileCount != 1 || len(view.Folders) != 1 || view.Folders[0].Folder != "event-a" ||
		view.DiscoveredFolderCount != 2 || view.DiscoveredFileCount != 16 || len(view.DiscoveredFolders) != 2 ||
		view.DiscoveredFolders[1].Folder != "event-b" {
		t.Fatalf("fixed ArchiveRun scope replaced the reusable drive inventory: %+v", view)
	}
}

func storageView(t *testing.T, drive archiveintakeworkspace.DriveView, storageID string) archiveintakeworkspace.StorageView {
	t.Helper()
	for _, view := range drive.StorageCandidates {
		if view.StorageID == storageID {
			return view
		}
	}
	t.Fatalf("storage %s missing", storageID)
	return archiveintakeworkspace.StorageView{}
}

type candidateInventory []archiveintake.Candidate

func (inventory candidateInventory) ListCandidates(context.Context) ([]archiveintake.Candidate, error) {
	return append([]archiveintake.Candidate(nil), inventory...), nil
}

type storageInventory []storage.Access

func (inventory storageInventory) ListAccess(context.Context) ([]storage.Access, error) {
	return append([]storage.Access(nil), inventory...), nil
}

type statusInventory []worker.Status

func (inventory statusInventory) Statuses(time.Time) []worker.Status {
	return append([]worker.Status(nil), inventory...)
}

type sessionInventory []archiveintake.Session

func (inventory sessionInventory) Session(_ context.Context, id string) (archiveintake.Session, error) {
	for _, session := range inventory {
		if session.ID == id {
			return session, nil
		}
	}
	return archiveintake.Session{}, archiveintake.ErrInvalidSession
}

func (inventory sessionInventory) ListSessions(context.Context) ([]archiveintake.Session, error) {
	return append([]archiveintake.Session(nil), inventory...), nil
}
