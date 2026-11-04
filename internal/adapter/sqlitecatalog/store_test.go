package sqlitecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/contentreview"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const checksumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const checksumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Requirement: INT-003
func TestSQLiteInputObservationIsAppendOnlyAndReloadable(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatalf("exact replay was not idempotent: %v", err)
	}
	conflicting := observation
	conflicting.RelativePath = "camera/RENAMED.MOV"
	conflicting.OriginalName = "RENAMED.MOV"
	if err := store.AppendObservation(ctx, conflicting); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("observation overwrite was not rejected: %v", err)
	}
	restored, err := store.Observation(ctx, observation.ID)
	if err != nil || !reflect.DeepEqual(restored, observation) {
		t.Fatalf("stored observation changed: restored=%+v err=%v", restored, err)
	}
}

// Requirement: INT-004
func TestSQLiteOriginalCatalogFindsChecksumDuplicateWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.FindOriginalsByChecksum(ctx, checksumA)
	if err != nil || len(candidates) != 1 || !reflect.DeepEqual(candidates[0], original) {
		t.Fatalf("checksum duplicate candidate missing: candidates=%+v err=%v", candidates, err)
	}
	mismatch := original
	mismatch.ID = "ASSET-2"
	mismatch.Checksum = checksumB
	if err := store.AppendOriginal(ctx, mismatch); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("original inconsistent with observation was accepted: %v", err)
	}
	candidates, err = store.FindOriginalsByChecksum(ctx, checksumA)
	if err != nil || len(candidates) != 1 || candidates[0].ID != "ASSET-1" {
		t.Fatalf("duplicate check overwrote catalog: candidates=%+v err=%v", candidates, err)
	}
}

// Requirement: MED-004
func TestSQLiteMediaInspectionsRemainAppendOnlyAndQueryable(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}

	first := inspectedMedia("INSPECTION-1", original.ID, checksumA, time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC))
	second := inspectedMedia("INSPECTION-2", original.ID, checksumA, first.InspectedAt.Add(time.Hour))
	if err := store.AppendInspection(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInspection(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInspection(ctx, first); err != nil {
		t.Fatalf("exact replay was not idempotent: %v", err)
	}
	mismatch := inspectedMedia("INSPECTION-3", original.ID, checksumB, second.InspectedAt.Add(time.Hour))
	if err := store.AppendInspection(ctx, mismatch); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("inspection for different bytes was accepted: %v", err)
	}

	inspections, err := store.InspectionsByOriginal(ctx, original.ID)
	if err != nil || !reflect.DeepEqual(inspections, []mediaanalysis.MediaInspection{first, second}) {
		t.Fatalf("append-only inspections were not restored in order: inspections=%+v err=%v", inspections, err)
	}
}

