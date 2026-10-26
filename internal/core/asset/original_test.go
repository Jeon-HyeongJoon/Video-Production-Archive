package asset

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/intake"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const checksumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Requirement: INT-002
func TestOriginalRegistrationUsesObservedContentIdentityWithoutStoragePath(t *testing.T) {
	observedAt := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	registeredAt := observedAt.Add(time.Minute)
	observation := intake.InputFileObservation{
		ID: "OBSERVATION-1", StorageID: "SOURCE-1", WorkspaceDriveID: "DRIVE-1",
		RelativePath: "camera/CLIP001.MOV", OriginalName: "CLIP001.MOV",
		SizeBytes: 1234, Checksum: checksumA, ObservedBy: "WORKER-1", ObservedAt: observedAt,
	}

	original, err := RegisterOriginalAsset("ASSET-1", observation, "SYSTEM-1", registeredAt)
	if err != nil {
		t.Fatal(err)
	}
	if original.ID != "ASSET-1" || original.Checksum != checksumA || original.OriginalName != "CLIP001.MOV" ||
		original.SizeBytes != 1234 || original.RegisteredFromObservationID != "OBSERVATION-1" ||
		original.RegisteredBy != "SYSTEM-1" || !original.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("original asset did not fix observed content identity: %+v", original)
	}
}

// Requirement: ARC-001
func TestOriginalAssetChecksumAndNameCannotChangeAfterIntake(t *testing.T) {
	asset := validOriginal("ASSET-1", checksumA, "camera-a.mov")
	if err := asset.ReplaceChecksum(checksumB); !errors.Is(err, ErrOriginalImmutable) {
		t.Fatalf("original checksum mutation accepted: %v", err)
	}
	if err := asset.ReplaceOriginalName("replacement.mov"); !errors.Is(err, ErrOriginalImmutable) {
		t.Fatalf("original name mutation accepted: %v", err)
	}
	if asset.Checksum != checksumA || asset.OriginalName != "camera-a.mov" {
		t.Fatalf("original content identity changed: %+v", asset)
	}

	replacement := validOriginal("ASSET-2", checksumB, "replacement.mov")
	relation, err := SupersedeOriginal(asset, replacement)
	if err != nil || relation.PreviousAssetID != asset.ID || relation.NewAssetID != replacement.ID {
		t.Fatalf("replacement relation was not created: relation=%+v err=%v", relation, err)
	}
}

func validOriginal(id, checksum, name string) OriginalAsset {
	return OriginalAsset{
		ID: id, Checksum: checksum, OriginalName: name, SizeBytes: 1234,
		RegisteredFromObservationID: "OBSERVATION-1", RegisteredBy: "SYSTEM-1",
		RegisteredAt: time.Date(2026, 10, 26, 9, 1, 0, 0, time.UTC),
	}
}
