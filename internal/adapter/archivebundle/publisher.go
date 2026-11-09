package archivebundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("archive bundle request is invalid")
	ErrPackageExists  = errors.New("archive bundle already exists")
	ErrPackageState   = errors.New("archive bundle state does not permit this operation")
)

type RootKind string

const (
	RootSource  RootKind = "SOURCE"
	RootArchive RootKind = "ARCHIVE"
)

type FileSource struct {
	Root     RootKind
	Path     string
	Checksum string
}

type SourceReference struct {
	OriginalAssetID      string    `json:"original_asset_id"`
	OriginalName         string    `json:"original_name"`
	RelativePath         string    `json:"relative_path"`
	Checksum             string    `json:"checksum"`
	DurationMillis       int64     `json:"duration_millis"`
	SourceCreatedAt      time.Time `json:"source_created_at,omitempty"`
	SizeBytes            int64     `json:"size_bytes,omitempty"`
	Container            string    `json:"container,omitempty"`
	BitRateBPS           int64     `json:"bitrate_bps,omitempty"`
	VideoCodec           string    `json:"video_codec,omitempty"`
	VideoProfile         string    `json:"video_profile,omitempty"`
	Width                int       `json:"width,omitempty"`
	Height               int       `json:"height,omitempty"`
	FrameRateNumerator   int64     `json:"frame_rate_numerator,omitempty"`
	FrameRateDenominator int64     `json:"frame_rate_denominator,omitempty"`
	VideoPixelFormat     string    `json:"video_pixel_format,omitempty"`
	VideoBitRateBPS      int64     `json:"video_bitrate_bps,omitempty"`
	AudioCodec           string    `json:"audio_codec,omitempty"`
	AudioProfile         string    `json:"audio_profile,omitempty"`
	AudioBitRateBPS      int64     `json:"audio_bitrate_bps,omitempty"`
	AudioSampleRateHz    int       `json:"audio_sample_rate_hz,omitempty"`
	AudioChannels        int       `json:"audio_channels,omitempty"`
	AudioChannelLayout   string    `json:"audio_channel_layout,omitempty"`
}

type Package struct {
	PackageID          string                       `json:"package_id"`
	LayoutVersion      archive.PackageLayoutVersion `json:"layout_version"`
	Revision           uint64                       `json:"revision"`
	State              archive.PackageState         `json:"state"`
	ManifestDigest     string                       `json:"manifest_digest"`
	ArchiveDate        string                       `json:"archive_date"`
	MediaFolder        string                       `json:"media_folder"`
	DisplayName        string                       `json:"display_name"`
	VideoFileName      string                       `json:"video_file_name"`
	VideoPath          string                       `json:"video_path"`
	TranscriptPath     string                       `json:"transcript_path,omitempty"`
	TranscriptTextPath string                       `json:"transcript_text_path,omitempty"`
	SubtitlePath       string                       `json:"subtitle_path,omitempty"`
	MetadataPath       string                       `json:"metadata_path"`
	VideoInfoPath      string                       `json:"video_info_path,omitempty"`
	FilesPath          string                       `json:"files_path"`
	EventsPath         string                       `json:"events_path"`
	ManifestPath       string                       `json:"manifest_path"`
	ReadmePath         string                       `json:"readme_path"`
}

type InitialRequest struct {
	LayoutVersion    archive.PackageLayoutVersion
	PackageID        string
	WorkspaceDriveID string
	DecisionID       string
	MediaFolder      string
	Disposition      vocabulary.OrganizationDisposition
	ProfileReference string
	Sources          []SourceReference
	Video            FileSource
	CreatedAt        time.Time
}

type RevisionRequest struct {
	PackageID      string
	Transcript     FileSource
	TranscriptText FileSource
	Subtitle       FileSource
	CreatedAt      time.Time
}