// Requirement: CAT-002
func TestSQLiteCatalogSearchUsesLatestDescriptionCodecAndDurationWithoutPaths(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	firstObservation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	secondObservation := observedFile("OBSERVATION-2", checksumB, "camera/CLIP002.MOV")
	for _, observation := range []intake.InputFileObservation{firstObservation, secondObservation} {
		if err := store.AppendObservation(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	firstOriginal := registeredOriginal(t, "ASSET-1", firstObservation)
	secondOriginal := registeredOriginal(t, "ASSET-2", secondObservation)
	for _, original := range []asset.OriginalAsset{firstOriginal, secondOriginal} {
		if err := store.AppendOriginal(ctx, original); err != nil {
			t.Fatal(err)
		}
	}
	oldInspection := inspectedMediaWith("INSPECTION-OLD", firstOriginal.ID, checksumA, now, "hevc", 55_000)
	latestInspection := inspectedMediaWith("INSPECTION-1", firstOriginal.ID, checksumA, now.Add(time.Minute), "h264", 60_000)
	otherInspection := inspectedMediaWith("INSPECTION-2", secondOriginal.ID, checksumB, now, "h264", 120_000)
	for _, inspection := range []mediaanalysis.MediaInspection{oldInspection, latestInspection, otherInspection} {
		if err := store.AppendInspection(ctx, inspection); err != nil {
			t.Fatal(err)
		}
	}
	records := []catalog.CatalogRecord{
		{ID: "RECORD-1", OriginalAssetID: "ASSET-1", Revision: 1, Title: "임시 제목", Description: "초기 설명", AuthorType: vocabulary.ActorHuman, AuthorID: "operator-1", RecordedAt: now},
		{ID: "RECORD-2", OriginalAssetID: "ASSET-1", Revision: 2, Title: "2026 가을 행사", Description: "전체 행사 기록 영상", Keywords: []string{"강연", "서울"}, AuthorType: vocabulary.ActorHuman, AuthorID: "operator-1", RecordedAt: now.Add(time.Minute)},
		{ID: "RECORD-3", OriginalAssetID: "ASSET-2", Revision: 1, Title: "2026 봄 행사", Description: "장시간 행사 기록", AuthorType: vocabulary.ActorArchivist, AuthorID: "archivist-1", RecordedAt: now},
	}
	for _, record := range records {
		if err := store.AppendCatalogRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	results, err := store.Search(ctx, catalog.SearchQuery{
		Text: " 가을 행사 ", VideoCodec: "H264", MinDurationMillis: 59_000, MaxDurationMillis: 61_000,
	})
	if err != nil || len(results) != 1 {
		t.Fatalf("search results=%+v err=%v", results, err)
	}
	result := results[0]
	if result.Original.ID != "ASSET-1" || result.Metadata.ID != "RECORD-2" || result.Metadata.Revision != 2 ||
		result.Metadata.Description != "전체 행사 기록 영상" || result.Inspection == nil || result.Inspection.ID != "INSPECTION-1" ||
		result.Inspection.VideoStreams[0].Codec != "h264" || result.Inspection.DurationMillis != 60_000 {
		t.Fatalf("search did not use latest searchable records: %+v", result)
	}
}

// Requirement: TRN-004
func TestSQLiteTranscriptsRemainAppendOnlyAndQueryableByOriginal(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	now := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}
	first := storedTranscript("TRANSCRIPT-1", original.ID, "첫 전사문", 1_250, now)
	orphan := storedTranscript("TRANSCRIPT-ORPHAN", "ASSET-MISSING", "참조 없는 전사문", 0, now)
	if err := store.AppendTranscript(ctx, orphan); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("transcript without a registered original was accepted: %v", err)
	}
	if err := store.AppendTranscript(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTranscript(ctx, first); err != nil {
		t.Fatalf("idempotent transcript replay failed: %v", err)
	}
	conflict := first
	conflict.Content.Segments[0].Text = "같은 ID의 다른 전사문"
	if err := store.AppendTranscript(ctx, conflict); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("conflicting transcript was not rejected: %v", err)
	}
	second := storedTranscript("TRANSCRIPT-2", original.ID, "두 번째 전사문", 2_500, now.Add(time.Minute))
	if err := store.AppendTranscript(ctx, second); err != nil {
		t.Fatal(err)
	}
	stored, err := store.TranscriptsByOriginal(ctx, original.ID)
	if err != nil || len(stored) != 2 || stored[0].Artifact.ID != first.Artifact.ID || stored[0].Content.Segments[0].Text != "첫 전사문" ||
		stored[1].Artifact.ID != second.Artifact.ID {
		t.Fatalf("stored transcripts=%+v err=%v", stored, err)
	}
}

// Requirement: CAT-003
func TestSQLiteTranscriptSearchUsesLatestSegmentsAndReturnsTimecodesWithoutPaths(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	now := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)
	observations := []intake.InputFileObservation{
		observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV"),
		observedFile("OBSERVATION-2", checksumB, "camera/CLIP002.MOV"),
	}
	for _, observation := range observations {
		if err := store.AppendObservation(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	originals := []asset.OriginalAsset{
		registeredOriginal(t, "ASSET-1", observations[0]),
		registeredOriginal(t, "ASSET-2", observations[1]),
	}
	for _, original := range originals {
		if err := store.AppendOriginal(ctx, original); err != nil {
			t.Fatal(err)
		}
	}
	transcripts := []transcription.Transcript{
		storedTranscript("TRANSCRIPT-OLD", originals[0].ID, "폐기된 가을 행사 구간", 1_000, now),
		storedTranscript("TRANSCRIPT-1", originals[0].ID, "최신 가을 행사 인사말", 12_500, now.Add(time.Minute)),
		storedTranscript("TRANSCRIPT-2", originals[1].ID, "봄 행사 인사말", 25_000, now),
	}
	for _, transcript := range transcripts {
		if err := store.AppendTranscript(ctx, transcript); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := store.SearchTranscripts(ctx, " 가을 행사 ")
	if err != nil || len(hits) != 1 {
		t.Fatalf("transcript hits=%+v err=%v", hits, err)
	}
	hit := hits[0]
	if hit.OriginalAssetID != originals[0].ID || hit.TranscriptID != "TRANSCRIPT-1" ||
		hit.Segment.Sequence != 1 || hit.Segment.StartMillis != 12_500 || hit.Segment.EndMillis != 13_500 ||
		hit.Segment.Text != "최신 가을 행사 인사말" {
		t.Fatalf("search did not return the latest timed segment: %+v", hit)
	}
	payload, err := json.Marshal(hits)
	if err != nil || strings.Contains(string(payload), "path") {
		t.Fatalf("transcript search exposed an artifact path: payload=%s err=%v", payload, err)
	}
}

// Requirement: SUM-003
func TestSQLiteSummariesRequireStoredTranscriptAndKeepImmutableRevisions(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	now := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}
	transcript := storedTranscript("TRANSCRIPT-1", original.ID, "행사 전체 전사문", 0, now)
	orphanTranscript := storedTranscript("TRANSCRIPT-MISSING", original.ID, "저장하지 않은 전사문", 0, now)
	orphan := storedSummary(t, "SUMMARY-ORPHAN", "SUMMARY-ARTIFACT-ORPHAN", 1, orphanTranscript, "고립된 요약", now)
	if err := store.AppendSummary(ctx, orphan); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("summary without a stored transcript was accepted: %v", err)
	}
	if err := store.AppendTranscript(ctx, transcript); err != nil {
		t.Fatal(err)
	}
	first := storedSummary(t, "SUMMARY-1", "SUMMARY-ARTIFACT-1", 1, transcript, "첫 요약", now.Add(time.Minute))
	second := storedSummary(t, "SUMMARY-1", "SUMMARY-ARTIFACT-2", 2, transcript, "수정 요약", now.Add(2*time.Minute))
	for _, value := range []summary.Summary{first, second, first} {
		if err := store.AppendSummary(ctx, value); err != nil {
			t.Fatalf("summary append or idempotent replay failed: %v", err)
		}
	}
	conflict := first
	conflict.Content.Text = "같은 리비전의 다른 요약"
	if err := store.AppendSummary(ctx, conflict); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("summary revision overwrite was not rejected: %v", err)
	}
	stored, err := store.SummariesByOriginal(ctx, original.ID)
	if err != nil || len(stored) != 2 || stored[0].Revision != 1 || stored[0].Content.Text != "첫 요약" ||
		stored[1].Revision != 2 || stored[1].Content.Text != "수정 요약" {
		t.Fatalf("immutable summary revisions were not restored: summaries=%+v err=%v", stored, err)
	}
}

