package transcripttext

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: TRN-006
func TestPublisherWritesHumanReadableUTF8TextFromOrderedTranscriptSegments(t *testing.T) {
	directory := t.TempDir()
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	now := time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)
	transcript := fixtureTranscript(t, now)
	publisher, err := NewPublisher(root, "WORKER-1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := publisher.Publish(Request{
		ID: "TEXT-1", Transcript: transcript,
		TemporaryPath: "staging/transcript.txt.partial", PublishedPath: "staging/transcript.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, "staging", "transcript.txt"))
	if err != nil || string(payload) != "첫 문장\n둘째 문장\n" || artifact.Path != "staging/transcript.txt" ||
		artifact.Origin.InputArtifactIDs[0] != transcript.Artifact.ID {
		t.Fatalf("human-readable transcript text is invalid: payload=%q artifact=%+v err=%v", payload, artifact, err)
	}
}

func fixtureTranscript(t *testing.T, now time.Time) transcription.Transcript {
	t.Helper()
	profile, err := transcription.NewProfile(transcription.Profile{ID: "event-ko", Version: 1, ModelID: "ggml", Language: "ko", ThreadCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	content, err := transcription.NewContent("ko", []transcription.Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 1_000, Text: "첫 문장"},
		{Sequence: 2, StartMillis: 1_000, EndMillis: 2_000, Text: "둘째 문장"},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := transcription.NewTranscript(archive.Artifact{
		ID: "TRANSCRIPT-1", Path: "staging/transcript.json", Size: 100,
		Checksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"AUDIO-1"}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "whisper.cpp", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: now,
		},
	}, profile, content)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
