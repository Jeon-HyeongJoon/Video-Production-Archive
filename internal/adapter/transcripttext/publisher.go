package transcripttext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid transcript text publish request")
	ErrPublishFailed  = errors.New("transcript text publish failed")
)

type Request struct {
	ID            string
	Transcript    transcription.Transcript
	TemporaryPath string
	PublishedPath string
}

type Publisher struct {
	root     *archive.StorageRoot
	workerID string
	now      func() time.Time
}

func NewPublisher(root *archive.StorageRoot, workerID string, now func() time.Time) (*Publisher, error) {
	if root == nil || strings.TrimSpace(workerID) == "" || now == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{root: root, workerID: strings.TrimSpace(workerID), now: now}, nil
}

func (publisher *Publisher) Publish(request Request) (archive.Artifact, error) {
	request.ID = strings.TrimSpace(request.ID)
	temporary := normalizePath(request.TemporaryPath)
	published := normalizePath(request.PublishedPath)
	if request.ID == "" || request.Transcript.Validate() != nil || temporary == "" || published == "" ||
		temporary == published || path.Dir(temporary) != path.Dir(published) {
		return archive.Artifact{}, ErrInvalidRequest
	}
	var builder strings.Builder
	for _, segment := range request.Transcript.Content.Segments {
		builder.WriteByte('[')
		builder.WriteString(formatTimecode(segment.StartMillis))
		builder.WriteString(" --> ")
		builder.WriteString(formatTimecode(segment.EndMillis))
		builder.WriteString("] ")
		builder.WriteString(segment.Text)
		builder.WriteByte('\n')
	}
	payload := []byte(builder.String())
	if directory := path.Dir(published); directory != "." {
		if err := publisher.root.MkdirAll(directory, 0o755); err != nil {
			return archive.Artifact{}, errors.Join(ErrPublishFailed, err)
		}
	}
	defer publisher.root.Remove(temporary)
	if err := publisher.root.WriteFile(temporary, payload, 0o600); err != nil {
		return archive.Artifact{}, errors.Join(ErrPublishFailed, err)
	}
	verified, err := publisher.root.ReadFile(temporary)
	if err != nil || !bytes.Equal(payload, verified) {
		return archive.Artifact{}, errors.Join(ErrPublishFailed, err)
	}
	sum := sha256.Sum256(verified)
	artifact := archive.Artifact{
		ID: request.ID, Path: published, Size: int64(len(verified)), Checksum: hex.EncodeToString(sum[:]),
		Origin: archive.Provenance{
			OriginalAssetID:  request.Transcript.Artifact.Origin.OriginalAssetID,
			InputArtifactIDs: []string{request.Transcript.Artifact.ID}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: "archive-transcript-text-v2", ProfileVersion: "plain-text-timecode@v1",
			CreatedBy: publisher.workerID, CreatedAt: publisher.now().UTC(),
		},
	}
	if artifact.ValidateDerived() != nil {
		return archive.Artifact{}, ErrInvalidRequest
	}
	if err := publisher.root.Rename(temporary, published); err != nil {
		return archive.Artifact{}, errors.Join(ErrPublishFailed, err)
	}
	return artifact, nil
}

func formatTimecode(milliseconds int64) string {
	hours := milliseconds / 3_600_000
	milliseconds %= 3_600_000
	minutes := milliseconds / 60_000
	milliseconds %= 60_000
	seconds := milliseconds / 1_000
	milliseconds %= 1_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, milliseconds)
}

func normalizePath(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return ""
	}
	return clean
}
