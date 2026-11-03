package archivebundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-008
func TestPublisherStoresOneResultPerRecoverablePackageAndAddsStageTwoAsNewRevision(t *testing.T) {
	publisher, sourceRoot, archiveRoot, originalChecksum, sourceDirectory, archiveDirectory := publisherFixture(t)
	createdAt := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	initial, err := publisher.PublishInitial(InitialRequest{
		PackageID: "VP-20261031-0001", WorkspaceDriveID: "SOURCE-DRIVE-1", DecisionID: "DECISION-1",
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1",
		Sources: []SourceReference{{
			OriginalAssetID: "ASSET-1", OriginalName: "event.mov", RelativePath: "recordings/event.mov",
			Checksum: originalChecksum, DurationMillis: 12_000,
		}},
		Video: FileSource{Root: RootSource, Path: "recordings/event.mov", Checksum: originalChecksum}, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.State != archive.PackageStored || initial.Revision != 1 || initial.VideoPath != "packages/VP-20261031-0001/revisions/000001/video.mp4" ||
		initial.TranscriptPath != "" || initial.TranscriptTextPath != "" || initial.SubtitlePath != "" {
		t.Fatalf("initial package is not a stored video-only revision: %+v", initial)
	}
	for _, required := range []string{initial.VideoPath, initial.MetadataPath, initial.FilesPath, initial.EventsPath, initial.ManifestPath, initial.ReadmePath} {
		if _, _, err := archiveRoot.FileSHA256(required); err != nil {
			t.Fatalf("required package file %s is unavailable: %v", required, err)
		}
	}
	metadata, err := archiveRoot.ReadFile(initial.MetadataPath)
	if err != nil || bytes.Contains(metadata, []byte(sourceDirectory)) || bytes.Contains(metadata, []byte(archiveDirectory)) ||
		!bytes.Contains(metadata, []byte(`"relative_path": "recordings/event.mov"`)) {
		t.Fatalf("metadata leaked a root or lost source tracking: %s err=%v", metadata, err)
	}
	unchanged, err := sourceRoot.ReadFile("recordings/event.mov")
	if err != nil || digest(unchanged) != originalChecksum {
		t.Fatalf("source original changed: checksum=%s err=%v", digest(unchanged), err)
	}

	revised, err := publisher.Revise(RevisionRequest{
		PackageID:      "VP-20261031-0001",
		Transcript:     FileSource{Root: RootArchive, Path: "staging/transcript.json", Checksum: fixtureDigest(`{"segments":[{"text":"행사"}]}`)},
		TranscriptText: FileSource{Root: RootArchive, Path: "staging/transcript.txt", Checksum: fixtureDigest("행사\n")},
		Subtitle:       FileSource{Root: RootArchive, Path: "staging/subtitle.srt", Checksum: fixtureDigest("1\n00:00:00,000 --> 00:00:01,000\n행사\n")},
		CreatedAt:      createdAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.ManifestDigest == initial.ManifestDigest || revised.TranscriptPath == "" ||
		revised.TranscriptTextPath == "" || revised.SubtitlePath == "" {
		t.Fatalf("stage-two files did not create a new manifest revision: initial=%+v revised=%+v", initial, revised)
	}
	if _, err := archiveRoot.ReadFile("packages/VP-20261031-0001/revisions/000001/transcript.json"); err == nil {
		t.Fatal("stage-two revision mutated the stored first revision")
	}
}

// Requirement: ARC-011
func TestCommittedPackageRejectsFurtherRevisionAndKeepsPriorRevisionsImmutable(t *testing.T) {
	publisher, _, archiveRoot, originalChecksum, _, _ := publisherFixture(t)
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	first, err := publisher.PublishInitial(InitialRequest{
		PackageID: "VP-20261031-0002", WorkspaceDriveID: "SOURCE-DRIVE-1", DecisionID: "DECISION-2",
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1",
		Sources: []SourceReference{{OriginalAssetID: "ASSET-2", OriginalName: "event.mov", RelativePath: "recordings/event.mov", Checksum: originalChecksum}},
		Video:   FileSource{Root: RootSource, Path: "recordings/event.mov", Checksum: originalChecksum}, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Revise(RevisionRequest{
		PackageID:      "VP-20261031-0002",
		Transcript:     FileSource{Root: RootArchive, Path: "staging/transcript.json", Checksum: fixtureDigest(`{"segments":[{"text":"행사"}]}`)},
		TranscriptText: FileSource{Root: RootArchive, Path: "staging/transcript.txt", Checksum: fixtureDigest("행사\n")},
		Subtitle:       FileSource{Root: RootArchive, Path: "staging/subtitle.srt", Checksum: fixtureDigest("1\n00:00:00,000 --> 00:00:01,000\n행사\n")},
		CreatedAt:      now.Add(time.Hour),
	})
	if err != nil || second.Revision != 2 || first.ManifestDigest == second.ManifestDigest {
		t.Fatalf("immutable second revision was not created: first=%+v second=%+v err=%v", first, second, err)
	}
	evidence := commitEvidence(second, now.Add(2*time.Hour))
	committed, err := publisher.Commit(second.PackageID, evidence)
	if err != nil || committed.State != archive.PackageCommitted {
		t.Fatalf("valid commit evidence did not commit current revision: package=%+v err=%v", committed, err)
	}
	_, err = publisher.Revise(RevisionRequest{
		PackageID:      second.PackageID,
		Transcript:     FileSource{Root: RootArchive, Path: "staging/transcript.json", Checksum: fixtureDigest(`{"segments":[{"text":"행사"}]}`)},
		TranscriptText: FileSource{Root: RootArchive, Path: "staging/transcript.txt", Checksum: fixtureDigest("행사\n")},
		Subtitle:       FileSource{Root: RootArchive, Path: "staging/subtitle.srt", Checksum: fixtureDigest("1\n00:00:00,000 --> 00:00:01,000\n행사\n")},
		CreatedAt:      now.Add(3 * time.Hour),
	})
	if !errors.Is(err, ErrPackageState) {
		t.Fatalf("committed package accepted another revision: %v", err)
	}
	if _, _, err := archiveRoot.FileSHA256(first.VideoPath); err != nil {
		t.Fatalf("first revision was lost after commit: %v", err)
	}
}

func publisherFixture(t *testing.T) (*Publisher, *archive.StorageRoot, *archive.StorageRoot, string, string, string) {
	t.Helper()
	sourceDirectory := t.TempDir()
	archiveDirectory := t.TempDir()
	write := func(root, relative, payload string) string {
		t.Helper()
		fullPath := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		return fixtureDigest(payload)
	}
	originalChecksum := write(sourceDirectory, "recordings/event.mov", "immutable-video")
	write(archiveDirectory, "staging/transcript.json", `{"segments":[{"text":"행사"}]}`)
	write(archiveDirectory, "staging/transcript.txt", "행사\n")
	write(archiveDirectory, "staging/subtitle.srt", "1\n00:00:00,000 --> 00:00:01,000\n행사\n")
	sourceRoot, err := archive.OpenStorageRoot(sourceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot, err := archive.OpenStorageRoot(archiveDirectory)
	if err != nil {
		sourceRoot.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { sourceRoot.Close(); archiveRoot.Close() })
	publisher, err := NewPublisher(sourceRoot, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	return publisher, sourceRoot, archiveRoot, originalChecksum, sourceDirectory, archiveDirectory
}

func commitEvidence(value Package, now time.Time) archive.CommitEvidence {
	required := archive.RequiredPackageSidecars()
	return archive.CommitEvidence{
		PackageID: value.PackageID, PackageRevision: value.Revision, ManifestDigest: value.ManifestDigest,
		QC: archive.QCVerification{ID: "QC-1", PackageID: value.PackageID, PackageRevision: value.Revision, Outcome: archive.VerificationPassed, VerifiedAt: now},
		Approval: approval.Record{
			ID: "APPROVAL-1", Sequence: 1, ActorType: vocabulary.ActorHuman, ActorID: "OPERATOR-1",
			TargetID: value.PackageID, TargetRevision: value.Revision, TargetDigest: value.ManifestDigest,
			Decision: vocabulary.ApprovalApproved, Reason: "verified", Basis: []string{"QC-1"}, OccurredAt: now,
		},
		Sidecars: archive.SidecarVerification{
			ID: "SIDECARS-1", PackageID: value.PackageID, PackageRevision: value.Revision,
			Required: append([]string(nil), required...), Present: append([]string(nil), required...), VerifiedAt: now,
		},
		Manifest: archive.ManifestVerification{
			ID: "MANIFEST-1", PackageID: value.PackageID, PackageRevision: value.Revision,
			ManifestDigest: value.ManifestDigest, VerifiedAt: now,
		},
		Copies: []archive.ArchiveCopyEvidence{
			{ID: "COPY-1", StorageID: "ARCHIVE-PRIMARY", PhysicalDeviceID: "DISK-1", ManifestDigest: value.ManifestDigest, VerifiedAt: now},
			{ID: "COPY-2", StorageID: "ARCHIVE-MIRROR", PhysicalDeviceID: "DISK-2", ManifestDigest: value.ManifestDigest, VerifiedAt: now},
		},
	}
}

func fixtureDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
