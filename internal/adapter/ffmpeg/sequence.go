package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/rendering"
	"videoarchive/internal/core/vocabulary"
)

type SequenceRequest struct {
	RenditionID           string
	MarkerArtifactID      string
	Timeline              organization.SourceTimeline
	ResolvedInputPaths    map[string]string
	HasAudio              bool
	TemporaryOutputPath   string
	PublishedOutputPath   string
	ArtifactPath          string
	TemporaryArtifactPath string
	OutputRoot            *archive.StorageRoot
	TemporaryMarkerPath   string
	PublishedMarkerPath   string
	Profile               rendering.Profile
}

type SequenceResult struct {
	Rendition      rendering.Rendition
	MarkerArtifact archive.Artifact
}

var ErrSequenceOutputConflict = errors.New("existing sequence output differs from verified replacement")

type SequenceRenderer struct {
	executable       string
	processorVersion string
	workerID         string
	runner           execution.Runner
	verifier         execution.Verifier
	publisher        execution.Publisher
	markerRoot       *archive.StorageRoot
	clock            func() time.Time
}

func NewSequenceRenderer(executable, processorVersion, workerID string, runner execution.Runner, verifier execution.Verifier, publisher execution.Publisher, markerRoot *archive.StorageRoot) (*SequenceRenderer, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" ||
		runner == nil || verifier == nil || publisher == nil || markerRoot == nil {
		return nil, ErrInvalidRequest
	}
	return &SequenceRenderer{
		executable: strings.TrimSpace(executable), processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		runner: runner, verifier: verifier, publisher: publisher, markerRoot: markerRoot, clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (renderer *SequenceRenderer) Render(ctx context.Context, request SequenceRequest) (_ SequenceResult, renderErr error) {
	profile, err := rendering.NewProfile(request.Profile)
	if err != nil || profile.TargetVideoBitRateKbps != 20_000 || request.Timeline.Validate() != nil {
		return SequenceResult{}, ErrInvalidRequest
	}
	artifactPath, artifactPathOK := normalizeArtifactPath(request.ArtifactPath)
	temporaryMarkerPath, temporaryMarkerOK := normalizeArtifactPath(request.TemporaryMarkerPath)
	publishedMarkerPath, publishedMarkerOK := normalizeArtifactPath(request.PublishedMarkerPath)
	if strings.TrimSpace(request.RenditionID) == "" || strings.TrimSpace(request.MarkerArtifactID) == "" ||
		strings.TrimSpace(request.TemporaryOutputPath) == "" || strings.TrimSpace(request.PublishedOutputPath) == "" ||
		!artifactPathOK || !temporaryMarkerOK || !publishedMarkerOK || strings.EqualFold(temporaryMarkerPath, publishedMarkerPath) ||
		strings.EqualFold(filepath.Clean(request.TemporaryOutputPath), filepath.Clean(request.PublishedOutputPath)) ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) ||
		path.Dir(temporaryMarkerPath) != path.Dir(publishedMarkerPath) {
		return SequenceResult{}, ErrInvalidRequest
	}
	if request.OutputRoot != nil {
		temporaryArtifactPath, temporaryOK := normalizeArtifactPath(request.TemporaryArtifactPath)
		temporaryResolved, temporaryErr := request.OutputRoot.ExternalProcessPath(temporaryArtifactPath)
		publishedResolved, publishedErr := request.OutputRoot.ExternalProcessPath(artifactPath)
		if !temporaryOK || temporaryArtifactPath == artifactPath || path.Dir(temporaryArtifactPath) != path.Dir(artifactPath) ||
			temporaryErr != nil || publishedErr != nil || filepath.Clean(temporaryResolved) != filepath.Clean(request.TemporaryOutputPath) ||
			filepath.Clean(publishedResolved) != filepath.Clean(request.PublishedOutputPath) {
			return SequenceResult{}, ErrInvalidRequest
		}
	}
	inputPaths := make([]string, 0, len(request.Timeline.Clips))
	rootOriginalAssetIDs := make([]string, 0, len(request.Timeline.Clips))
	for _, clip := range request.Timeline.Clips {
		inputPath := strings.TrimSpace(request.ResolvedInputPaths[clip.OriginalAssetID])
		if inputPath == "" || strings.EqualFold(filepath.Clean(request.TemporaryOutputPath), filepath.Clean(inputPath)) {
			return SequenceResult{}, ErrInvalidRequest
		}
		inputPaths = append(inputPaths, inputPath)
		rootOriginalAssetIDs = append(rootOriginalAssetIDs, clip.OriginalAssetID)
	}
	if request.OutputRoot != nil {
		if err := request.OutputRoot.MkdirAll(path.Dir(artifactPath), 0o755); err != nil {
			return SequenceResult{}, err
		}
	} else if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return SequenceResult{}, err
	}
	arguments := sequenceArguments(inputPaths, request.HasAudio, request.TemporaryOutputPath, profile)
	defer func() {
		if renderErr != nil {
			renderErr = errors.Join(renderErr, removeTemporaryFile(request.OutputRoot, request.TemporaryArtifactPath, request.TemporaryOutputPath))
		}
	}()
	if err := renderer.runner.Run(ctx, renderer.executable, arguments); err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	if err := renderer.verifier.Verify(request.TemporaryOutputPath); err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	markerPayload, err := json.Marshal(struct {
		SchemaVersion uint32                      `json:"schema_version"`
		Timeline      organization.SourceTimeline `json:"timeline"`
	}{SchemaVersion: 1, Timeline: request.Timeline})
	if err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	defer func() {
		if renderErr != nil {
			renderErr = errors.Join(renderErr, removeTemporaryFile(renderer.markerRoot, temporaryMarkerPath, ""))
		}
	}()
	if err := renderer.markerRoot.WriteFile(temporaryMarkerPath, markerPayload, 0o600); err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	readBack, err := renderer.markerRoot.ReadFile(temporaryMarkerPath)
	if err != nil || string(readBack) != string(markerPayload) {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	// Preflight both published names under the Worker's output locks before
	// publishing a missing member of the pair.
	markerExists, err := matchingSequenceFileExists(renderer.markerRoot, publishedMarkerPath, temporaryMarkerPath)
	if err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	currentVideo, stagedVideo := artifactPath, request.TemporaryArtifactPath
	if request.OutputRoot == nil {
		currentVideo, stagedVideo = request.PublishedOutputPath, request.TemporaryOutputPath
	}
	videoExists, err := matchingSequenceFileExists(request.OutputRoot, currentVideo, stagedVideo)
	if err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return SequenceResult{}, err
	}
	if videoExists {
		err = removeTemporaryFile(request.OutputRoot, request.TemporaryArtifactPath, request.TemporaryOutputPath)
	} else {
		err = renderer.publisher.Publish(request.TemporaryOutputPath, request.PublishedOutputPath)
	}
	if err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	if markerExists {
		err = renderer.markerRoot.Remove(temporaryMarkerPath)
	} else {
		err = renderer.markerRoot.Rename(temporaryMarkerPath, publishedMarkerPath)
	}
	if err != nil {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}

	var videoSize int64
	var videoChecksum string
	if request.OutputRoot != nil {
		videoSize, videoChecksum, err = request.OutputRoot.FileSHA256(artifactPath)
	} else {
		videoSize, videoChecksum, err = hashFile(request.PublishedOutputPath)
	}
	if err != nil || videoSize <= 0 {
		return SequenceResult{}, errors.Join(ErrRenderFailed, err)
	}
	markerDigest := sha256.Sum256(markerPayload)
	sort.Strings(rootOriginalAssetIDs)
	createdAt := renderer.clock()
	provenance := archive.Provenance{
		RootOriginalAssetIDs: rootOriginalAssetIDs, InputTimelineID: request.Timeline.ID, InputArtifactIDs: []string{},
		ProcessingKind: vocabulary.ProcessingRender, ProcessorVersion: renderer.processorVersion,
		ProfileVersion: profile.Reference(), CreatedBy: renderer.workerID, CreatedAt: createdAt,
	}
	videoArtifact := archive.Artifact{
		ID: strings.TrimSpace(request.RenditionID), Path: artifactPath, Size: videoSize, Checksum: videoChecksum, Origin: provenance,
	}
	rendition, err := rendering.NewRendition(videoArtifact, profile)
	if err != nil {
		return SequenceResult{}, err
	}
	markerArtifact := archive.Artifact{
		ID: strings.TrimSpace(request.MarkerArtifactID), Path: publishedMarkerPath, Size: int64(len(markerPayload)),
		Checksum: hex.EncodeToString(markerDigest[:]), Origin: provenance,
	}
	if err := markerArtifact.ValidateDerived(); err != nil {
		return SequenceResult{}, err
	}
	return SequenceResult{Rendition: rendition, MarkerArtifact: markerArtifact}, nil
}

