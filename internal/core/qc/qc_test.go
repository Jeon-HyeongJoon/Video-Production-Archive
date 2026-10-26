package qc

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: QC-001
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
