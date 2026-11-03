package sqlitestorage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: DEV-006
func TestStorageRootsResolveToLogicalVolumeWorkspaceDrive(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	topologies := []storage.Topology{
		{PhysicalDeviceID: "drive-a", PartitionID: "partition-a1", VolumeID: "volume-a1", StorageID: "source-a", Role: vocabulary.StorageSourceMedia, Root: "E:/source"},
		{PhysicalDeviceID: "drive-a", PartitionID: "partition-a2", VolumeID: "volume-a2", StorageID: "working-a", Role: vocabulary.StorageWorking, Root: "F:/working"},
		{PhysicalDeviceID: "drive-b", PartitionID: "partition-b1", VolumeID: "volume-b1", StorageID: "archive-b", Role: vocabulary.StorageArchivePrimary, Root: "G:/archive"},
	}
	for _, topology := range topologies {
		if err := repository.Register(ctx, topology); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]string{
		"source-a":  "volume-a1",
		"working-a": "volume-a2",
		"archive-b": "volume-b1",
	}
	for storageID, workspaceDriveID := range want {
		workspaceDrive, err := repository.ResolveWorkspaceDrive(ctx, storageID)
		if err != nil {
			t.Fatalf("resolve %s: %v", storageID, err)
		}
		if workspaceDrive.ID != workspaceDriveID {
			t.Fatalf("storage %s resolved to %s, want %s", storageID, workspaceDrive.ID, workspaceDriveID)
		}
		access, err := repository.ResolveAccess(ctx, storageID)
		if err != nil || access.PhysicalDeviceID == "" {
			t.Fatalf("storage %s lost its auxiliary physical I/O guard: access=%+v err=%v", storageID, access, err)
		}
	}

	if _, err := repository.ResolveWorkspaceDrive(ctx, "unregistered"); !errors.Is(err, storage.ErrWorkspaceDriveUnresolved) {
		t.Fatalf("unregistered storage returned %v", err)
	}
}
