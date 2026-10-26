package sqlitecatalog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const checksumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Requirement: INT-003
func TestSQLiteInputObservationIsAppendOnlyAndReloadable(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatalf("exact replay was not idempotent: %v", err)
	}
	conflicting := observation
	conflicting.RelativePath = "camera/RENAMED.MOV"
	conflicting.OriginalName = "RENAMED.MOV"
	if err := store.AppendObservation(ctx, conflicting); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("observation overwrite was not rejected: %v", err)
	}
	restored, err := store.Observation(ctx, observation.ID)
	if err != nil || !reflect.DeepEqual(restored, observation) {
		t.Fatalf("stored observation changed: restored=%+v err=%v", restored, err)
	}
}

// Requirement: INT-004
func TestSQLiteOriginalCatalogFindsChecksumDuplicateWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.FindOriginalsByChecksum(ctx, checksumA)
	if err != nil || len(candidates) != 1 || !reflect.DeepEqual(candidates[0], original) {
		t.Fatalf("checksum duplicate candidate missing: candidates=%+v err=%v", candidates, err)
	}
	mismatch := original
	mismatch.ID = "ASSET-2"
	mismatch.Checksum = checksumB
	if err := store.AppendOriginal(ctx, mismatch); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("original inconsistent with observation was accepted: %v", err)
	}
	candidates, err = store.FindOriginalsByChecksum(ctx, checksumA)
	if err != nil || len(candidates) != 1 || candidates[0].ID != "ASSET-1" {
		t.Fatalf("duplicate check overwrote catalog: candidates=%+v err=%v", candidates, err)
	}
}

// Requirement: MED-004
func TestSQLiteMediaInspectionsRemainAppendOnlyAndQueryable(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}

	first := inspectedMedia("INSPECTION-1", original.ID, checksumA, time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC))
	second := inspectedMedia("INSPECTION-2", original.ID, checksumA, first.InspectedAt.Add(time.Hour))
	if err := store.AppendInspection(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInspection(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInspection(ctx, first); err != nil {
		t.Fatalf("exact replay was not idempotent: %v", err)
	}
	mismatch := inspectedMedia("INSPECTION-3", original.ID, checksumB, second.InspectedAt.Add(time.Hour))
	if err := store.AppendInspection(ctx, mismatch); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("inspection for different bytes was accepted: %v", err)
	}

	inspections, err := store.InspectionsByOriginal(ctx, original.ID)
	if err != nil || !reflect.DeepEqual(inspections, []mediaanalysis.MediaInspection{first, second}) {
		t.Fatalf("append-only inspections were not restored in order: inspections=%+v err=%v", inspections, err)
	}
}

func openCatalog(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func observedFile(id, checksum, relativePath string) intake.InputFileObservation {
	return intake.InputFileObservation{
		ID: id, StorageID: "SOURCE-1", WorkspaceDriveID: "DRIVE-1",
		RelativePath: relativePath, OriginalName: filepath.Base(relativePath), SizeBytes: 1234,
		Checksum: checksum, ObservedBy: "WORKER-1", ObservedAt: time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
	}
}

func registeredOriginal(t *testing.T, id string, observation intake.InputFileObservation) asset.OriginalAsset {
	t.Helper()
	original, err := asset.RegisterOriginalAsset(id, observation, "SYSTEM-1", observation.ObservedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return original
}

func inspectedMedia(id, originalID, checksum string, inspectedAt time.Time) mediaanalysis.MediaInspection {
	return mediaanalysis.MediaInspection{
		ID: id, OriginalAssetID: originalID, OriginalChecksum: checksum,
		Containers: []string{"mov"}, DurationMillis: 10_000, SizeBytes: 1234, BitRate: 1_000_000,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: "h264", Width: 1920, Height: 1080,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, PixelFormat: "yuv420p",
		}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2, ChannelLayout: "stereo"}},
		ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: inspectedAt,
	}
}