// Requirement: REV-002
func TestSQLiteContentReviewsReferenceSummaryRevisionAndProjectLatestSequence(t *testing.T) {
	ctx := context.Background()
	store := openCatalog(t)
	now := time.Date(2026, 10, 26, 19, 0, 0, 0, time.UTC)
	observation := observedFile("OBSERVATION-1", checksumA, "camera/CLIP001.MOV")
	if err := store.AppendObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	original := registeredOriginal(t, "ASSET-1", observation)
	if err := store.AppendOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}
	transcript := storedTranscript("TRANSCRIPT-1", original.ID, "행사 전체 전사문", 0, now)
	if err := store.AppendTranscript(ctx, transcript); err != nil {
		t.Fatal(err)
	}
	value := storedSummary(t, "SUMMARY-1", "SUMMARY-ARTIFACT-1", 1, transcript, "검수할 요약", now.Add(time.Minute))
	if err := store.AppendSummary(ctx, value); err != nil {
		t.Fatal(err)
	}
	accepted := contentreview.Record{
		ID: "REVIEW-1", Sequence: 10, ReviewerID: "operator-1", ActorType: vocabulary.ActorHuman,
		TargetID: value.ID, TargetRevision: value.Revision, Decision: vocabulary.ContentAccepted,
		Reason: "전사문과 대조함", Basis: []string{value.Artifact.ID}, OccurredAt: now.Add(2 * time.Minute),
	}
	changes := accepted
	changes.ID = "REVIEW-2"
	changes.Sequence = 11
	changes.Decision = vocabulary.ContentChangesRequested
	changes.Reason = "발표자 이름 수정 필요"
	for _, review := range []contentreview.Record{accepted, changes, accepted} {
		if err := store.AppendContentReview(ctx, review); err != nil {
			t.Fatalf("content review append or idempotent replay failed: %v", err)
		}
	}
	orphan := accepted
	orphan.ID = "REVIEW-ORPHAN"
	orphan.Sequence = 12
	orphan.TargetRevision = 99
	if err := store.AppendContentReview(ctx, orphan); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("review without a summary revision was accepted: %v", err)
	}
	duplicateSequence := accepted
	duplicateSequence.ID = "REVIEW-DUPLICATE-SEQUENCE"
	duplicateSequence.Reason = "다른 기록"
	if err := store.AppendContentReview(ctx, duplicateSequence); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("duplicate event sequence was accepted: %v", err)
	}
	reviews, err := store.ContentReviews(ctx, value.ID, value.Revision)
	if err != nil || len(reviews) != 2 || reviews[0].ID != accepted.ID || reviews[1].ID != changes.ID {
		t.Fatalf("immutable review history was not restored: reviews=%+v err=%v", reviews, err)
	}
	latest, err := store.CurrentContentReview(ctx, value.ID, value.Revision)
	if err != nil || latest.ID != changes.ID || latest.Decision != vocabulary.ContentChangesRequested {
		t.Fatalf("latest persisted review projection is wrong: latest=%+v err=%v", latest, err)
	}
}