func matchingSequenceFileExists(root *archive.StorageRoot, published, staged string) (bool, error) {
	stat := os.Lstat
	hash := hashFile
	if root != nil {
		stat = root.Stat
		hash = root.FileSHA256
	}
	info, err := stat(published)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, ErrSequenceOutputConflict
	}
	size, sum, err := hash(published)
	if err != nil {
		return false, err
	}
	newSize, newSum, err := hash(staged)
	if err != nil {
		return false, err
	}
	if size != newSize || sum != newSum {
		return false, ErrSequenceOutputConflict
	}
	return true, nil
}

func sequenceArguments(inputPaths []string, hasAudio bool, outputPath string, profile rendering.Profile) []string {
	arguments := []string{"-hide_banner", "-nostdin", "-y"}
	for _, inputPath := range inputPaths {
		arguments = append(arguments, "-i", inputPath)
	}
	var inputs strings.Builder
	for index := range inputPaths {
		inputs.WriteString("[")
		inputs.WriteString(strconv.Itoa(index))
		inputs.WriteString(":v:0]")
		if hasAudio {
			inputs.WriteString("[")
			inputs.WriteString(strconv.Itoa(index))
			inputs.WriteString(":a:0]")
		}
	}
	concat := inputs.String() + "concat=n=" + strconv.Itoa(len(inputPaths)) + ":v=1:a=" + strconv.Itoa(boolInt(hasAudio)) + "[vcat]"
	if hasAudio {
		concat += "[acat]"
	}
	scale := "[vcat]scale=" + strconv.Itoa(profile.MaxWidth) + ":" + strconv.Itoa(profile.MaxHeight) +
		":force_original_aspect_ratio=decrease:force_divisible_by=2[v]"
	arguments = append(arguments, "-filter_complex", concat+";"+scale, "-map", "[v]")
	if hasAudio {
		arguments = append(arguments, "-map", "[acat]")
	} else {
		arguments = append(arguments, "-an")
	}
	bitRate := strconv.Itoa(profile.TargetVideoBitRateKbps) + "k"
	arguments = append(arguments,
		"-c:v", profile.VideoEncoder, "-b:v", bitRate, "-maxrate", bitRate,
		"-bufsize", strconv.Itoa(profile.TargetVideoBitRateKbps*2)+"k", "-pix_fmt", profile.PixelFormat,
	)
	if hasAudio {
		arguments = append(arguments, "-c:a", profile.AudioEncoder, "-b:a", strconv.Itoa(profile.AudioBitRateKbps)+"k")
	}
	return append(arguments,
		"-threads", strconv.Itoa(profile.ThreadCount), "-movflags", "+faststart", "-f", profile.Container, outputPath,
	)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