type currentDocument struct {
	PackageID        string                       `json:"package_id"`
	LayoutVersion    archive.PackageLayoutVersion `json:"layout_version,omitempty"`
	Revision         uint64                       `json:"revision"`
	State            archive.PackageState         `json:"state"`
	ManifestDigest   string                       `json:"manifest_digest"`
	packageDirectory string                       `json:"-"`
}

type Metadata struct {
	PackageID          string                             `json:"package_id"`
	LayoutVersion      archive.PackageLayoutVersion       `json:"layout_version,omitempty"`
	Revision           uint64                             `json:"revision"`
	WorkspaceDriveID   string                             `json:"workspace_drive_id"`
	DecisionID         string                             `json:"organization_decision_id"`
	ArchiveDate        string                             `json:"archive_date"`
	MediaFolder        string                             `json:"media_folder"`
	DisplayName        string                             `json:"display_name"`
	VideoFileName      string                             `json:"video_file_name"`
	Disposition        vocabulary.OrganizationDisposition `json:"organization_disposition"`
	ProfileReference   string                             `json:"profile_reference"`
	Sources            []SourceReference                  `json:"sources"`
	VideoPath          string                             `json:"video_path"`
	TranscriptPath     string                             `json:"transcript_path,omitempty"`
	TranscriptTextPath string                             `json:"transcript_text_path,omitempty"`
	SubtitlePath       string                             `json:"subtitle_path,omitempty"`
	VideoInfoPath      string                             `json:"video_info_path,omitempty"`
	CreatedAt          time.Time                          `json:"created_at"`
}

type Publisher struct {
	source  *archive.StorageRoot
	archive *archive.StorageRoot
}

