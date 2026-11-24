package organizedmedia

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid organized media publish request")
	ErrPublishFailed  = errors.New("organized media publish failed")
)

type Storage string

const (
	StorageSourceMedia Storage = "SOURCE_MEDIA"
	StorageWorking     Storage = "WORKING"
)

type FileSource struct {
	Storage  Storage
	Path     string
	Checksum string
}

type Request struct {
	WorkspaceDriveID   string
	MediaID            string
	DecisionID         string
	Disposition        vocabulary.OrganizationDisposition
	TranscriptEligible bool
	Sources            []organization.BundleSource
	Video              FileSource
	Transcript         *FileSource
	Subtitle           *FileSource
}

type Publisher struct {
	sourceRoot  *archive.StorageRoot
	workingRoot *archive.StorageRoot
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	checksumPattern   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func NewPublisher(sourceRoot, workingRoot *archive.StorageRoot) (*Publisher, error) {
	if sourceRoot == nil || workingRoot == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{sourceRoot: sourceRoot, workingRoot: workingRoot}, nil
}

func (publisher *Publisher) Publish(request Request) (organization.OrganizedMediaBundle, error) {
	workspaceDriveID := strings.TrimSpace(request.WorkspaceDriveID)
	mediaID := strings.TrimSpace(request.MediaID)
	decisionID := strings.TrimSpace(request.DecisionID)
	if !identifierPattern.MatchString(workspaceDriveID) || !identifierPattern.MatchString(mediaID) || decisionID == "" ||
		!request.Disposition.ProducesMedia() || len(request.Sources) == 0 ||
		(request.TranscriptEligible && (request.Transcript == nil || request.Subtitle == nil)) ||
		(!request.TranscriptEligible && (request.Transcript != nil || request.Subtitle != nil)) {
		return organization.OrganizedMediaBundle{}, ErrInvalidRequest
	}
	sources := append([]organization.BundleSource(nil), request.Sources...)
	seenAssets := make(map[string]bool, len(sources))
	for index := range sources {
		source := &sources[index]
		source.OriginalAssetID = strings.TrimSpace(source.OriginalAssetID)
		source.OriginalName = strings.TrimSpace(source.OriginalName)
		var ok bool
		source.RelativePath, ok = normalizePath(source.RelativePath)
		source.Checksum = strings.ToLower(strings.TrimSpace(source.Checksum))
		if source.OriginalAssetID == "" || source.OriginalName == "" || !ok || !checksumPattern.MatchString(source.Checksum) || seenAssets[source.OriginalAssetID] {
			return organization.OrganizedMediaBundle{}, ErrInvalidRequest
		}
		seenAssets[source.OriginalAssetID] = true
		size, checksum, err := publisher.sourceRoot.FileSHA256(source.RelativePath)
		if err != nil || size <= 0 || checksum != source.Checksum {
			return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
		}
	}
	directory := path.Join("organized", workspaceDriveID, mediaID)
	if err := publisher.workingRoot.MkdirAll(directory, 0o755); err != nil {
		return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
	}
	type stagedFile struct {
		source FileSource
		target string
		temp   string
	}
	staged := []stagedFile{{source: request.Video, target: path.Join(directory, "video.mp4"), temp: path.Join(directory, "video.mp4.partial")}}
	if request.TranscriptEligible {
		staged = append(staged,
			stagedFile{source: *request.Transcript, target: path.Join(directory, "transcript.json"), temp: path.Join(directory, "transcript.json.partial")},
			stagedFile{source: *request.Subtitle, target: path.Join(directory, "subtitle.srt"), temp: path.Join(directory, "subtitle.srt.partial")},
		)
	}
	for _, file := range staged {
		defer publisher.workingRoot.Remove(file.temp)
		if err := publisher.stage(file); err != nil {
			return organization.OrganizedMediaBundle{}, err
		}
	}
	result := organization.OrganizedMediaBundle{
		WorkspaceDriveID: workspaceDriveID, MediaID: mediaID, DecisionID: decisionID, Disposition: request.Disposition,
		Sources: sources, VideoPath: staged[0].target, VideoChecksum: strings.ToLower(strings.TrimSpace(request.Video.Checksum)),
		MetadataPath: path.Join(directory, "metadata.json"),
	}
	if request.TranscriptEligible {
		result.TranscriptPath = staged[1].target
		result.SubtitlePath = staged[2].target
	}
	metadata, err := json.Marshal(result)
	if err != nil {
		return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
	}
	metadataTemp := result.MetadataPath + ".partial"
	defer publisher.workingRoot.Remove(metadataTemp)
	if err := publisher.workingRoot.WriteFile(metadataTemp, metadata, 0o600); err != nil {
		return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
	}
	verifiedMetadata, err := publisher.workingRoot.ReadFile(metadataTemp)
	if err != nil || !bytes.Equal(verifiedMetadata, metadata) {
		return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
	}
	for _, file := range staged {
		if err := publisher.workingRoot.Rename(file.temp, file.target); err != nil {
			return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
		}
	}
	if err := publisher.workingRoot.Rename(metadataTemp, result.MetadataPath); err != nil {
		return organization.OrganizedMediaBundle{}, errors.Join(ErrPublishFailed, err)
	}
	return result, nil
}

func (publisher *Publisher) stage(file struct {
	source FileSource
	target string
	temp   string
}) error {
	sourcePath, ok := normalizePath(file.source.Path)
	expectedChecksum := strings.ToLower(strings.TrimSpace(file.source.Checksum))
	if !ok || !checksumPattern.MatchString(expectedChecksum) {
		return ErrInvalidRequest
	}
	var sourceRoot *archive.StorageRoot
	switch file.source.Storage {
	case StorageSourceMedia:
		sourceRoot = publisher.sourceRoot
	case StorageWorking:
		sourceRoot = publisher.workingRoot
	default:
		return ErrInvalidRequest
	}
	size, copiedChecksum, err := publisher.workingRoot.CopyFileFrom(sourceRoot, sourcePath, file.temp, 0o600)
	if err != nil || size <= 0 || copiedChecksum != expectedChecksum {
		return errors.Join(ErrPublishFailed, err)
	}
	verifiedSize, verifiedChecksum, err := publisher.workingRoot.FileSHA256(file.temp)
	if err != nil || verifiedSize != size || verifiedChecksum != expectedChecksum {
		return errors.Join(ErrPublishFailed, err)
	}
	return nil
}

func normalizePath(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "", false
	}
	return clean, clean == normalized
}
