package subtitle

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: SUB-001
func TestSubtitleKeepsTranscriptTimecodesAndVersionedSRTProvenance(t *testing.T) {
	transcript := validTranscript(t)
	profile, err := NewProfile(Profile{ID: " event-srt ", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	artifact := archive.Artifact{
		ID: "SUBTITLE-1", Path: "subtitle/event.srt", Size: 96, Checksum: checksumA,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"TRANSCRIPT-1"}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "archive-subtitle-v1", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}
	result, err := NewSubtitle(artifact, profile, transcript)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := RenderSRT(result.Segments)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:00,000 --> 00:00:01,250\n첫 번째 문장\n\n2\n00:00:01,400 --> 00:00:02,800\n두 번째 문장\n\n"
	if profile.ID != "event-srt" || result.TranscriptID != "TRANSCRIPT-1" || result.Language != "ko" ||
		string(payload) != want || !reflect.DeepEqual(result.Segments, transcript.Content.Segments) {
		t.Fatalf("subtitle did not preserve normalized transcript cues: result=%+v payload=%q", result, payload)
	}
	artifact.Origin.InputArtifactIDs = []string{"OTHER-TRANSCRIPT"}
	if _, err := NewSubtitle(artifact, profile, transcript); !errors.Is(err, ErrInvalidSubtitle) {
		t.Fatalf("mismatched transcript provenance was accepted: %v", err)
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
	content, err := transcription.NewContent("ko", []transcription.Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Text: "첫 번째 문장"},
		{Sequence: 2, StartMillis: 1_400, EndMillis: 2_800, Text: "두 번째 문장"},
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := transcription.NewTranscript(archive.Artifact{
		ID: "TRANSCRIPT-1", Path: "transcript/event.json", Size: 321, Checksum: checksumA,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"AUDIO-1"}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "whisper.cpp-b6250", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}, profile, content)
	if err != nil {
		t.Fatal(err)
	}
	return transcript
}
