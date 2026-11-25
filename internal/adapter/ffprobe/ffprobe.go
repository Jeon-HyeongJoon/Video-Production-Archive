package ffprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/qc"
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
	observed, _, err := inspector.probe(ctx, request.ResolvedInputPath)
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

// InspectPublishedOutput records the independently observed bytes and primary
// streams used by integrated QC. It does not reuse expected Rendition fields.
func (inspector *Inspector) InspectPublishedOutput(ctx context.Context, resolvedPath string) (qc.OutputObservation, error) {
	if strings.TrimSpace(resolvedPath) == "" {
		return qc.OutputObservation{}, ErrInvalidRequest
	}
	output, err := inspector.inspectOutputStreams(ctx, resolvedPath)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	output.Checksum = hex.EncodeToString(hash.Sum(nil))
	output.SizeBytes = size
	return output, nil
}

// InspectStorageOutput probes through the external process path derived from a
// StorageRoot, but reads and hashes the resulting bytes only through that
// capability.
func (inspector *Inspector) InspectStorageOutput(ctx context.Context, root *archive.StorageRoot, relativePath string) (qc.OutputObservation, error) {
	if root == nil {
		return qc.OutputObservation{}, ErrInvalidRequest
	}
	resolvedPath, err := root.ExternalProcessPath(relativePath)
	if err != nil {
		return qc.OutputObservation{}, ErrInvalidRequest
	}
	output, err := inspector.inspectOutputStreams(ctx, resolvedPath)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	size, checksum, err := root.FileSHA256(relativePath)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	output.SizeBytes = size
	output.Checksum = checksum
	return output, nil
}

func (inspector *Inspector) inspectOutputStreams(ctx context.Context, resolvedPath string) (qc.OutputObservation, error) {
	observed, brand, err := inspector.probe(ctx, resolvedPath)
	if err != nil {
		return qc.OutputObservation{}, err
	}
	output := qc.OutputObservation{
		Containers: qcContainers(observed.Containers, brand), DurationMillis: observed.DurationMillis, Decodable: true,
	}
	primaryVideo := int(^uint(0) >> 1)
	for _, stream := range observed.VideoStreams {
		if stream.Index < primaryVideo {
			primaryVideo = stream.Index
			output.VideoCodec = strings.ToLower(strings.TrimSpace(stream.Codec))
			output.Width = stream.Width
			output.Height = stream.Height
			output.VideoDurationMillis = stream.DurationMillis
		}
	}
	primaryAudio := int(^uint(0) >> 1)
	for _, stream := range observed.AudioStreams {
		if stream.Index < primaryAudio {
			primaryAudio = stream.Index
			output.AudioCodec = strings.ToLower(strings.TrimSpace(stream.Codec))
		}
	}
	return output, nil
}

// The MOV demuxer lists every format it supports. A recognized major brand
// identifies which one was actually written, independently of the file name.
func qcContainers(containers []string, majorBrand string) []string {
	for i, name := range containers {
		containers[i] = strings.ToLower(strings.TrimSpace(name))
	}
	if slices.Contains(containers, "mov") && slices.Contains(containers, "mp4") {
		brand := strings.ToLower(strings.TrimSpace(majorBrand))
		switch {
		case brand == "qt":
			return []string{"mov"}
		case strings.HasPrefix(brand, "3gp"):
			return []string{"3gp"}
		case strings.HasPrefix(brand, "3g2"):
			return []string{"3g2"}
		case brand == "isom" || brand == "mp41" || brand == "mp42":
			return []string{"mp4"}
		}
	}
	return containers
}

type probeDocument struct {
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Size       string            `json:"size"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Index         int    `json:"index"`
		Duration      string `json:"duration"`
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Profile       string `json:"profile"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		AverageRate   string `json:"avg_frame_rate"`
		RealRate      string `json:"r_frame_rate"`
		PixelFormat   string `json:"pix_fmt"`
		BitRate       string `json:"bit_rate"`
		SampleRate    string `json:"sample_rate"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
	} `json:"streams"`
}

func (inspector *Inspector) probe(ctx context.Context, resolvedPath string) (mediaanalysis.MediaInspection, string, error) {
	arguments := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-i", resolvedPath}
	payload, err := inspector.runner.Output(ctx, inspector.executable, arguments)
	if err != nil {
		return mediaanalysis.MediaInspection{}, "", fmt.Errorf("%w: %w", ErrProbeFailed, err)
	}
	var document probeDocument
	if err := json.Unmarshal(payload, &document); err != nil {
		return mediaanalysis.MediaInspection{}, "", fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	observed, err := document.inspection()
	return observed, document.Format.Tags["major_brand"], err
}

func (document *probeDocument) inspection() (mediaanalysis.MediaInspection, error) {
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
	if raw := strings.TrimSpace(document.Format.Tags["creation_time"]); raw != "" {
		if createdAt, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
			inspection.ContainerCreatedAt = createdAt.UTC()
		}
	}
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			var videoDuration int64
			if stream.Duration != "" && stream.Duration != "N/A" {
				// A zero duration, like a missing duration, supplies no track measurement.
				videoDuration, err = parseMillis(stream.Duration)
				if err != nil {
					return mediaanalysis.MediaInspection{}, err
				}
			}
			frameRate, err := parseFrameRate(stream.AverageRate, stream.RealRate)
			if err != nil {
				return mediaanalysis.MediaInspection{}, err
			}
			bitRate, err := parseOptionalInteger(stream.BitRate)
			if err != nil {
				return mediaanalysis.MediaInspection{}, err
			}
			inspection.VideoStreams = append(inspection.VideoStreams, mediaanalysis.VideoStream{
				Index: stream.Index, Codec: stream.CodecName, Profile: stream.Profile, Width: stream.Width, Height: stream.Height,
				FrameRate: frameRate, PixelFormat: stream.PixelFormat, BitRate: bitRate, DurationMillis: videoDuration,
			})
		case "audio":
			sampleRate, err := parseRequiredInteger(stream.SampleRate)
			if err != nil || sampleRate > int64(int(^uint(0)>>1)) {
				return mediaanalysis.MediaInspection{}, ErrInvalidOutput
			}
			audioBitRate, err := parseOptionalInteger(stream.BitRate)
			if err != nil {
				return mediaanalysis.MediaInspection{}, err
			}
			inspection.AudioStreams = append(inspection.AudioStreams, mediaanalysis.AudioStream{
				Index: stream.Index, Codec: stream.CodecName, Profile: stream.Profile, BitRate: audioBitRate, SampleRateHz: int(sampleRate),
				Channels: stream.Channels, ChannelLayout: stream.ChannelLayout,
			})
		}
	}
	return inspection, nil
}

func parseDurationMillis(value string) (int64, error) {
	millis, err := parseMillis(value)
	if err != nil || millis == 0 {
		return 0, ErrInvalidOutput
	}
	return millis, nil
}

func parseMillis(value string) (int64, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 || seconds >= float64(math.MaxInt64)/1000 {
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
