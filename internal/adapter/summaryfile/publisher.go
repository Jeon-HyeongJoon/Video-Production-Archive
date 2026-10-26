package summaryfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/summary"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid summary publish request")
	ErrPublishFailed  = errors.New("summary publish failed")
)

type Request struct {
	SummaryID     string
	Revision      uint64
	ArtifactID    string
	Transcript    transcription.Transcript
	Profile       summary.Profile
	AuthorType    vocabulary.ActorType
	AuthorID      string
	Content       summary.Content
	TemporaryPath string
	PublishedPath string
}

type Publisher struct {
	root             *archive.StorageRoot
	processorVersion string
	clock            func() time.Time
}

func NewPublisher(root *archive.StorageRoot, processorVersion string) (*Publisher, error) {
	if root == nil || strings.TrimSpace(processorVersion) == "" {
		return nil, ErrInvalidRequest
	}
	return &Publisher{
		root: root, processorVersion: strings.TrimSpace(processorVersion),
		clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (publisher *Publisher) Publish(request Request) (summary.Summary, error) {
	profile, err := summary.NewProfile(request.Profile)
	if err != nil {
		return summary.Summary{}, err
	}
	content, err := summary.NewContent(profile, request.Transcript, request.Content)
	if err != nil {
		return summary.Summary{}, err
	}
	temporaryPath, temporaryOK := normalizeArtifactPath(request.TemporaryPath)
	publishedPath, publishedOK := normalizeArtifactPath(request.PublishedPath)
	if strings.TrimSpace(request.SummaryID) == "" || request.Revision == 0 || strings.TrimSpace(request.ArtifactID) == "" ||
		request.Transcript.Validate() != nil || strings.TrimSpace(request.AuthorID) == "" || !temporaryOK || !publishedOK ||
		temporaryPath == publishedPath || path.Dir(temporaryPath) != path.Dir(publishedPath) {
		return summary.Summary{}, ErrInvalidRequest
	}
	document := summaryDocument{
		SchemaVersion: 1, SummaryID: strings.TrimSpace(request.SummaryID), Revision: request.Revision,
		ProfileReference: profile.Reference(), TranscriptID: request.Transcript.Artifact.ID,
		TranscriptDurationMillis: request.Transcript.Content.Segments[len(request.Transcript.Content.Segments)-1].EndMillis,
		Language:                 profile.Language, AuthorType: request.AuthorType, AuthorID: strings.TrimSpace(request.AuthorID), Content: content,
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return summary.Summary{}, err
	}
	if directory := path.Dir(publishedPath); directory != "." {
		if err := publisher.root.MkdirAll(directory, 0o755); err != nil {
			return summary.Summary{}, errors.Join(ErrPublishFailed, err)
		}
	}
	defer publisher.root.Remove(temporaryPath)
	if err := publisher.root.WriteFile(temporaryPath, payload, 0o600); err != nil {
		return summary.Summary{}, errors.Join(ErrPublishFailed, err)
	}
	verified, err := publisher.root.ReadFile(temporaryPath)
	if err != nil || !bytes.Equal(verified, payload) {
		return summary.Summary{}, errors.Join(ErrPublishFailed, err)
	}
	var verifiedDocument summaryDocument
	if err := json.Unmarshal(verified, &verifiedDocument); err != nil || !reflect.DeepEqual(verifiedDocument, document) {
		return summary.Summary{}, errors.Join(ErrPublishFailed, err)
	}
	digest := sha256.Sum256(verified)
	artifact := archive.Artifact{
		ID: strings.TrimSpace(request.ArtifactID), Path: publishedPath, Size: int64(len(verified)), Checksum: hex.EncodeToString(digest[:]),
		Origin: archive.Provenance{
			OriginalAssetID:  request.Transcript.Artifact.Origin.OriginalAssetID,
			InputArtifactIDs: []string{request.Transcript.Artifact.ID}, ProcessingKind: vocabulary.ProcessingSummary,
			ProcessorVersion: publisher.processorVersion, ProfileVersion: profile.Reference(),
			CreatedBy: strings.TrimSpace(request.AuthorID), CreatedAt: publisher.clock(),
		},
	}
	result, err := summary.NewSummary(document.SummaryID, document.Revision, artifact, profile, request.Transcript, document.AuthorType, document.AuthorID, document.Content)
	if err != nil {
		return summary.Summary{}, err
	}
	if err := publisher.root.Rename(temporaryPath, publishedPath); err != nil {
		return summary.Summary{}, errors.Join(ErrPublishFailed, err)
	}
	return result, nil
}

type summaryDocument struct {
	SchemaVersion            uint32               `json:"schema_version"`
	SummaryID                string               `json:"summary_id"`
	Revision                 uint64               `json:"revision"`
	ProfileReference         string               `json:"profile_reference"`
	TranscriptID             string               `json:"transcript_id"`
	TranscriptDurationMillis int64                `json:"transcript_duration_millis"`
	Language                 string               `json:"language"`
	AuthorType               vocabulary.ActorType `json:"author_type"`
	AuthorID                 string               `json:"author_id"`
	Content                  summary.Content      `json:"content"`
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