var (
	packageIDPattern = regexp.MustCompile(`^VP-[0-9]{8}-[A-Za-z0-9._-]+$`)
	checksumPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func NewPublisher(sourceRoot, archiveRoot *archive.StorageRoot) (*Publisher, error) {
	if sourceRoot == nil || archiveRoot == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{source: sourceRoot, archive: archiveRoot}, nil
}

func (publisher *Publisher) PublishInitial(request InitialRequest) (Package, error) {
	request.PackageID = strings.TrimSpace(request.PackageID)
	request.WorkspaceDriveID = strings.TrimSpace(request.WorkspaceDriveID)
	request.DecisionID = strings.TrimSpace(request.DecisionID)
	if !packageIDPattern.MatchString(request.PackageID) || request.WorkspaceDriveID == "" || request.DecisionID == "" ||
		len(request.Sources) == 0 || request.CreatedAt.IsZero() || !request.Disposition.Valid() ||
		request.Disposition == vocabulary.OrganizationHold || strings.TrimSpace(request.ProfileReference) == "" {
		return Package{}, ErrInvalidRequest
	}
	lock, err := publisher.acquirePackageLock(request.PackageID)
	if err != nil {
		return Package{}, err
	}
	defer lock.Close()
	sources, err := normalizeSources(request.Sources)
	if err != nil {
		return Package{}, err
	}
	originalNames := make([]string, 0, len(sources))
	for _, source := range sources {
		originalNames = append(originalNames, source.OriginalName)
	}
	presentation, err := archive.PresentPackage(request.PackageID, request.MediaFolder, originalNames)
	if err != nil {
		return Package{}, ErrInvalidRequest
	}
	if _, err := publisher.readCurrent(request.PackageID); err == nil {
		return Package{}, ErrPackageExists
	}
	metadata := Metadata{
		PackageID: request.PackageID, Revision: 1, WorkspaceDriveID: request.WorkspaceDriveID,
		DecisionID: request.DecisionID, ArchiveDate: presentation.ArchiveDate, MediaFolder: presentation.MediaFolder,
		DisplayName: presentation.DisplayName, VideoFileName: presentation.VideoFileName, Disposition: request.Disposition,
		ProfileReference: strings.TrimSpace(request.ProfileReference), Sources: sources, CreatedAt: request.CreatedAt.UTC(),
	}
	return publisher.publish(metadata, request.Video, nil, presentation.PackageDirectory)
}

func (publisher *Publisher) Revise(request RevisionRequest) (Package, error) {
	request.PackageID = strings.TrimSpace(request.PackageID)
	if !packageIDPattern.MatchString(request.PackageID) || request.CreatedAt.IsZero() {
		return Package{}, ErrInvalidRequest
	}
	lock, err := publisher.acquirePackageLock(request.PackageID)
	if err != nil {
		return Package{}, err
	}
	defer lock.Close()
	current, err := publisher.readCurrent(request.PackageID)
	if err != nil {
		return Package{}, err
	}
	if current.State != archive.PackageStored {
		return Package{}, ErrPackageState
	}
	previousDirectory := path.Join(current.packageDirectory, "revisions", fmt.Sprintf("%06d", current.Revision))
	previousMetadataPath := path.Join(previousDirectory, "metadata.json")
	payload, err := publisher.archive.ReadFile(previousMetadataPath)
	if err != nil {
		return Package{}, err
	}
	var metadata Metadata
	if json.Unmarshal(payload, &metadata) != nil || metadata.PackageID != request.PackageID || metadata.Revision != current.Revision {
		return Package{}, ErrInvalidRequest
	}
	metadata.Revision++
	metadata.CreatedAt = request.CreatedAt.UTC()
	video := FileSource{
		Root: RootArchive, Path: metadata.VideoPath,
	}
	_, video.Checksum, err = publisher.archive.FileSHA256(video.Path)
	if err != nil {
		return Package{}, err
	}
	return publisher.publish(metadata, video, map[string]FileSource{
		"transcript.json": request.Transcript, "transcript.txt": request.TranscriptText, "subtitle.srt": request.Subtitle,
	}, current.packageDirectory)
}

func (publisher *Publisher) Commit(packageID string, evidence archive.CommitEvidence) (Package, error) {
	packageID = strings.TrimSpace(packageID)
	if !packageIDPattern.MatchString(packageID) {
		return Package{}, ErrInvalidRequest
	}
	lock, err := publisher.acquirePackageLock(packageID)
	if err != nil {
		return Package{}, err
	}
	defer lock.Close()
	current, err := publisher.readCurrent(packageID)
	if err != nil {
		return Package{}, err
	}
	if current.State != archive.PackageStored || evidence.PackageID != packageID || evidence.PackageRevision != current.Revision ||
		evidence.ManifestDigest != current.ManifestDigest || archive.ValidateCommit(evidence) != nil {
		return Package{}, ErrPackageState
	}
	current.State = archive.PackageCommitted
	if err := publisher.writeCurrent(current); err != nil {
		return Package{}, err
	}
	return publisher.loadPackage(current), nil
}

func (publisher *Publisher) acquirePackageLock(packageID string) (*archive.StorageLock, error) {
	const directory = ".archive-locks/packages"
	if publisher == nil || publisher.archive == nil || !packageIDPattern.MatchString(strings.TrimSpace(packageID)) {
		return nil, ErrInvalidRequest
	}
	if err := publisher.archive.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	return publisher.archive.AcquireExclusiveLock(context.Background(), path.Join(directory, packageID+".lock"))
}

func (publisher *Publisher) Current(packageID string) (Package, error) {
	current, err := publisher.readCurrent(strings.TrimSpace(packageID))
	if err != nil {
		return Package{}, err
	}
	return publisher.loadPackage(current), nil
}

func (publisher *Publisher) publish(metadata Metadata, video FileSource, additions map[string]FileSource, packageDirectory string) (Package, error) {
	if strings.TrimSpace(packageDirectory) == "" {
		return Package{}, ErrInvalidRequest
	}
	finalDirectory := path.Join(packageDirectory, "revisions", fmt.Sprintf("%06d", metadata.Revision))
	temporaryDirectory := finalDirectory + ".partial"
	if err := publisher.archive.MkdirAll(temporaryDirectory, 0o755); err != nil {
		return Package{}, err
	}
	defer publisher.archive.RemoveAll(temporaryDirectory)
	videoFileName := strings.TrimSpace(metadata.VideoFileName)
	if videoFileName == "" {
		videoFileName = path.Base(metadata.VideoPath)
	}
	if videoFileName == "." || videoFileName == "" {
		videoFileName = "video.mp4"
	}
	videoChecksum, err := publisher.copyVerified(video, path.Join(temporaryDirectory, videoFileName))
	if err != nil {
		return Package{}, err
	}
	metadata.VideoFileName = videoFileName
	metadata.VideoPath = path.Join(finalDirectory, videoFileName)
	fileChecksums := map[string]string{videoFileName: videoChecksum}
	for _, name := range []string{"transcript.json", "transcript.txt", "subtitle.srt"} {
		source, exists := additions[name]
		if !exists {
			continue
		}
		checksum, err := publisher.copyVerified(source, path.Join(temporaryDirectory, name))
		if err != nil {
			return Package{}, err
		}
		fileChecksums[name] = checksum
		switch name {
		case "transcript.json":
			metadata.TranscriptPath = path.Join(finalDirectory, name)
		case "transcript.txt":
			metadata.TranscriptTextPath = path.Join(finalDirectory, name)
		case "subtitle.srt":
			metadata.SubtitlePath = path.Join(finalDirectory, name)
		}
	}
	metadataPayload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return Package{}, err
	}
	metadataPayload = append(metadataPayload, '\n')
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "metadata.json"), metadataPayload, 0o600); err != nil {
		return Package{}, err
	}
	fileChecksums["metadata.json"] = digest(metadataPayload)
	eventPayload := []byte(fmt.Sprintf("{\"event_type\":\"ArchivePackageRevisionStored\",\"package_id\":%q,\"revision\":%d}\n", metadata.PackageID, metadata.Revision))
	readmePayload := []byte(fmt.Sprintf("# %s\n\n- Package: `%s`\n- Archive date: %s\n- Media folder: `%s`\n- Result video: `%s`\n- Revision: %d · STORED\n",
		metadata.DisplayName, metadata.PackageID, metadata.ArchiveDate, metadata.MediaFolder, metadata.VideoFileName, metadata.Revision))
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "events.jsonl"), eventPayload, 0o600); err != nil {
		return Package{}, err
	}
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "README.md"), readmePayload, 0o600); err != nil {
		return Package{}, err
	}
	fileChecksums["events.jsonl"] = digest(eventPayload)
	fileChecksums["README.md"] = digest(readmePayload)
	filesPayload, err := filesCSV(fileChecksums)
	if err != nil {
		return Package{}, err
	}
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "files.csv"), filesPayload, 0o600); err != nil {
		return Package{}, err
	}
	fileChecksums["files.csv"] = digest(filesPayload)
	entries := make([]archive.ManifestEntry, 0, len(fileChecksums))
	for name, checksum := range fileChecksums {
		entries = append(entries, archive.ManifestEntry{Path: name, Checksum: checksum})
	}
	manifest, err := archive.BuildManifest(entries)
	if err != nil {
		return Package{}, err
	}
	manifestPayload := []byte(manifest)
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "manifest-sha256.txt"), manifestPayload, 0o600); err != nil {
		return Package{}, err
	}
	for name, checksum := range fileChecksums {
		_, actual, err := publisher.archive.FileSHA256(path.Join(temporaryDirectory, name))
		if err != nil || actual != checksum {
			return Package{}, errors.Join(ErrInvalidRequest, err)
		}
	}
	if err := publisher.archive.Rename(temporaryDirectory, finalDirectory); err != nil {
		return Package{}, err
	}
	current := currentDocument{
		PackageID: metadata.PackageID, Revision: metadata.Revision, State: archive.PackageStored,
		ManifestDigest: digest(manifestPayload), packageDirectory: packageDirectory,
	}
	if err := publisher.writeCurrent(current); err != nil {
		return Package{}, err
	}
	return publisher.loadPackage(current), nil
}

