package mediaanalysis

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ErrInvalidInspection = errors.New("media inspection is incomplete or invalid")

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Rational struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

type VideoStream struct {
	Index       int      `json:"index"`
	Codec       string   `json:"codec"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	FrameRate   Rational `json:"frame_rate"`
	PixelFormat string   `json:"pixel_format,omitempty"`
}

type AudioStream struct {
	Index         int    `json:"index"`
	Codec         string `json:"codec"`
	SampleRateHz  int    `json:"sample_rate_hz"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout,omitempty"`
}

// MediaInspection is an append-only technical record bound to the immutable bytes
// of an OriginalAsset by both ID and checksum.
type MediaInspection struct {
	ID               string        `json:"id"`
	OriginalAssetID  string        `json:"original_asset_id"`
	OriginalChecksum string        `json:"original_checksum"`
	Containers       []string      `json:"containers"`
	DurationMillis   int64         `json:"duration_millis"`
	SizeBytes        int64         `json:"size_bytes"`
	BitRate          int64         `json:"bit_rate"`
	VideoStreams     []VideoStream `json:"video_streams"`
	AudioStreams     []AudioStream `json:"audio_streams"`
	ProcessorVersion string        `json:"processor_version"`
	ObservedBy       string        `json:"observed_by"`
	InspectedAt      time.Time     `json:"inspected_at"`
}

func NewMediaInspection(source MediaInspection) (MediaInspection, error) {
	inspection := source
	inspection.ID = strings.TrimSpace(source.ID)
	inspection.OriginalAssetID = strings.TrimSpace(source.OriginalAssetID)
	inspection.OriginalChecksum = strings.ToLower(strings.TrimSpace(source.OriginalChecksum))
	inspection.ProcessorVersion = strings.TrimSpace(source.ProcessorVersion)
	inspection.ObservedBy = strings.TrimSpace(source.ObservedBy)
	inspection.Containers = normalizeContainers(source.Containers)
	inspection.VideoStreams = append([]VideoStream(nil), source.VideoStreams...)
	inspection.AudioStreams = append([]AudioStream(nil), source.AudioStreams...)

	indexes := make(map[int]bool, len(inspection.VideoStreams)+len(inspection.AudioStreams))
	for index := range inspection.VideoStreams {
		stream := &inspection.VideoStreams[index]
		stream.Codec = strings.ToLower(strings.TrimSpace(stream.Codec))
		stream.PixelFormat = strings.ToLower(strings.TrimSpace(stream.PixelFormat))
		stream.FrameRate = normalizeRational(stream.FrameRate)
		if stream.Index < 0 || indexes[stream.Index] || stream.Codec == "" || stream.Width <= 0 || stream.Height <= 0 ||
			stream.FrameRate.Numerator <= 0 || stream.FrameRate.Denominator <= 0 {
			return MediaInspection{}, ErrInvalidInspection
		}
		indexes[stream.Index] = true
	}
	for index := range inspection.AudioStreams {
		stream := &inspection.AudioStreams[index]
		stream.Codec = strings.ToLower(strings.TrimSpace(stream.Codec))
		stream.ChannelLayout = strings.ToLower(strings.TrimSpace(stream.ChannelLayout))
		if stream.Index < 0 || indexes[stream.Index] || stream.Codec == "" || stream.SampleRateHz <= 0 || stream.Channels <= 0 {
			return MediaInspection{}, ErrInvalidInspection
		}
		indexes[stream.Index] = true
	}
	sort.Slice(inspection.VideoStreams, func(i, j int) bool { return inspection.VideoStreams[i].Index < inspection.VideoStreams[j].Index })
	sort.Slice(inspection.AudioStreams, func(i, j int) bool { return inspection.AudioStreams[i].Index < inspection.AudioStreams[j].Index })

	if inspection.ID == "" || inspection.OriginalAssetID == "" || !sha256Pattern.MatchString(inspection.OriginalChecksum) ||
		len(inspection.Containers) == 0 || inspection.DurationMillis <= 0 || inspection.SizeBytes <= 0 || inspection.BitRate < 0 ||
		len(inspection.VideoStreams) == 0 || inspection.ProcessorVersion == "" || inspection.ObservedBy == "" || inspection.InspectedAt.IsZero() {
		return MediaInspection{}, ErrInvalidInspection
	}
	return inspection, nil
}

func normalizeContainers(source []string) []string {
	unique := make(map[string]bool, len(source))
	for _, container := range source {
		container = strings.ToLower(strings.TrimSpace(container))
		if container != "" {
			unique[container] = true
		}
	}
	containers := make([]string, 0, len(unique))
	for container := range unique {
		containers = append(containers, container)
	}
	sort.Strings(containers)
	return containers
}

func normalizeRational(value Rational) Rational {
	if value.Denominator < 0 {
		value.Numerator = -value.Numerator
		value.Denominator = -value.Denominator
	}
	divisor := greatestCommonDivisor(value.Numerator, value.Denominator)
	if divisor > 1 {
		value.Numerator /= divisor
		value.Denominator /= divisor
	}
	return value
}

func greatestCommonDivisor(left, right int64) int64 {
	if left < 0 {
		left = -left
	}
	if right < 0 {
		right = -right
	}
	for right != 0 {
		left, right = right, left%right
	}
	return left
}
