package qc

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: QCT-001
func TestIntegratedQCOnlyPassesCompleteMatchingRenditionEvidence(t *testing.T) {
	payload := []byte("verified-rendered-video")
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	profile := rendering.Profile{
		ID: "archive-event-1080p", Version: 1,
		Container: rendering.ContainerMP4, VideoEncoder: rendering.VideoEncoderX264,
		AudioEncoder: rendering.AudioEncoderAAC, PixelFormat: rendering.PixelFormatYUV420P,
		MaxWidth: 1920, MaxHeight: 1080, ConstantRateFactor: 20, AudioBitRateKbps: 192, ThreadCount: 4,
	}
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	artifact := archive.Artifact{
		ID: "RENDITION-1", Path: "proxy/clip.mp4", Size: int64(len(payload)), Checksum: checksum,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
			ProcessorVersion: "ffmpeg-8.0", ProfileVersion: "archive-event-1080p@v1", CreatedBy: "WORKER-1", CreatedAt: now,
		},
	}
	rendition, err := rendering.NewRendition(artifact, profile)
	if err != nil {
		t.Fatal(err)
	}
	source, err := mediaanalysis.NewMediaInspection(mediaanalysis.MediaInspection{
		ID: "INSPECTION-1", OriginalAssetID: "ASSET-1", OriginalChecksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Containers: []string{"mov"}, DurationMillis: 60_000, SizeBytes: 10_000, BitRate: 8_000_000,
		VideoStreams:     []mediaanalysis.VideoStream{{Index: 0, Codec: "h264", Width: 3840, Height: 2160, FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}}},
		AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
		ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	output := OutputObservation{
		Checksum: checksum, SizeBytes: int64(len(payload)), Containers: []string{"MP4", "mov"}, DurationMillis: 60_040,
		VideoCodec: "H264", AudioCodec: "AAC", Width: 1920, Height: 1080, Decodable: true,
	}
	report, err := Evaluate("QC-1", rendition, profile, source, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now.Add(time.Minute))
	if err != nil || report.Outcome != Passed || len(report.Checks) != 8 {
		t.Fatalf("matching rendition did not pass QC: report=%+v err=%v", report, err)
	}

	silentSource := source
	silentSource.AudioStreams = nil
	silentOutput := output
	silentOutput.AudioCodec = ""
	silentReport, err := Evaluate("QC-SILENT", rendition, profile, silentSource, silentOutput, Policy{DurationToleranceMillis: 100}, "WORKER-1", now.Add(90*time.Second))
	if err != nil || silentReport.Outcome != Passed {
		t.Fatalf("matching silent rendition did not pass QC: report=%+v err=%v", silentReport, err)
	}
	for _, check := range silentReport.Checks {
		if check.Kind == CheckAudioCodec && (!check.Passed || check.Expected != "absent" || check.Actual != "absent") {
			t.Fatalf("silent audio evidence is incomplete: %+v", check)
		}
	}

	output.DurationMillis = 60_500
	failed, err := Evaluate("QC-2", rendition, profile, source, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now.Add(2*time.Minute))
	if err != nil || failed.Outcome != Failed {
		t.Fatalf("duration mismatch did not fail QC: report=%+v err=%v", failed, err)
	}
	for _, check := range failed.Checks {
		if check.Kind == CheckDuration {
			if check.Passed || check.Expected == "" || check.Actual == "" {
				t.Fatalf("duration failure lacks evidence: %+v", check)
			}
			return
		}
	}
	t.Fatal("duration check was not recorded")
}

// Requirement: QCT-003
func TestSourceTimelineQCUsesClipSumInsteadOfAnySingleOriginalDuration(t *testing.T) {
	payload := []byte("verified-source-sequence")
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	profile, err := rendering.OrganizationProfile(20_000)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
	timeline := organization.SourceTimeline{
		ID: "TIMELINE-1", Revision: 1, DecisionID: "DECISION-1",
		Clips: []organization.SourceClip{
			{Sequence: 1, OriginalAssetID: "ASSET-1", InMillis: 0, OutMillis: 10_000},
			{Sequence: 2, OriginalAssetID: "ASSET-2", InMillis: 0, OutMillis: 12_000},
			{Sequence: 3, OriginalAssetID: "ASSET-3", InMillis: 0, OutMillis: 8_000},
		},
		Markers: []organization.CutMarker{
			{BeforeClipID: "ASSET-1", AfterClipID: "ASSET-2", AtMillis: 10_000},
			{BeforeClipID: "ASSET-2", AfterClipID: "ASSET-3", AtMillis: 22_000},
		},
	}
	artifact := archive.Artifact{
		ID: "RENDITION-SEQUENCE", Path: "organized/SOURCE-1/video.mp4", Size: int64(len(payload)), Checksum: checksum,
		Origin: archive.Provenance{
			RootOriginalAssetIDs: []string{"ASSET-1", "ASSET-2", "ASSET-3"}, InputTimelineID: timeline.ID,
			InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
			ProcessorVersion: "ffmpeg-8.0", ProfileVersion: profile.Reference(), CreatedBy: "WORKER-1", CreatedAt: now,
		},
	}
	rendition, err := rendering.NewRendition(artifact, profile)
	if err != nil {
		t.Fatal(err)
	}
	output := OutputObservation{
		Checksum: checksum, SizeBytes: int64(len(payload)), Containers: []string{"mp4"}, DurationMillis: 30_040,
		VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080, Decodable: true,
	}
	passed, err := EvaluateTimeline("QC-TIMELINE-1", rendition, profile, timeline, true, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now)
	if err != nil || passed.Outcome != Passed || passed.SourceTimelineID != timeline.ID {
		t.Fatalf("timeline sum did not pass QC: report=%+v err=%v", passed, err)
	}
	output.DurationMillis = 10_000
	failed, err := EvaluateTimeline("QC-TIMELINE-2", rendition, profile, timeline, true, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now)
	if err != nil || failed.Outcome != Failed {
		t.Fatalf("single-clip duration passed timeline QC: report=%+v err=%v", failed, err)
	}
	// With mapped audio the final video ends after the last cut plus that
	// clip's video, even when the same timeline has a longer audio tail.
	durations := map[string]int64{"ASSET-1": 9000, "ASSET-2": 12000, "ASSET-3": 4000}
	output.DurationMillis, output.VideoDurationMillis = 30_040, 26_000
	withTracks, err := EvaluateTimelineWithVideoDurations("QC-TRACKS", rendition, profile, timeline, true, durations, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now)
	if err != nil || withTracks.Outcome != Passed {
		t.Fatal(withTracks, err)
	}
	output.VideoDurationMillis = 0
	missing, err := EvaluateTimelineWithVideoDurations("QC-MISSING", rendition, profile, timeline, true, durations, output, Policy{DurationToleranceMillis: 100}, "WORKER-1", now)
	if err != nil || missing.Outcome != Failed || missing.Failure() == nil || len(missing.Failure().Checks) != 1 || missing.Failure().Checks[0].Kind != CheckVideoDuration || missing.Failure().Checks[0].Actual != "unavailable" {
		t.Fatal(missing, err)
	}
}