func (publisher *Publisher) copyVerified(source FileSource, target string) (string, error) {
	source.Path = normalizePath(source.Path)
	source.Checksum = strings.ToLower(strings.TrimSpace(source.Checksum))
	if source.Path == "" || !checksumPattern.MatchString(source.Checksum) {
		return "", ErrInvalidRequest
	}
	var root *archive.StorageRoot
	switch source.Root {
	case RootSource:
		root = publisher.source
	case RootArchive:
		root = publisher.archive
	default:
		return "", ErrInvalidRequest
	}
	_, checksum, err := publisher.archive.CopyFileFrom(root, source.Path, target, 0o600)
	if err != nil || checksum != source.Checksum {
		return "", errors.Join(ErrInvalidRequest, err)
	}
	return checksum, nil
}

func (publisher *Publisher) readCurrent(packageID string) (currentDocument, error) {
	directory, pathErr := archive.PackageDirectory(packageID)
	if pathErr != nil {
		return currentDocument{}, ErrInvalidRequest
	}
	directories := []string{directory, path.Join("packages", packageID)}
	if entries, readErr := publisher.archive.ReadDir("."); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "packages" || entry.Name() == "catalog" {
				continue
			}
			directories = append(directories, entry.Name())
		}
	}
	var payload []byte
	var err error
	selected := ""
	for _, candidate := range directories {
		payload, err = publisher.archive.ReadFile(path.Join(candidate, "current.json"))
		if err == nil {
			var observed currentDocument
			if json.Unmarshal(payload, &observed) == nil && observed.PackageID == packageID {
				selected = candidate
				break
			}
		}
	}
	if selected == "" {
		if err == nil {
			err = ErrInvalidRequest
		}
		return currentDocument{}, err
	}
	var current currentDocument
	if json.Unmarshal(payload, &current) != nil || current.PackageID != packageID || current.Revision == 0 ||
		!current.State.Valid() || !checksumPattern.MatchString(current.ManifestDigest) {
		return currentDocument{}, ErrInvalidRequest
	}
	if current.LayoutVersion == 0 {
		current.LayoutVersion = archive.PackageLayoutDateRevisionTree
	}
	if !current.LayoutVersion.Valid() {
		return currentDocument{}, ErrInvalidRequest
	}
	current.packageDirectory = selected
	return current, nil
}

