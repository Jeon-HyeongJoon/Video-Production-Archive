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
	return (OSRunner{}).RunStreams(ctx, executable, arguments, nil, nil)
}

func (OSRunner) RunStreams(ctx context.Context, executable string, arguments []string, stdout, stderr io.Writer) error {
	process := exec.CommandContext(ctx, executable, arguments...)
	process.Stdout, process.Stderr = stdout, stderr
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
	RenditionID           string
	OriginalAssetID       string
	ResolvedInputPath     string
	TemporaryOutputPath   string
	PublishedOutputPath   string
	ArtifactPath          string
	TemporaryArtifactPath string
	OutputRoot            *archive.StorageRoot
	Profile               rendering.Profile
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
		strings.EqualFold(filepath.Clean(request.TemporaryOutputPath), filepath.Clean(request.PublishedOutputPath)) ||
		strings.EqualFold(filepath.Clean(request.TemporaryOutputPath), filepath.Clean(request.ResolvedInputPath)) ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) {
		return rendering.Rendition{}, ErrInvalidRequest
	}
	if request.OutputRoot != nil {
		temporaryArtifactPath, temporaryOK := normalizeArtifactPath(request.TemporaryArtifactPath)
		temporaryResolved, temporaryErr := request.OutputRoot.ExternalProcessPath(temporaryArtifactPath)
		publishedResolved, publishedErr := request.OutputRoot.ExternalProcessPath(artifactPath)
		if !temporaryOK || temporaryArtifactPath == artifactPath || path.Dir(temporaryArtifactPath) != path.Dir(artifactPath) ||
			temporaryErr != nil || publishedErr != nil || filepath.Clean(temporaryResolved) != filepath.Clean(request.TemporaryOutputPath) ||
			filepath.Clean(publishedResolved) != filepath.Clean(request.PublishedOutputPath) {
			return rendering.Rendition{}, ErrInvalidRequest
		}
		if err := request.OutputRoot.MkdirAll(path.Dir(artifactPath), 0o755); err != nil {
			return rendering.Rendition{}, err
		}
	} else if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return rendering.Rendition{}, err
	}
	arguments := renderArguments(request.ResolvedInputPath, request.TemporaryOutputPath, profile)
	plan := execution.Plan{
		Executable: renderer.executable, Arguments: arguments,
		TemporaryOutput: request.TemporaryOutputPath, PublishedOutput: request.PublishedOutputPath,
	}
	if err := execution.RunVerified(ctx, plan, renderer.runner, renderer.verifier, renderer.publisher); err != nil {
		return rendering.Rendition{}, errors.Join(ErrRenderFailed, err, removeTemporaryFile(request.OutputRoot, request.TemporaryArtifactPath, request.TemporaryOutputPath))
	}
	var size int64
	var checksum string
	if request.OutputRoot != nil {
		size, checksum, err = request.OutputRoot.FileSHA256(artifactPath)
	} else {
		size, checksum, err = hashFile(request.PublishedOutputPath)
	}
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

// Remove only the attempt's media file. An occupied directory is an output
// error and may contain someone else's files.
func removeTemporaryFile(root *archive.StorageRoot, relative, resolved string) error {
	var info os.FileInfo
	var err error
	if root != nil {
		info, err = root.Stat(relative)
	} else {
		info, err = os.Lstat(resolved)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	if root != nil {
		return root.Remove(relative)
	}
	return os.Remove(resolved)
}

func renderArguments(inputPath, outputPath string, profile rendering.Profile) []string {
	scale := "scale=" + strconv.Itoa(profile.MaxWidth) + ":" + strconv.Itoa(profile.MaxHeight) +
		":force_original_aspect_ratio=decrease:force_divisible_by=2"
	arguments := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", inputPath,
		"-map", "0:v:0", "-map", "0:a?",
		"-vf", scale,
		"-c:v", profile.VideoEncoder,
	}
	if profile.TargetVideoBitRateKbps > 0 {
		bitRate := strconv.Itoa(profile.TargetVideoBitRateKbps) + "k"
		arguments = append(arguments, "-b:v", bitRate, "-maxrate", bitRate, "-bufsize", strconv.Itoa(profile.TargetVideoBitRateKbps*2)+"k")
	} else {
		arguments = append(arguments, "-crf", strconv.Itoa(profile.ConstantRateFactor))
	}
	return append(arguments,
		"-pix_fmt", profile.PixelFormat,
		"-c:a", profile.AudioEncoder, "-b:a", strconv.Itoa(profile.AudioBitRateKbps)+"k",
		"-threads", strconv.Itoa(profile.ThreadCount),
		"-movflags", "+faststart", "-f", profile.Container,
		outputPath,
	)
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
