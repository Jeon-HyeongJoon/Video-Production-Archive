package transcription

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

const transcriptChecksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Requirement: TRN-001
func TestTranscriptKeepsTimedSegmentsAndVersionedSTTProvenance(t *testing.T) {
	profile, err := NewProfile(Profile{
		ID: " event-ko ", Version: 1, ModelID: " GGML-Large-V3-Turbo-Q5_0 ", Language: " KO ", ThreadCount: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := NewContent(" KO ", []Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Text: " 첫 번째 문장 "},
		{Sequence: 2, StartMillis: 1_400, EndMillis: 2_800, Text: "두 번째 문장"},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := archive.Artifact{
		ID: "TRANSCRIPT-1", Path: "transcript/event.json", Size: 321, Checksum: transcriptChecksum,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{"AUDIO-1"}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "whisper.cpp-b6250", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}
	transcript, err := NewTranscript(artifact, profile, content)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "event-ko" || profile.ModelID != "ggml-large-v3-turbo-q5_0" || profile.Language != "ko" ||
		transcript.ProfileID != "event-ko" || transcript.ProfileVersion != 1 || transcript.Content.Language != "ko" ||
		!reflect.DeepEqual(transcript.Content.Segments, []Segment{
			{Sequence: 1, StartMillis: 0, EndMillis: 1_250, Text: "첫 번째 문장"},
			{Sequence: 2, StartMillis: 1_400, EndMillis: 2_800, Text: "두 번째 문장"},
		}) {
		t.Fatalf("transcript was not normalized with stable provenance: %+v", transcript)
	}
	_, err = NewContent("ko", []Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 1_500, Text: "first"},
		{Sequence: 2, StartMillis: 1_400, EndMillis: 2_000, Text: "overlap"},
	})
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("overlapping transcript segments were accepted: %v", err)
	}
}

// Requirement: TRN-007
func TestTranscriptQualityAssessmentRecommendsReviewFromDeterministicTimingAndTextMetrics(t *testing.T) {
	good, err := NewContent("ko", []Segment{
		{Sequence: 1, StartMillis: 0, EndMillis: 2_500, Text: "오늘 행사를 시작합니다"},
		{Sequence: 2, StartMillis: 2_700, EndMillis: 5_000, Text: "첫 번째 순서를 소개합니다"},
	})
	if err != nil {
		t.Fatal(err)
	}
	passed, err := AssessQuality(good)
	if err != nil || passed.State != QualityPassed || len(passed.Reasons) != 0 || passed.SegmentCount != 2 || passed.TextCharacterCount != 21 {
		t.Fatalf("ordinary transcript did not pass deterministic quality assessment: %+v err=%v", passed, err)
	}

	suspicious, err := NewContent("ko", []Segment{{
		Sequence: 1, StartMillis: 0, EndMillis: 19_120, Text: "이 시각 세계였습니다.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := AssessQuality(suspicious)
	if err != nil || review.State != QualityReviewRecommended || review.DurationMillis != 19_120 ||
		review.SegmentCount != 1 || review.TextCharacterCount != 9 ||
		!reflect.DeepEqual(review.Reasons, []QualityReason{QualityReasonSegmentTooLong, QualityReasonLowTextDensity}) {
		t.Fatalf("suspicious transcript did not preserve review reasons and metrics: %+v err=%v", review, err)
	}
}
