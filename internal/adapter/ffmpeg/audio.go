package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
)

var ErrAudioExtractionFailed = errors.New("ffmpeg audio extraction failed")

type AudioRequest struct {
	AudioArtifactID       string
	OriginalAssetID       string
	ResolvedInputPath     string
	TemporaryOutputPath   string
	PublishedOutputPath   string
	ArtifactPath          string
	TemporaryArtifactPath string
	OutputRoot            *archive.StorageRoot
	Profile               audio.Profile
}

type AudioExtractor struct {
	executable       string
	processorVersion string
	workerID         string
	runner           execution.Runner
	verifier         execution.Verifier
	publisher        execution.Publisher
	clock            func() time.Time
}

func NewAudioExtractor(executable, processorVersion, workerID string, runner execution.Runner, verifier execution.Verifier, publisher execution.Publisher) (*AudioExtractor, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" ||
		runner == nil || verifier == nil || publisher == nil {
		return nil, ErrInvalidRequest
	}
	return &AudioExtractor{
		executable: strings.TrimSpace(executable), processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		runner: runner, verifier: verifier, publisher: publisher, clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (extractor *AudioExtractor) Extract(ctx context.Context, request AudioRequest) (audio.AudioArtifact, error) {
	profile, err := audio.NewProfile(request.Profile)
	if err != nil {
		return audio.AudioArtifact{}, err
	}
	artifactPath, pathOK := normalizeArtifactPath(request.ArtifactPath)
	if strings.TrimSpace(request.AudioArtifactID) == "" || strings.TrimSpace(request.OriginalAssetID) == "" ||
		strings.TrimSpace(request.ResolvedInputPath) == "" || strings.TrimSpace(request.TemporaryOutputPath) == "" ||
		strings.TrimSpace(request.PublishedOutputPath) == "" || !pathOK ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) {
		return audio.AudioArtifact{}, ErrInvalidRequest
	}
	if request.OutputRoot != nil {
		temporaryArtifactPath, temporaryOK := normalizeArtifactPath(request.TemporaryArtifactPath)
		temporaryResolved, temporaryErr := request.OutputRoot.ExternalProcessPath(temporaryArtifactPath)
		publishedResolved, publishedErr := request.OutputRoot.ExternalProcessPath(artifactPath)
		if !temporaryOK || temporaryArtifactPath == artifactPath || path.Dir(temporaryArtifactPath) != path.Dir(artifactPath) ||
			temporaryErr != nil || publishedErr != nil || filepath.Clean(temporaryResolved) != filepath.Clean(request.TemporaryOutputPath) ||
			filepath.Clean(publishedResolved) != filepath.Clean(request.PublishedOutputPath) {
			return audio.AudioArtifact{}, ErrInvalidRequest
		}
		if err := request.OutputRoot.MkdirAll(path.Dir(artifactPath), 0o755); err != nil {
			return audio.AudioArtifact{}, err
		}
	} else if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return audio.AudioArtifact{}, err
	}
	arguments := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", request.ResolvedInputPath,
		"-vn",
	}
	if profile.FilterGraph != "" {
		arguments = append(arguments, "-af", profile.FilterGraph)
	}
	arguments = append(arguments,
		"-ac", strconv.Itoa(profile.Channels), "-ar", strconv.Itoa(profile.SampleRateHz),
		"-c:a", profile.Codec, "-threads", strconv.Itoa(profile.ThreadCount), "-f", profile.Container,
		request.TemporaryOutputPath,
	)
	plan := execution.Plan{
		Executable: extractor.executable, Arguments: arguments,
		TemporaryOutput: request.TemporaryOutputPath, PublishedOutput: request.PublishedOutputPath,
	}
	if err := execution.RunVerified(ctx, plan, extractor.runner, extractor.verifier, extractor.publisher); err != nil {
		return audio.AudioArtifact{}, errors.Join(ErrAudioExtractionFailed, err)
	}
	var size int64
	var checksum string
	if request.OutputRoot != nil {
		size, checksum, err = request.OutputRoot.FileSHA256(artifactPath)
	} else {
		size, checksum, err = hashFile(request.PublishedOutputPath)
	}
	if err != nil || size <= 0 {
		return audio.AudioArtifact{}, errors.Join(ErrAudioExtractionFailed, err)
	}
	artifact := archive.Artifact{
		ID: strings.TrimSpace(request.AudioArtifactID), Path: artifactPath, Size: size, Checksum: checksum,
		Origin: archive.Provenance{
			OriginalAssetID: strings.TrimSpace(request.OriginalAssetID), InputArtifactIDs: []string{},
			ProcessingKind: vocabulary.ProcessingAudioExtract, ProcessorVersion: extractor.processorVersion,
			ProfileVersion: profile.Reference(), CreatedBy: extractor.workerID, CreatedAt: extractor.clock(),
		},
	}
	result, err := audio.NewAudioArtifact(artifact, profile)
	if err != nil {
		return audio.AudioArtifact{}, err
	}
	return result, nil
}
