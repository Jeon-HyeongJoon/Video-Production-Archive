package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid ffmpeg render request")
	ErrRenderFailed   = errors.New("ffmpeg render failed")
)

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, executable string, arguments []string) error {
	process := exec.CommandContext(ctx, executable, arguments...)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	return process.Run()
}

type AtomicPublisher struct{}

func (AtomicPublisher) Publish(temporaryPath, publishedPath string) error {
	if filepath.Clean(filepath.Dir(temporaryPath)) != filepath.Clean(filepath.Dir(publishedPath)) {
		return ErrInvalidRequest
	}
	return os.Rename(temporaryPath, publishedPath)
}

type Request struct {
	RenditionID         string
	OriginalAssetID     string
	ResolvedInputPath   string
	TemporaryOutputPath string
	PublishedOutputPath string
	ArtifactPath        string
	Profile             rendering.Profile
}

type Renderer struct {
	executable       string
	processorVersion string
	workerID         string
	runner           execution.Runner
	verifier         execution.Verifier
	publisher        execution.Publisher
	clock            func() time.Time
}

func NewRenderer(executable, processorVersion, workerID string, runner execution.Runner, verifier execution.Verifier, publisher execution.Publisher) (*Renderer, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" ||
		runner == nil || verifier == nil || publisher == nil {
		return nil, ErrInvalidRequest
	}
	return &Renderer{
		executable: strings.TrimSpace(executable), processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		runner: runner, verifier: verifier, publisher: publisher, clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (renderer *Renderer) Render(ctx context.Context, request Request) (rendering.Rendition, error) {
	profile, err := rendering.NewProfile(request.Profile)
	if err != nil {
		return rendering.Rendition{}, err
	}
	artifactPath, ok := normalizeArtifactPath(request.ArtifactPath)
	if strings.TrimSpace(request.RenditionID) == "" || strings.TrimSpace(request.OriginalAssetID) == "" ||
		strings.TrimSpace(request.ResolvedInputPath) == "" || strings.TrimSpace(request.TemporaryOutputPath) == "" ||
		strings.TrimSpace(request.PublishedOutputPath) == "" || !ok ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) {
		return rendering.Rendition{}, ErrInvalidRequest
	}
	if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return rendering.Rendition{}, err
	}
	arguments := renderArguments(request.ResolvedInputPath, request.TemporaryOutputPath, profile)
	plan := execution.Plan{
		Executable: renderer.executable, Arguments: arguments,
		TemporaryOutput: request.TemporaryOutputPath, PublishedOutput: request.PublishedOutputPath,
	}
	if err := execution.RunVerified(ctx, plan, renderer.runner, renderer.verifier, renderer.publisher); err != nil {
		return rendering.Rendition{}, errors.Join(ErrRenderFailed, err)
	}
	size, checksum, err := hashFile(request.PublishedOutputPath)
	if err != nil || size <= 0 {
		return rendering.Rendition{}, errors.Join(ErrRenderFailed, err)
	}
	artifact := archive.Artifact{
		ID: strings.TrimSpace(request.RenditionID), Path: artifactPath, Size: size, Checksum: checksum,
		Origin: archive.Provenance{
			OriginalAssetID: strings.TrimSpace(request.OriginalAssetID), InputArtifactIDs: []string{},
			ProcessingKind: vocabulary.ProcessingRender, ProcessorVersion: renderer.processorVersion,
			ProfileVersion: profile.Reference(), CreatedBy: renderer.workerID, CreatedAt: renderer.clock(),
		},
	}
	rendition, err := rendering.NewRendition(artifact, profile)
	if err != nil {
		return rendering.Rendition{}, err
	}
	return rendition, nil
}

func renderArguments(inputPath, outputPath string, profile rendering.Profile) []string {
	scale := "scale=" + strconv.Itoa(profile.MaxWidth) + ":" + strconv.Itoa(profile.MaxHeight) +
		":force_original_aspect_ratio=decrease:force_divisible_by=2"
	return []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", inputPath,
		"-map", "0:v:0", "-map", "0:a?",
		"-vf", scale,
		"-c:v", profile.VideoEncoder, "-crf", strconv.Itoa(profile.ConstantRateFactor), "-pix_fmt", profile.PixelFormat,
		"-c:a", profile.AudioEncoder, "-b:a", strconv.Itoa(profile.AudioBitRateKbps) + "k",
		"-threads", strconv.Itoa(profile.ThreadCount),
		"-movflags", "+faststart", "-f", profile.Container,
		outputPath,
	}
}

func normalizeArtifactPath(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "", false
	}
	return clean, clean == normalized
}

func hashFile(filePath string) (int64, string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
