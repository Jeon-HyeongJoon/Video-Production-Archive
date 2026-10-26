package mediaanalysis

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// Requirement: MED-001
func TestMediaInspectionNormalizesCompleteTechnicalMetadata(t *testing.T) {
	observed := MediaInspection{
		ID: " INSPECTION-1 ", OriginalAssetID: " ASSET-1 ",
		OriginalChecksum: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Containers:       []string{" MP4 ", "mov", "mp4"},
		DurationMillis:   3_603_500, SizeBytes: 12_345_678, BitRate: 8_000_000,
		VideoStreams: []VideoStream{
			{Index: 2, Codec: "H264", Width: 1280, Height: 720, FrameRate: Rational{Numerator: 30, Denominator: 1}},
			{Index: 0, Codec: "HEVC", Width: 3840, Height: 2160, FrameRate: Rational{Numerator: 60000, Denominator: 2002}, PixelFormat: "YUV420P10LE"},
		},
		AudioStreams:     []AudioStream{{Index: 1, Codec: "AAC", SampleRateHz: 48_000, Channels: 2, ChannelLayout: "STEREO"}},
		ProcessorVersion: " ffprobe-8.0 ", ObservedBy: " worker-1 ",
		InspectedAt: time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC),
	}

	inspection, err := NewMediaInspection(observed)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.ID != "INSPECTION-1" || inspection.OriginalAssetID != "ASSET-1" ||
		inspection.OriginalChecksum != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		!reflect.DeepEqual(inspection.Containers, []string{"mov", "mp4"}) ||
		inspection.VideoStreams[0].Index != 0 || inspection.VideoStreams[0].Codec != "hevc" ||
		inspection.VideoStreams[0].FrameRate != (Rational{Numerator: 30000, Denominator: 1001}) ||
		inspection.AudioStreams[0].Codec != "aac" || inspection.AudioStreams[0].ChannelLayout != "stereo" {
		t.Fatalf("inspection was not normalized: %+v", inspection)
	}

	invalid := []MediaInspection{
		func() MediaInspection { value := observed; value.OriginalChecksum = ""; return value }(),
		func() MediaInspection { value := observed; value.VideoStreams = nil; return value }(),
		func() MediaInspection {
			value := observed
			value.VideoStreams = []VideoStream{{Index: 0, Codec: "h264", Width: 1920, Height: 1080}}
			return value
		}(),
	}
	for _, candidate := range invalid {
		if _, err := NewMediaInspection(candidate); !errors.Is(err, ErrInvalidInspection) {
			t.Fatalf("incomplete inspection accepted: %+v err=%v", candidate, err)
		}
	}
}
