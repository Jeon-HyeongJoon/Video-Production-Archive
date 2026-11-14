package archive

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/vocabulary"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const checksumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Requirement: ARC-002
func TestDerivedArtifactRequiresCompleteVersionedProvenance(t *testing.T) {
	artifact := Artifact{
		ID: "ART-1", Path: "proxy/video.mp4", Size: 42, Checksum: checksumA,
		Origin: Provenance{OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender, ProcessorVersion: "1.2.0", ProfileVersion: "proxy-v1", CreatedBy: "worker-1", CreatedAt: time.Now()},
	}
	if err := artifact.ValidateDerived(); err != nil {
		t.Fatalf("valid provenance rejected: %v", err)
	}
	artifact.Origin.ProfileVersion = ""
	if err := artifact.ValidateDerived(); !errors.Is(err, ErrInvalidProvenance) {
		t.Fatalf("incomplete provenance accepted: %v", err)
	}
}

// Requirement: ARC-009
func TestMultiOriginalArtifactProvenanceRoundTripsTimelineAndEveryRootAsset(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	snapshot := ArchiveSnapshot{
		OriginalAssets: []OriginalAsset{
			archiveOriginal("ASSET-1", checksumA, "C0001.MP4", now),
			archiveOriginal("ASSET-2", checksumB, "C0002.MP4", now),
			archiveOriginal("ASSET-3", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "C0003.MP4", now),
		},
		Artifacts: []Artifact{{
			ID: "RENDITION-SEQUENCE", Path: "organized/SOURCE-1/video.mp4", Size: 30, Checksum: checksumA,
			Origin: Provenance{
				RootOriginalAssetIDs: []string{"ASSET-3", "ASSET-1", "ASSET-2"}, InputTimelineID: "TIMELINE-1",
				InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
				ProcessorVersion: "ffmpeg-8.0", ProfileVersion: "archive-1080p-20mbps@v1",
				CreatedBy: "WORKER-1", CreatedAt: now,
			},
		}},
	}
	payload, err := ExportSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ImportSnapshot(payload)
	if err != nil {
		t.Fatal(err)
	}
	origin := restored.Artifacts[0].Origin
	if origin.OriginalAssetID != "" || origin.InputTimelineID != "TIMELINE-1" ||
		!reflect.DeepEqual(origin.RootOriginalAssetIDs, []string{"ASSET-1", "ASSET-2", "ASSET-3"}) {
		t.Fatalf("multi-root provenance did not round trip canonically: %+v", origin)
	}
	invalidCases := []struct {
		name       string
		rootIDs    []string
		timelineID string
	}{
		{name: "representative only", rootIDs: []string{"ASSET-1"}, timelineID: "TIMELINE-1"},
		{name: "unknown root", rootIDs: []string{"ASSET-1", "ASSET-UNKNOWN"}, timelineID: "TIMELINE-1"},
		{name: "missing timeline", rootIDs: []string{"ASSET-1", "ASSET-2", "ASSET-3"}},
	}
	for _, testCase := range invalidCases {
		invalid := snapshot
		invalid.Artifacts = append([]Artifact(nil), snapshot.Artifacts...)
		invalid.Artifacts[0].Origin.RootOriginalAssetIDs = testCase.rootIDs
		invalid.Artifacts[0].Origin.InputTimelineID = testCase.timelineID
		if _, err := ExportSnapshot(invalid); !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("%s provenance accepted: %v", testCase.name, err)
		}
	}
}

// Requirement: ARC-003
func TestManifestIsDeterministicSortedAndUsesRelativePaths(t *testing.T) {
	manifest, err := BuildManifest([]ManifestEntry{{Path: "z/video.mp4", Checksum: checksumB}, {Path: "a/meta.json", Checksum: checksumA}})
	if err != nil {
		t.Fatal(err)
	}
	want := checksumA + "  a/meta.json\n" + checksumB + "  z/video.mp4\n"
	if manifest != want {
		t.Fatalf("manifest=%q want=%q", manifest, want)
	}
	if _, err := BuildManifest([]ManifestEntry{{Path: "../escape", Checksum: checksumA}}); err == nil {
		t.Fatal("path traversal accepted")
	}
	for _, absolute := range []string{"/etc/passwd", `C:\\Windows\\system.ini`, `\\\\server\\share\\file`} {
		if _, err := BuildManifest([]ManifestEntry{{Path: absolute, Checksum: checksumA}}); err == nil {
			t.Fatalf("absolute path accepted: %s", absolute)
		}
	}
	forward, err := BuildManifest([]ManifestEntry{{Path: `folder\video.mp4`, Checksum: checksumA}, {Path: "a.json", Checksum: checksumB}})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := BuildManifest([]ManifestEntry{{Path: "a.json", Checksum: checksumB}, {Path: "folder/video.mp4", Checksum: checksumA}})
	if err != nil || forward != reversed {
		t.Fatalf("manifest is not byte deterministic: forward=%q reverse=%q err=%v", forward, reversed, err)
	}
	if _, err := BuildManifest([]ManifestEntry{{Path: `folder\video.mp4`, Checksum: checksumA}, {Path: "folder/video.mp4", Checksum: checksumB}}); err == nil {
		t.Fatal("duplicate normalized path accepted")
	}
}

