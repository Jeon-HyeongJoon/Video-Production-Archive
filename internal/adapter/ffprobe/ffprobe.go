package ffprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/mediaanalysis"
)

var (
	ErrInvalidRequest = errors.New("invalid ffprobe inspection request")
	ErrProbeFailed    = errors.New("ffprobe execution failed")
	ErrInvalidOutput  = errors.New("ffprobe output is invalid")
)

type Runner interface {
	Output(context.Context, string, []string) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Output(ctx context.Context, executable string, arguments []string) ([]byte, error) {
	process := exec.CommandContext(ctx, executable, arguments...)
	process.Stderr = io.Discard
	output, err := process.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

type Request struct {
	InspectionID      string
	OriginalAssetID   string
	OriginalChecksum  string
	ResolvedInputPath string
}

type Inspector struct {
	executable       string
	processorVersion string
	workerID         string
	runner           Runner
	clock            func() time.Time
}

func NewInspector(executable, processorVersion, workerID string, runner Runner) (*Inspector, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" || runner == nil {
		return nil, ErrInvalidRequest
	}
	return &Inspector{
		executable: executable, processorVersion: processorVersion, workerID: workerID,
		runner: runner, clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (inspector *Inspector) Inspect(ctx context.Context, request Request) (mediaanalysis.MediaInspection, error) {
	if strings.TrimSpace(request.InspectionID) == "" || strings.TrimSpace(request.OriginalAssetID) == "" ||
		strings.TrimSpace(request.OriginalChecksum) == "" || strings.TrimSpace(request.ResolvedInputPath) == "" {
		return mediaanalysis.MediaInspection{}, ErrInvalidRequest
	}
	arguments := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-i", request.ResolvedInputPath,
	}
	payload, err := inspector.runner.Output(ctx, inspector.executable, arguments)
	if err != nil {
		return mediaanalysis.MediaInspection{}, fmt.Errorf("%w: %w", ErrProbeFailed, err)
	}
	observed, err := decode(payload)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	observed.ID = request.InspectionID
	observed.OriginalAssetID = request.OriginalAssetID
	observed.OriginalChecksum = request.OriginalChecksum
	observed.ProcessorVersion = inspector.processorVersion
	observed.ObservedBy = inspector.workerID
	observed.InspectedAt = inspector.clock()
	inspection, err := mediaanalysis.NewMediaInspection(observed)
	if err != nil {
		return mediaanalysis.MediaInspection{}, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	return inspection, nil
}

type probeDocument struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index         int    `json:"index"`
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		AverageRate   string `json:"avg_frame_rate"`
		RealRate      string `json:"r_frame_rate"`
		PixelFormat   string `json:"pix_fmt"`
		SampleRate    string `json:"sample_rate"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
	} `json:"streams"`
}

func decode(payload []byte) (mediaanalysis.MediaInspection, error) {
	var document probeDocument
	if err := json.Unmarshal(payload, &document); err != nil {
		return mediaanalysis.MediaInspection{}, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	duration, err := parseDurationMillis(document.Format.Duration)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	size, err := parseRequiredInteger(document.Format.Size)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}
	bitRate, err := parseOptionalInteger(document.Format.BitRate)
	if err != nil {
		return mediaanalysis.MediaInspection{}, err
	}

	inspection := mediaanalysis.MediaInspection{
		Containers:     strings.Split(document.Format.FormatName, ","),
		DurationMillis: duration, SizeBytes: size, BitRate: bitRate,
	}
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			frameRate, err := parseFrameRate(stream.AverageRate, stream.RealRate)
			if err != nil {
				return mediaanalysis.MediaInspection{}, err
			}
			inspection.VideoStreams = append(inspection.VideoStreams, mediaanalysis.VideoStream{
				Index: stream.Index, Codec: stream.CodecName, Width: stream.Width, Height: stream.Height,
				FrameRate: frameRate, PixelFormat: stream.PixelFormat,
			})
		case "audio":
			sampleRate, err := parseRequiredInteger(stream.SampleRate)
			if err != nil || sampleRate > int64(int(^uint(0)>>1)) {
				return mediaanalysis.MediaInspection{}, ErrInvalidOutput
			}
			inspection.AudioStreams = append(inspection.AudioStreams, mediaanalysis.AudioStream{
				Index: stream.Index, Codec: stream.CodecName, SampleRateHz: int(sampleRate),
				Channels: stream.Channels, ChannelLayout: stream.ChannelLayout,
			})
		}
	}
	return inspection, nil
}

func parseDurationMillis(value string) (int64, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 || seconds > float64(math.MaxInt64)/1000 {
		return 0, ErrInvalidOutput
	}
	return int64(math.Round(seconds * 1000)), nil
}

func parseRequiredInteger(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, ErrInvalidOutput
	}
	return parsed, nil
}

func parseOptionalInteger(value string) (int64, error) {
	if value == "" || value == "N/A" {
		return 0, nil
	}
	return parseRequiredInteger(value)
}

func parseFrameRate(preferred, fallback string) (mediaanalysis.Rational, error) {
	for _, value := range []string{preferred, fallback} {
		parts := strings.Split(value, "/")
		if len(parts) != 2 {
			continue
		}
		numerator, numeratorErr := strconv.ParseInt(parts[0], 10, 64)
		denominator, denominatorErr := strconv.ParseInt(parts[1], 10, 64)
		if numeratorErr == nil && denominatorErr == nil && numerator > 0 && denominator > 0 {
			return mediaanalysis.Rational{Numerator: numerator, Denominator: denominator}, nil
		}
	}
	return mediaanalysis.Rational{}, ErrInvalidOutput
}