func (publisher *Publisher) writeCurrent(current currentDocument) error {
	directory := current.packageDirectory
	if directory == "" {
		var err error
		directory, err = archive.PackageDirectory(current.PackageID)
		if err != nil {
			return ErrInvalidRequest
		}
		current.packageDirectory = directory
	}
	if err := publisher.archive.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(current)
	if err != nil {
		return err
	}
	temporary := path.Join(directory, "current.json.partial")
	defer publisher.archive.Remove(temporary)
	if err := publisher.archive.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	verified, err := publisher.archive.ReadFile(temporary)
	if err != nil || !bytes.Equal(verified, payload) {
		return errors.Join(ErrInvalidRequest, err)
	}
	return publisher.archive.Rename(temporary, path.Join(directory, "current.json"))
}

func (publisher *Publisher) loadPackage(current currentDocument) Package {
	directory := current.packageDirectory
	if directory == "" {
		var err error
		directory, err = archive.PackageDirectory(current.PackageID)
		if err != nil {
			return Package{}
		}
	}
	metadataPath := path.Join(directory, "metadata.json")
	filesPath := path.Join(directory, "files.csv")
	eventsPath := path.Join(directory, "events.jsonl")
	manifestPath := path.Join(directory, "manifest-sha256.txt")
	readmePath := path.Join(directory, "README.md")
	if current.LayoutVersion != archive.PackageLayoutFlatResultFolder {
		directory = path.Join(directory, "revisions", fmt.Sprintf("%06d", current.Revision))
		metadataPath = path.Join(directory, "metadata.json")
		filesPath = path.Join(directory, "files.csv")
		eventsPath = path.Join(directory, "events.jsonl")
		manifestPath = path.Join(directory, "manifest-sha256.txt")
		readmePath = path.Join(directory, "README.md")
	}
	result := Package{
		PackageID: current.PackageID, LayoutVersion: current.LayoutVersion, Revision: current.Revision, State: current.State, ManifestDigest: current.ManifestDigest,
		MetadataPath: metadataPath,
		FilesPath:    filesPath, EventsPath: eventsPath,
		ManifestPath: manifestPath, ReadmePath: readmePath,
	}
	if payload, err := publisher.archive.ReadFile(result.MetadataPath); err == nil {
		var metadata Metadata
		if json.Unmarshal(payload, &metadata) == nil {
			result.ArchiveDate = metadata.ArchiveDate
			result.MediaFolder = metadata.MediaFolder
			result.DisplayName = metadata.DisplayName
			result.VideoFileName = metadata.VideoFileName
			result.VideoPath = metadata.VideoPath
			result.TranscriptPath = metadata.TranscriptPath
			result.TranscriptTextPath = metadata.TranscriptTextPath
			result.SubtitlePath = metadata.SubtitlePath
			result.VideoInfoPath = metadata.VideoInfoPath
		}
	}
	return result
}