func openCatalog(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func observedFile(id, checksum, relativePath string) intake.InputFileObservation {
	return intake.InputFileObservation{
		ID: id, StorageID: "SOURCE-1", WorkspaceDriveID: "DRIVE-1",
		RelativePath: relativePath, OriginalName: filepath.Base(relativePath), SizeBytes: 1234,
		Checksum: checksum, ObservedBy: "WORKER-1", ObservedAt: time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
	}
}

func registeredOriginal(t *testing.T, id string, observation intake.InputFileObservation) asset.OriginalAsset {
	t.Helper()
	original, err := asset.RegisterOriginalAsset(id, observation, "SYSTEM-1", observation.ObservedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return original
}

func inspectedMedia(id, originalID, checksum string, inspectedAt time.Time) mediaanalysis.MediaInspection {
	return inspectedMediaWith(id, originalID, checksum, inspectedAt, "h264", 10_000)
}

func inspectedMediaWith(id, originalID, checksum string, inspectedAt time.Time, videoCodec string, durationMillis int64) mediaanalysis.MediaInspection {
	return mediaanalysis.MediaInspection{
		ID: id, OriginalAssetID: originalID, OriginalChecksum: checksum,
		Containers: []string{"mov"}, DurationMillis: durationMillis, SizeBytes: 1234, BitRate: 1_000_000,
		VideoStreams: []mediaanalysis.VideoStream{{
			Index: 0, Codec: videoCodec, Width: 1920, Height: 1080,
			FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, PixelFormat: "yuv420p",
		}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2, ChannelLayout: "stereo"}},
		ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: inspectedAt,
	}
}

func storedTranscript(id, originalID, text string, startMillis int64, createdAt time.Time) transcription.Transcript {
	return transcription.Transcript{
		Artifact: archive.Artifact{
			ID: id, Path: "transcript/" + id + ".json", Size: int64(len(text)), Checksum: checksumA,
			Origin: archive.Provenance{
				OriginalAssetID: originalID, InputArtifactIDs: []string{"AUDIO-" + originalID}, ProcessingKind: vocabulary.ProcessingSTT,
				ProcessorVersion: "whisper.cpp-b6250", ProfileVersion: "event-ko@v1", CreatedBy: "WORKER-1", CreatedAt: createdAt,
			},
		},
		ProfileID: "event-ko", ProfileVersion: 1,
		Content: transcription.Content{
			Language: "ko",
			Segments: []transcription.Segment{{Sequence: 1, StartMillis: startMillis, EndMillis: startMillis + 1_000, Text: text}},
		},
	}
}

func storedSummary(t *testing.T, id, artifactID string, revision uint64, transcript transcription.Transcript, text string, createdAt time.Time) summary.Summary {
	t.Helper()
	profile := summary.Profile{
		ID: "event-summary-ko", Version: 1, Language: "ko",
		MaxKeywords: summary.KeywordLimit, MaxChapters: summary.ChapterLimit,
	}
	result, err := summary.NewSummary(id, revision, archive.Artifact{
		ID: artifactID, Path: "summary/" + artifactID + ".json", Size: int64(len(text)), Checksum: checksumB,
		Origin: archive.Provenance{
			OriginalAssetID: transcript.Artifact.Origin.OriginalAssetID, InputArtifactIDs: []string{transcript.Artifact.ID},
			ProcessingKind: vocabulary.ProcessingSummary, ProcessorVersion: "archive-summary-v1",
			ProfileVersion: profile.Reference(), CreatedBy: "ARCHIVIST-1", CreatedAt: createdAt,
		},
	}, profile, transcript, vocabulary.ActorArchivist, "ARCHIVIST-1", summary.Content{
		Text: text, Keywords: []string{"행사", "기록"},
		Chapters: []summary.Chapter{{
			Sequence: 1, StartMillis: transcript.Content.Segments[0].StartMillis,
			EndMillis: transcript.Content.Segments[len(transcript.Content.Segments)-1].EndMillis, Title: "전체 행사",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
