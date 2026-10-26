package subtitlefile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/subtitle"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: SUB-002
func TestSubtitlePublisherVerifiesTemporarySRTInsideStorageRootBeforePublishing(t *testing.T) {
	directory := t.TempDir()
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := NewPublisher(root, "videoarchive-subtitle-v1", "WORKER-1")
	if err != nil {
		t.Fatal(err)
	}
	transcript := validTranscript(t)
	result, err := publisher.Publish(Request{
		SubtitleID: "SUBTITLE-1", Transcript: transcript, Profile: subtitle.Profile{ID: "event-srt", Version: 1},
		TemporaryPath: "subtitle/event.srt.partial", PublishedPath: "subtitle/event.srt",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, "subtitle", "event.srt"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if string(payload) != "1\n00:00:00,000 --> 00:00:01,250\n첫 문장\n\n" ||
		result.Artifact.Checksum != hex.EncodeToString(digest[:]) || result.Artifact.Size != int64(len(payload)) ||
		result.Artifact.Path != "subtitle/event.srt" || result.Artifact.Origin.InputArtifactIDs[0] != "TRANSCRIPT-1" ||
		result.Artifact.Origin.ProfileVersion != "event-srt@v1" || result.Artifact.Origin.CreatedBy != "WORKER-1" {
		t.Fatalf("published subtitle lacks verified bytes or provenance: result=%+v payload=%q", result, payload)
	}
	if _, err := os.Stat(filepath.Join(directory, "subtitle", "event.srt.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary subtitle remained after publish: %v", err)
	}
	if _, err := publisher.Publish(Request{
		SubtitleID: "SUBTITLE-2", Transcript: transcript, Profile: subtitle.Profile{ID: "event-srt", Version: 1},
		TemporaryPath: "../escape.srt.partial", PublishedPath: "../escape.srt",
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsafe storage path was accepted: %v", err)
	}
}

func validTranscript(t *testing.T) transcription.Transcript {
	t.Helper()
	profile, err := transcription.NewProfile(transcription.Profile{
		ID: "event-ko", Version: 1, ModelID: "ggml-large-v3-turbo-q5_0", Language: "ko", ThreadCount: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := transcription.NewContent("ko", []transcription.Segment{{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Text: "첫 문장"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := transcription.NewTranscript(archive.Artifact{
		ID: "TRANSCRIPT-1", Path: "transcript/event.json", Size: 200, Checksum: checksumA,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"AUDIO-1"}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "whisper.cpp-b6250", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}, profile, content)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