func normalizeSources(source []SourceReference) ([]SourceReference, error) {
	result := append([]SourceReference(nil), source...)
	seen := make(map[string]bool, len(result))
	for index := range result {
		item := &result[index]
		item.OriginalAssetID = strings.TrimSpace(item.OriginalAssetID)
		item.OriginalName = strings.TrimSpace(item.OriginalName)
		item.RelativePath = normalizePath(item.RelativePath)
		item.Checksum = strings.ToLower(strings.TrimSpace(item.Checksum))
		if !item.SourceCreatedAt.IsZero() {
			item.SourceCreatedAt = item.SourceCreatedAt.UTC()
		}
		item.Container = strings.ToLower(strings.TrimSpace(item.Container))
		item.VideoCodec = strings.ToLower(strings.TrimSpace(item.VideoCodec))
		item.VideoProfile = strings.TrimSpace(item.VideoProfile)
		item.VideoPixelFormat = strings.ToLower(strings.TrimSpace(item.VideoPixelFormat))
		item.AudioCodec = strings.ToLower(strings.TrimSpace(item.AudioCodec))
		item.AudioProfile = strings.TrimSpace(item.AudioProfile)
		item.AudioChannelLayout = strings.ToLower(strings.TrimSpace(item.AudioChannelLayout))
		if item.OriginalAssetID == "" || item.OriginalName == "" || item.RelativePath == "" ||
			!checksumPattern.MatchString(item.Checksum) || item.DurationMillis < 0 || item.SizeBytes < 0 || item.BitRateBPS < 0 ||
			item.VideoBitRateBPS < 0 || item.AudioBitRateBPS < 0 || item.AudioSampleRateHz < 0 || item.AudioChannels < 0 ||
			item.FrameRateNumerator < 0 || item.FrameRateDenominator < 0 ||
			((item.FrameRateNumerator == 0) != (item.FrameRateDenominator == 0)) || seen[item.OriginalAssetID] {
			return nil, ErrInvalidRequest
		}
		seen[item.OriginalAssetID] = true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OriginalAssetID < result[j].OriginalAssetID })
	return result, nil
}

func filesCSV(checksums map[string]string) ([]byte, error) {
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write([]string{"path", "sha256"}); err != nil {
		return nil, err
	}
	for _, name := range names {
		if err := writer.Write([]string{name, checksums[name]}); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return buffer.Bytes(), writer.Error()
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

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
