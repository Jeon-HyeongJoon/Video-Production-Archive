package archivepackage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

// Planned contract: ARC-007
func TestArchivePackageRestoresSameRecordsIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source, err := OpenRepository(filepath.Join(root, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := OpenRepository(filepath.Join(root, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	packageDir := filepath.Join(root, "VP-20261026-0001")
	originalBytes := []byte("camera-original")
	proxyBytes := []byte("archive-proxy")
	writeArchiveFile(t, packageDir, "original/camera.mov", originalBytes)
	writeArchiveFile(t, packageDir, "proxy/camera.mp4", proxyBytes)
	now := time.Date(2026, 10, 26, 10, 30, 0, 0, time.UTC)
	record := Package{
		ID: "VP-20261026-0001",
		Snapshot: archive.ArchiveSnapshot{
			OriginalAssets: []archive.OriginalAsset{{
				ID: "ASSET-1", Checksum: packageChecksum(originalBytes), OriginalName: "camera.mov", SizeBytes: int64(len(originalBytes)),
				RegisteredFromObservationID: "OBSERVATION-1", RegisteredBy: "worker-1", RegisteredAt: now,
			}},
			Artifacts: []archive.Artifact{{
				ID: "ART-1", Path: "proxy/camera.mp4", Size: int64(len(proxyBytes)), Checksum: packageChecksum(proxyBytes),
				Origin: archive.Provenance{OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
					ProcessorVersion: "ffmpeg-test", ProfileVersion: "proxy-v1", CreatedBy: "worker-1", CreatedAt: now},
			}},
		},
		Relationships: []Relationship{{Type: "DERIVED_FROM", FromID: "ART-1", ToID: "ASSET-1"}},
		Events: []command.Event{{
			EventID: "EVT-1", EventType: "ArtifactPublished", CommandID: "CMD-1",
			Actor: command.Actor{Type: command.WorkerActor, ID: "worker-1"}, RequestedVia: "worker-api",
			TargetID: "ART-1", BeforeRevision: 1, AfterRevision: 2, Reason: "proxy verified",
		}},
		Files: []FileRecord{
			{Path: "original/camera.mov", Size: int64(len(originalBytes)), Checksum: packageChecksum(originalBytes)},
			{Path: "proxy/camera.mp4", Size: int64(len(proxyBytes)), Checksum: packageChecksum(proxyBytes)},
		},
		Readme: "# Archived event\n",
	}
	if err := source.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := Export(ctx, source, record.ID, packageDir); err != nil {
		t.Fatal(err)
	}
	if err := Import(ctx, destination, packageDir); err != nil {
		t.Fatal(err)
	}
	if err := Import(ctx, destination, packageDir); err != nil {
		t.Fatalf("second import was not idempotent: %v", err)
	}

	want, err := source.Load(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := destination.Load(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored package differs:\nwant=%+v\n got=%+v", want, got)
	}
	var eventCount int
	if err := destination.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM archive_events WHERE package_id = ?`, record.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != len(record.Events) {
		t.Fatalf("second import duplicated events: got=%d want=%d", eventCount, len(record.Events))
	}
}

func writeArchiveFile(t *testing.T, root, relative string, payload []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func packageChecksum(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
