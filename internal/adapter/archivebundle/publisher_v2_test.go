package archivebundle

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ARC-015
func TestCompactArchivePackagePublishesOnlyHumanFilesAndRecoveryDocument(t *testing.T) {
	publisher, _, archiveRoot, checksum, _, _ := publisherFixture(t)
	stored, err := publisher.PublishInitialCompact(compactInitialRequest("VP-20261108-FLAT", "DECISION-FLAT", checksum, []SourceReference{{
		OriginalAssetID: "ASSET-FLAT", OriginalName: "event.mov", RelativePath: "recordings/event.mov", Checksum: checksum,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	directory := path.Dir(stored.MetadataPath)
	entries, err := archiveRoot.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	videoCount := 0
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("flat package contains a nested directory: %s", entry.Name())
		}
		if strings.EqualFold(path.Ext(entry.Name()), ".mp4") && !strings.HasPrefix(entry.Name(), ".") {
			videoCount++
		}
		names[entry.Name()] = true
	}
	if directory != "2026-11-08_event__FLAT" || strings.Contains(directory, "/") || videoCount != 1 ||
		stored.LayoutVersion != archive.PackageLayoutCompactResultFolder || stored.VideoPath != path.Join(directory, "event.mp4") ||
		len(entries) != 3 || !names["event.mp4"] || !names["video-info.csv"] || !names[CompactPackageDocumentName] {
		t.Fatalf("layout v3 package contains files outside the compact contract: package=%+v entries=%v", stored, names)
	}
	for _, required := range []string{stored.VideoPath, stored.MetadataPath, stored.VideoInfoPath} {
		if _, _, err := archiveRoot.FileSHA256(required); err != nil {
			t.Fatalf("flat package sidecar %s is missing: %v", required, err)
		}
	}
	payload, err := archiveRoot.ReadFile(stored.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if json.Unmarshal(payload, &metadata) != nil || metadata.PackageID != stored.PackageID ||
		metadata.LayoutVersion != archive.PackageLayoutCompactResultFolder || stored.FilesPath != "" ||
		stored.EventsPath != "" || stored.ManifestPath != "" || stored.ReadmePath != "" {
		t.Fatalf("metadata identity does not match the result folder: %s", payload)
	}
}

// Requirement: MED-008
func TestFlatArchivePackageVideoInfoHasOneHumanReadableRowPerOriginal(t *testing.T) {
	publisher, _, archiveRoot, checksum, sourceDirectory, archiveDirectory := publisherFixture(t)
	created := time.Date(2024, 4, 5, 6, 7, 8, 0, time.UTC)
	sources := []SourceReference{
		{
			OriginalAssetID: "ASSET-VIDEO-1", OriginalName: "A001.MOV", RelativePath: "recordings/A001.MOV",
			Checksum: checksum, SourceCreatedAt: created, DurationMillis: 12_345, SizeBytes: 9_000_000,
			Container: "mov,mp4", BitRateBPS: 6_400_000, VideoCodec: "h264", VideoProfile: "High",
			Width: 1920, Height: 1080, FrameRateNumerator: 30_000, FrameRateDenominator: 1_001,
			VideoPixelFormat: "yuv420p", VideoBitRateBPS: 6_000_000, AudioCodec: "aac", AudioProfile: "LC",
			AudioBitRateBPS: 256_000, AudioSampleRateHz: 48_000, AudioChannels: 2, AudioChannelLayout: "stereo",
		},
		{
			OriginalAssetID: "ASSET-VIDEO-2", OriginalName: "A002.MOV", RelativePath: "recordings/A002.MOV",
			Checksum: strings.Repeat("b", 64), DurationMillis: 5_000, VideoCodec: "h264", Width: 1920, Height: 1080,
		},
	}
	stored, err := publisher.PublishInitialCompact(compactInitialRequest("VP-20261108-VIDEOINFO", "DECISION-VIDEO", checksum, sources))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := archiveRoot.ReadFile(stored.VideoInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(payload)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0][0] != "source_created_at" || records[0][1] != "source_name" ||
		records[1][0] != created.Format(time.RFC3339Nano) || records[1][1] != "A001.MOV" ||
		records[1][8] != "h264" || records[1][10] != "1920" || records[1][22] != "ASSET-VIDEO-1" ||
		records[2][0] != "" || records[2][1] != "A002.MOV" || records[2][22] != "ASSET-VIDEO-2" ||
		bytes.Contains(payload, []byte(sourceDirectory)) || bytes.Contains(payload, []byte(archiveDirectory)) {
		t.Fatalf("video-info.csv lost source rows, human columns, or path privacy: %q", records)
	}
}

// Requirement: ARC-016
func TestCompactArchivePackageRevisionPreservesHistoryInsideRecoveryDocument(t *testing.T) {
	publisher, _, archiveRoot, checksum, _, _ := publisherFixture(t)
	now := time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)
	request := compactInitialRequest("VP-20261108-REVISION", "DECISION-REVISION", checksum, []SourceReference{{
		OriginalAssetID: "ASSET-REVISION", OriginalName: "event.mov", RelativePath: "recordings/event.mov", Checksum: checksum,
	}})
	request.CreatedAt = now
	initial, err := publisher.PublishInitialCompact(request)
	if err != nil {
		t.Fatal(err)
	}
	directory := path.Dir(initial.MetadataPath)
	documentV1Payload, err := archiveRoot.ReadFile(initial.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var documentV1 CompactDocument
	if json.Unmarshal(documentV1Payload, &documentV1) != nil || len(documentV1.Revisions) != 1 {
		t.Fatalf("initial compact history is invalid: %s", documentV1Payload)
	}
	firstRevision, _ := json.Marshal(documentV1.Revisions[0])
	revised, err := publisher.ReviseCompact(RevisionRequest{
		PackageID:      initial.PackageID,
		Transcript:     FileSource{Root: RootArchive, Path: "staging/transcript.json", Checksum: fixtureDigest(`{"segments":[{"text":"행사"}]}`)},
		TranscriptText: FileSource{Root: RootArchive, Path: "staging/transcript.txt", Checksum: fixtureDigest("행사\n")},
		Subtitle:       FileSource{Root: RootArchive, Path: "staging/subtitle.srt", Checksum: fixtureDigest("1\n00:00:00,000 --> 00:00:01,000\n행사\n")},
		CreatedAt:      now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := publisher.Current(initial.PackageID)
	if err != nil {
		t.Fatal(err)
	}
	documentV2Payload, _ := archiveRoot.ReadFile(revised.MetadataPath)
	var documentV2 CompactDocument
	if json.Unmarshal(documentV2Payload, &documentV2) != nil || len(documentV2.Revisions) != 2 {
		t.Fatalf("revised compact history is invalid: %s", documentV2Payload)
	}
	firstRevisionAfter, _ := json.Marshal(documentV2.Revisions[0])
	transcript, transcriptErr := archiveRoot.ReadFile(path.Join(directory, "transcript.txt"))
	if revised.Revision != 2 || current.Revision != 2 || current.ManifestDigest != revised.ManifestDigest ||
		!bytes.Equal(firstRevision, firstRevisionAfter) ||
		transcriptErr != nil || string(transcript) != "행사\n" {
		t.Fatalf("revision projection moved without preserving compact history: initial=%+v revised=%+v current=%+v", initial, revised, current)
	}
	entries, err := archiveRoot.ReadDir(directory)
	if err != nil || len(entries) != 5 {
		t.Fatalf("STT revision must expose only video, video info, text, subtitle, and recovery document: entries=%v err=%v", entries, err)
	}
	if exists, _ := archiveRoot.Exists(path.Join(directory, "transcript.json")); exists {
		t.Fatal("structured transcript leaked into the human-facing result folder")
	}
}

func compactInitialRequest(packageID, decisionID, checksum string, sources []SourceReference) InitialRequest {
	return InitialRequest{
		LayoutVersion: CompactLayoutVersion, PackageID: packageID, WorkspaceDriveID: "SOURCE-DRIVE-1", DecisionID: decisionID,
		MediaFolder: "recordings", Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1",
		Sources: sources, Video: FileSource{Root: RootSource, Path: "recordings/event.mov", Checksum: checksum},
		CreatedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
	}
}
