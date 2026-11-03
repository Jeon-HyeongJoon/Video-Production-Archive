package organizedmedia

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

func TestPublisherCreatesStableBundlesWithoutChangingOriginalsAndIncludesOnlyEligibleTextFiles(t *testing.T) {
	sourceDirectory := t.TempDir()
	workingDirectory := t.TempDir()
	writeFixture := func(root, relative string, payload []byte) string {
		t.Helper()
		fullPath := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		return hex.EncodeToString(digest[:])
	}
	originalPayload := []byte("immutable-original-video")
	originalChecksum := writeFixture(sourceDirectory, "DCIM/Event 1.mp4", originalPayload)
	writeFixture(sourceDirectory, "DCIM/Interview.mov", originalPayload)
	transcriptChecksum := writeFixture(workingDirectory, "staging/transcript.json", []byte(`{"segments":[]}`))
	subtitleChecksum := writeFixture(workingDirectory, "staging/subtitle.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\n행사\n"))
	renditionChecksum := writeFixture(workingDirectory, "staging/rendition.mp4", []byte("verified-rendered-video"))

	sourceRoot, err := archive.OpenStorageRoot(sourceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	workingRoot, err := archive.OpenStorageRoot(workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer workingRoot.Close()
	publisher, err := NewPublisher(sourceRoot, workingRoot)
	if err != nil {
		t.Fatal(err)
	}

	withText, err := publisher.Publish(Request{
		WorkspaceDriveID: "DRIVE-1", MediaID: "MEDIA-1", DecisionID: "DECISION-1",
		Disposition: vocabulary.OrganizationAsIs, TranscriptEligible: true,
		Sources: []organization.BundleSource{{
			OriginalAssetID: "ASSET-1", OriginalName: "Event 1.mp4", RelativePath: "DCIM/Event 1.mp4", Checksum: originalChecksum,
		}},
		Video:      FileSource{Storage: StorageSourceMedia, Path: "DCIM/Event 1.mp4", Checksum: originalChecksum},
		Transcript: &FileSource{Storage: StorageWorking, Path: "staging/transcript.json", Checksum: transcriptChecksum},
		Subtitle:   &FileSource{Storage: StorageWorking, Path: "staging/subtitle.srt", Checksum: subtitleChecksum},
	})
	if err != nil {
		t.Fatal(err)
	}
	if withText.VideoPath != "organized/DRIVE-1/MEDIA-1/video.mp4" ||
		withText.TranscriptPath != "organized/DRIVE-1/MEDIA-1/transcript.json" ||
		withText.SubtitlePath != "organized/DRIVE-1/MEDIA-1/subtitle.srt" ||
		withText.MetadataPath != "organized/DRIVE-1/MEDIA-1/metadata.json" {
		t.Fatalf("bundle paths are not stable: %+v", withText)
	}
	metadataPayload, err := workingRoot.ReadFile(withText.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataPayload, &metadata); err != nil || metadata["media_id"] != "MEDIA-1" ||
		metadata["workspace_drive_id"] != "DRIVE-1" || bytes.Contains(metadataPayload, []byte(sourceDirectory)) ||
		bytes.Contains(metadataPayload, []byte(workingDirectory)) {
		t.Fatalf("bundle metadata is incomplete: payload=%s err=%v", metadataPayload, err)
	}
	for bundlePath, expected := range map[string][]byte{
		withText.VideoPath: originalPayload, withText.TranscriptPath: []byte(`{"segments":[]}`),
		withText.SubtitlePath: []byte("1\n00:00:00,000 --> 00:00:01,000\n행사\n"),
	} {
		published, err := workingRoot.ReadFile(bundlePath)
		if err != nil || !bytes.Equal(published, expected) {
			t.Fatalf("bundle file mismatch at %s: payload=%q err=%v", bundlePath, published, err)
		}
	}
	unchanged, err := sourceRoot.ReadFile("DCIM/Event 1.mp4")
	if err != nil || string(unchanged) != string(originalPayload) {
		t.Fatalf("source original changed: payload=%q err=%v", unchanged, err)
	}

	withoutText, err := publisher.Publish(Request{
		WorkspaceDriveID: "DRIVE-1", MediaID: "MEDIA-2", DecisionID: "DECISION-2",
		Disposition: vocabulary.OrganizationRenderRecordings, TranscriptEligible: false,
		Sources: []organization.BundleSource{{
			OriginalAssetID: "ASSET-2", OriginalName: "Interview.mov", RelativePath: "DCIM/Interview.mov", Checksum: originalChecksum,
		}},
		Video: FileSource{Storage: StorageWorking, Path: "staging/rendition.mp4", Checksum: renditionChecksum},
	})
	if err != nil {
		t.Fatal(err)
	}
	if withoutText.TranscriptPath != "" || withoutText.SubtitlePath != "" {
		t.Fatalf("non-eligible bundle included text files: %+v", withoutText)
	}
	publishedRendition, err := workingRoot.ReadFile(withoutText.VideoPath)
	if err != nil || string(publishedRendition) != "verified-rendered-video" {
		t.Fatalf("rendition was not published into its bundle: payload=%q err=%v", publishedRendition, err)
	}
	for _, unexpected := range []string{"organized/DRIVE-1/MEDIA-2/transcript.json", "organized/DRIVE-1/MEDIA-2/subtitle.srt"} {
		if _, err := workingRoot.ReadFile(unexpected); err == nil {
			t.Fatalf("unexpected text file was published: %s", unexpected)
		}
	}
}
