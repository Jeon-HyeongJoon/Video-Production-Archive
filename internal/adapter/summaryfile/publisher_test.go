package summaryfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: SUM-002
func TestSummaryPublisherVerifiesHumanFallbackJSONInsideStorageRootBeforePublishing(t *testing.T) {
	directory := t.TempDir()
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := NewPublisher(root, "videoarchive-summary-v1")
	if err != nil {
		t.Fatal(err)
	}
	transcript := validTranscript(t)
	profile := summary.Profile{
		ID: "event-summary-ko", Version: 1, Language: "ko",
		MaxKeywords: summary.KeywordLimit, MaxChapters: summary.ChapterLimit,
	}
	result, err := publisher.Publish(Request{
		SummaryID: "SUMMARY-1", Revision: 1, ArtifactID: "SUMMARY-ARTIFACT-1", Transcript: transcript, Profile: profile,
		AuthorType: vocabulary.ActorHuman, AuthorID: "operator-1",
		Content: summary.Content{
			Text: " 사람이 확인할 행사 요약 ", Keywords: []string{"행사", "기록"},
			Chapters: []summary.Chapter{{Sequence: 1, StartMillis: 0, EndMillis: 2_800, Title: "전체 행사"}},
		},
		TemporaryPath: "summary/event-r1.json.partial", PublishedPath: "summary/event-r1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, "summary", "event-r1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document summaryDocument
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if document.SchemaVersion != 1 || document.SummaryID != "SUMMARY-1" || document.Revision != 1 ||
		document.TranscriptID != "TRANSCRIPT-1" || document.AuthorType != vocabulary.ActorHuman || document.AuthorID != "operator-1" ||
		!reflect.DeepEqual(document.Content.Keywords, []string{"기록", "행사"}) ||
		result.Artifact.Checksum != hex.EncodeToString(digest[:]) || result.Artifact.Size != int64(len(payload)) ||
		result.Artifact.Origin.ProcessingKind != vocabulary.ProcessingSummary || result.Artifact.Origin.InputArtifactIDs[0] != "TRANSCRIPT-1" ||
		result.Artifact.Origin.ProfileVersion != "event-summary-ko@v1" || result.AuthorType != vocabulary.ActorHuman {
		t.Fatalf("published summary lacks verified structured content or provenance: result=%+v document=%+v", result, document)
	}
	if _, err := os.Stat(filepath.Join(directory, "summary", "event-r1.json.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary summary remained after publish: %v", err)
	}
	if _, err := publisher.Publish(Request{
		SummaryID: "SUMMARY-2", Revision: 1, ArtifactID: "SUMMARY-ARTIFACT-2", Transcript: transcript, Profile: profile,
		AuthorType: vocabulary.ActorHuman, AuthorID: "operator-1", Content: result.Content,
		TemporaryPath: "../escape.json.partial", PublishedPath: "../escape.json",
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
	content, err := transcription.NewContent("ko", []transcription.Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Text: "첫 번째 원문"},
		{Sequence: 2, StartMillis: 1_400, EndMillis: 2_800, Text: "두 번째 원문"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := transcription.NewTranscript(archive.Artifact{
		ID: "TRANSCRIPT-1", Path: "transcript/event.json", Size: 321, Checksum: checksumA,
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
