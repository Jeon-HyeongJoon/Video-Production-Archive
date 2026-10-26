package intake_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	intakeadapter "videoarchive/internal/adapter/intake"
	"videoarchive/internal/adapter/sqlitestorage"
	"videoarchive/internal/core/storage"
)

// Requirement: INT-001
func TestInputInspectionHashesRegisteredStorageFileWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	storageRoot := filepath.Join(directory, "source-media")
	if err := os.MkdirAll(filepath.Join(storageRoot, "camera-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("representative-camera-original")
	mediaPath := filepath.Join(storageRoot, "camera-a", "CLIP 001.MOV")
	if err := os.WriteFile(mediaPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}

	storages, err := sqlitestorage.Open(filepath.Join(directory, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storages.Close()
	if err := storages.Register(ctx, storage.Topology{
		PhysicalDeviceID: "DRIVE-USB-1", PartitionID: "PARTITION-1", VolumeID: "VOLUME-1",
		StorageID: "SOURCE-1", Root: storageRoot,
	}); err != nil {
		t.Fatal(err)
	}
	inspector, err := intakeadapter.NewInspector(storages)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := inspector.Inspect(ctx, intakeadapter.Request{
		ObservationID: "OBSERVATION-1", StorageID: "SOURCE-1", RelativePath: `camera-a\CLIP 001.MOV`, WorkerID: "WORKER-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(payload)
	if observation.ID != "OBSERVATION-1" || observation.StorageID != "SOURCE-1" || observation.WorkspaceDriveID != "DRIVE-USB-1" ||
		observation.RelativePath != "camera-a/CLIP 001.MOV" || observation.OriginalName != "CLIP 001.MOV" ||
		observation.SizeBytes != int64(len(payload)) || observation.Checksum != hex.EncodeToString(digest[:]) ||
		observation.ObservedBy != "WORKER-1" || observation.ObservedAt.IsZero() {
		t.Fatalf("input observation is incomplete: %+v", observation)
	}
	afterPayload, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterPayload) != string(payload) || after.Size() != before.Size() || after.ModTime() != before.ModTime() || after.Mode() != before.Mode() {
		t.Fatalf("input inspection changed original file: before=%+v after=%+v", before, after)
	}
}
