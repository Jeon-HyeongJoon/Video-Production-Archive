package subtitlefile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/subtitle"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid subtitle publish request")
	ErrPublishFailed  = errors.New("subtitle publish failed")
)

type Request struct {
	SubtitleID    string
	Transcript    transcription.Transcript
	Profile       subtitle.Profile
	TemporaryPath string
	PublishedPath string
}

type Publisher struct {
	root             *archive.StorageRoot
	processorVersion string
	workerID         string
	clock            func() time.Time
}

func NewPublisher(root *archive.StorageRoot, processorVersion, workerID string) (*Publisher, error) {
	if root == nil || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" {
		return nil, ErrInvalidRequest
	}
	return &Publisher{
		root: root, processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (publisher *Publisher) Publish(request Request) (subtitle.Subtitle, error) {
	profile, err := subtitle.NewProfile(request.Profile)
	if err != nil {
		return subtitle.Subtitle{}, err
	}
	temporaryPath, temporaryOK := normalizeArtifactPath(request.TemporaryPath)
	publishedPath, publishedOK := normalizeArtifactPath(request.PublishedPath)
	if strings.TrimSpace(request.SubtitleID) == "" || request.Transcript.Validate() != nil || !temporaryOK || !publishedOK ||
		temporaryPath == publishedPath || path.Dir(temporaryPath) != path.Dir(publishedPath) {
		return subtitle.Subtitle{}, ErrInvalidRequest
	}
	payload, err := subtitle.RenderSRT(request.Transcript.Content.Segments)
	if err != nil {
		return subtitle.Subtitle{}, err
	}
	if directory := path.Dir(publishedPath); directory != "." {
		if err := publisher.root.MkdirAll(directory, 0o755); err != nil {
			return subtitle.Subtitle{}, errors.Join(ErrPublishFailed, err)
		}
	}
	defer publisher.root.Remove(temporaryPath)
	if err := publisher.root.WriteFile(temporaryPath, payload, 0o600); err != nil {
		return subtitle.Subtitle{}, errors.Join(ErrPublishFailed, err)
	}
	verified, err := publisher.root.ReadFile(temporaryPath)
	if err != nil || !bytes.Equal(verified, payload) {
		return subtitle.Subtitle{}, errors.Join(ErrPublishFailed, err)
	}
	digest := sha256.Sum256(verified)
	artifact := archive.Artifact{
		ID: strings.TrimSpace(request.SubtitleID), Path: publishedPath, Size: int64(len(verified)), Checksum: hex.EncodeToString(digest[:]),
		Origin: archive.Provenance{
			OriginalAssetID:  request.Transcript.Artifact.Origin.OriginalAssetID,
			InputArtifactIDs: []string{request.Transcript.Artifact.ID},
			ProcessingKind:   vocabulary.ProcessingSTT, ProcessorVersion: publisher.processorVersion,
			ProfileVersion: profile.Reference(), CreatedBy: publisher.workerID, CreatedAt: publisher.clock(),
		},
	}
	result, err := subtitle.NewSubtitle(artifact, profile, request.Transcript)
	if err != nil {
		return subtitle.Subtitle{}, err
	}
	if err := publisher.root.Rename(temporaryPath, publishedPath); err != nil {
		return subtitle.Subtitle{}, errors.Join(ErrPublishFailed, err)
	}
	return result, nil
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
