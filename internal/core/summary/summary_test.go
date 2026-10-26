package summary

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

// Requirement: SUM-001
func TestSummaryKeepsRevisionAuthorKeywordsAndTimedChaptersSeparateFromTranscript(t *testing.T) {
	transcript := validTranscript(t)
	profile, err := NewProfile(Profile{
		ID: " event-summary-ko ", Version: 1, Language: " KO ", MaxKeywords: KeywordLimit, MaxChapters: ChapterLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := archive.Artifact{
		ID: "SUMMARY-ARTIFACT-2", Path: "summary/event-r2.json", Size: 512, Checksum: checksumA,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"TRANSCRIPT-1"}, ProcessingKind: vocabulary.ProcessingSummary,
			ProcessorVersion: "provider-model-2026-10", ProfileVersion: profile.Reference(), CreatedBy: "AGENT-1", CreatedAt: time.Now().UTC(),
		},
	}
	result, err := NewSummary("SUMMARY-1", 2, artifact, profile, transcript, vocabulary.ActorAgent, "AGENT-1", Content{
		Text: " 행사 핵심 내용을 요약했습니다. ", Keywords: []string{"행사", " 기록 ", "행사"},
		Chapters: []Chapter{
			{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Title: " 시작 "},
			{Sequence: 2, StartMillis: 1_400, EndMillis: 2_800, Title: "마무리"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "SUMMARY-1" || result.Revision != 2 || result.TranscriptID != "TRANSCRIPT-1" ||
		result.TranscriptDurationMillis != 2_800 || result.AuthorType != vocabulary.ActorAgent || result.AuthorID != "AGENT-1" ||
		result.Content.Text != "행사 핵심 내용을 요약했습니다." || !reflect.DeepEqual(result.Content.Keywords, []string{"기록", "행사"}) ||
		result.Content.Chapters[0].Title != "시작" {
		t.Fatalf("summary did not preserve its revision and structured content: %+v", result)
	}
	badContent := result.Content
	badContent.Chapters = append([]Chapter(nil), badContent.Chapters...)
	badContent.Chapters[1].StartMillis = 1_000
	if _, err := NewSummary("SUMMARY-1", 3, artifact, profile, transcript, vocabulary.ActorAgent, "AGENT-1", badContent); !errors.Is(err, ErrInvalidSummary) {
		t.Fatalf("overlapping chapter was accepted: %v", err)
	}
	if transcript.Content.Segments[0].Text != "첫 번째 원문" {
		t.Fatal("summary creation changed transcript text")
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