// Requirement: ARC-004
func TestArchiveCommitRequiresQCApprovalAndTwoPhysicalCopies(t *testing.T) {
	valid := validCommitEvidence()
	if err := ValidateCommit(valid); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	cases := []CommitEvidence{
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.QC.Outcome = VerificationFailed
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Approval.TargetRevision++
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Sidecars.Present = value.Sidecars.Present[:len(value.Sidecars.Present)-1]
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Manifest.ManifestDigest = checksumB
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Copies = value.Copies[:1]
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Copies[1].PhysicalDeviceID = "disk-a"
			return value
		}(),
		func() CommitEvidence {
			value := cloneCommitEvidence(valid)
			value.Copies[1].ManifestDigest = checksumB
			return value
		}(),
	}
	for _, evidence := range cases {
		if err := ValidateCommit(evidence); !errors.Is(err, ErrCommitPrecondition) {
			t.Fatalf("invalid evidence accepted: %+v", evidence)
		}
	}
}

// Requirement: ARC-005
func TestArchiveSnapshotRoundTripsWithoutDatabase(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	snapshot := ArchiveSnapshot{
		OriginalAssets: []OriginalAsset{archiveOriginal("ASSET-1", checksumA, "a.mov", now)},
		Artifacts: []Artifact{{
			ID: "ART-1", Path: "proxy/a.mp4", Size: 10, Checksum: checksumB,
			Origin: Provenance{OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender, ProcessorVersion: "1", ProfileVersion: "proxy-v1", CreatedBy: "worker-1", CreatedAt: now},
		}},
	}
	payload, err := ExportSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ImportSnapshot(payload)
	if err != nil || !reflect.DeepEqual(restored, snapshot) {
		t.Fatalf("round trip mismatch: restored=%+v err=%v", restored, err)
	}
}

func archiveOriginal(id, checksum, name string, registeredAt time.Time) OriginalAsset {
	return OriginalAsset{
		ID: id, Checksum: checksum, OriginalName: name, SizeBytes: 10,
		RegisteredFromObservationID: "OBSERVATION-1", RegisteredBy: "operator-1", RegisteredAt: registeredAt,
	}
}

func validCommitEvidence() CommitEvidence {
	now := time.Now()
	sidecars := RequiredPackageSidecars()
	return CommitEvidence{
		PackageID: "VP-20261021-0001", PackageRevision: 3, ManifestDigest: checksumA,
		QC: QCVerification{ID: "QC-1", PackageID: "VP-20261021-0001", PackageRevision: 3, Outcome: VerificationPassed, VerifiedAt: now},
		Approval: approval.Record{
			ID: "APR-1", Sequence: 7, ActorType: vocabulary.ActorHuman, ActorID: "operator-1",
			TargetID: "VP-20261021-0001", TargetRevision: 3, TargetDigest: checksumA,
			Decision: vocabulary.ApprovalApproved, Reason: "QC reviewed", Basis: []string{"QC-1"}, OccurredAt: now,
		},
		Sidecars: SidecarVerification{
			ID: "SIDE-1", PackageID: "VP-20261021-0001", PackageRevision: 3,
			Required: append([]string(nil), sidecars...), Present: append([]string(nil), sidecars...), VerifiedAt: now,
		},
		Manifest: ManifestVerification{ID: "MAN-1", PackageID: "VP-20261021-0001", PackageRevision: 3, ManifestDigest: checksumA, VerifiedAt: now},
		Copies: []ArchiveCopyEvidence{
			{ID: "COPY-1", StorageID: "archive-primary", PhysicalDeviceID: "disk-a", ManifestDigest: checksumA, VerifiedAt: now},
			{ID: "COPY-2", StorageID: "archive-mirror", PhysicalDeviceID: "disk-b", ManifestDigest: checksumA, VerifiedAt: now},
		},
	}
}

func cloneCommitEvidence(value CommitEvidence) CommitEvidence {
	value.Copies = append([]ArchiveCopyEvidence(nil), value.Copies...)
	value.Sidecars.Required = append([]string(nil), value.Sidecars.Required...)
	value.Sidecars.Present = append([]string(nil), value.Sidecars.Present...)
	value.Approval.Basis = append([]string(nil), value.Approval.Basis...)
	return value
}

// Requirement: ARC-006
func TestStorageRootRejectsTraversalAbsoluteUNCAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "safe", "video.mov"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	storageRoot, err := OpenStorageRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storageRoot.Close()
	file, err := storageRoot.Open("safe/video.mov")
	if err != nil {
		t.Fatalf("safe path rejected: %v", err)
	}
	file.Close()
	for _, unsafe := range []string{"../escape.mov", "/absolute.mov", `\absolute.mov`, `C:\escape.mov`, `\\server\share\file.mov`} {
		if _, err := storageRoot.Open(unsafe); !errors.Is(err, ErrUnsafeStoragePath) {
			t.Fatalf("unsafe path %q accepted: %v", unsafe, err)
		}
	}
	outside := t.TempDir()
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := storageRoot.Open("escape-link/video.mov"); !errors.Is(err, ErrUnsafeStoragePath) {
			t.Fatalf("symlink escape accepted: %v", err)
		}
	}
}
