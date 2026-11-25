package archivebundle

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
)

const FlatLayoutVersion = archive.PackageLayoutFlatResultFolder

var videoInfoHeader = []string{
	"source_created_at", "source_name", "source_relative_path", "source_checksum",
	"source_duration_ms", "source_size_bytes", "source_container", "source_bitrate_bps",
	"source_video_codec", "source_video_profile", "source_width", "source_height", "source_frame_rate",
	"source_video_pixel_format", "source_video_bitrate_bps", "source_audio_codec", "source_audio_profile",
	"source_audio_bitrate_bps", "source_audio_sample_rate_hz", "source_audio_channels", "source_audio_channel_layout",
	"organization_decision_id", "original_asset_id",
}

func (publisher *Publisher) PublishInitialV2(request InitialRequest) (Package, error) {
	request.PackageID = strings.TrimSpace(request.PackageID)
	request.WorkspaceDriveID = strings.TrimSpace(request.WorkspaceDriveID)
	request.DecisionID = strings.TrimSpace(request.DecisionID)
	if request.LayoutVersion != FlatLayoutVersion || !packageIDPattern.MatchString(request.PackageID) ||
		request.WorkspaceDriveID == "" || request.DecisionID == "" ||
		len(request.Sources) == 0 || request.CreatedAt.IsZero() || !request.Disposition.ProducesMedia() || strings.TrimSpace(request.ProfileReference) == "" {
		return Package{}, ErrInvalidRequest
	}
	lock, err := publisher.acquirePackageLock(request.PackageID)
	if err != nil {
		return Package{}, err
	}
	defer lock.Close()
	if _, err := publisher.readCurrent(request.PackageID); err == nil {
		return Package{}, ErrPackageExists
	}
	sources, err := normalizeSources(request.Sources)
	if err != nil {
		return Package{}, err
	}
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.OriginalName)
	}
	presentation, err := archive.PresentFlatPackage(request.PackageID, request.MediaFolder, names)
	if err != nil {
		return Package{}, ErrInvalidRequest
	}
	exists, err := publisher.archive.Exists(presentation.PackageDirectory)
	if err != nil {
		return Package{}, err
	}
	if exists {
		return Package{}, ErrPackageExists
	}
	metadata := Metadata{
		PackageID: request.PackageID, LayoutVersion: FlatLayoutVersion, Revision: 1,
		WorkspaceDriveID: request.WorkspaceDriveID, DecisionID: request.DecisionID,
		ArchiveDate: presentation.ArchiveDate, MediaFolder: presentation.MediaFolder,
		DisplayName: presentation.DisplayName, VideoFileName: presentation.VideoFileName,
		Disposition: request.Disposition, ProfileReference: strings.TrimSpace(request.ProfileReference),
		Sources: sources, CreatedAt: request.CreatedAt.UTC(),
	}
	return publisher.publishInitialV2(metadata, request.Video, presentation.PackageDirectory)
}

func (publisher *Publisher) ReviseV2(request RevisionRequest) (Package, error) {
	request.PackageID = strings.TrimSpace(request.PackageID)
	if !packageIDPattern.MatchString(request.PackageID) || request.BaseRevision == 0 || request.CreatedAt.IsZero() {
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
	if current.LayoutVersion != FlatLayoutVersion || current.State != archive.PackageStored {
		return Package{}, ErrPackageState
	}
	if replayed, ok, replayErr := publisher.replayedStoredRevision(current, request); replayErr != nil || ok {
		return replayed, replayErr
	}
	metadataPayload, err := publisher.archive.ReadFile(path.Join(current.packageDirectory, revisionFileName(current.Revision, "metadata.json")))
	if err != nil {
		return Package{}, err
	}
	var metadata Metadata
	if json.Unmarshal(metadataPayload, &metadata) != nil || metadata.PackageID != request.PackageID ||
		metadata.LayoutVersion != FlatLayoutVersion || metadata.Revision != current.Revision {
		return Package{}, ErrInvalidRequest
	}
	metadata.Revision++
	metadata.CreatedAt = request.CreatedAt.UTC()
	// The first immutable write fixes the revision time across Worker attempts.
	// All pending bytes must still match the regenerated revision before publication.
	pending, err := publisher.archive.ReadFile(path.Join(current.packageDirectory, revisionFileName(metadata.Revision, "metadata.json")))
	resuming := err == nil
	if resuming {
		var staged Metadata
		if json.Unmarshal(pending, &staged) != nil || staged.CreatedAt.IsZero() {
			return Package{}, ErrPackageExists
		}
		metadata.CreatedAt = staged.CreatedAt
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Package{}, err
	}
	return publisher.publishRevisionV2(current, metadata, map[string]FileSource{
		"transcript.json": request.Transcript,
		"transcript.txt":  request.TranscriptText,
		"subtitle.srt":    request.Subtitle,
	}, resuming)
}

func (publisher *Publisher) publishInitialV2(metadata Metadata, video FileSource, packageDirectory string) (Package, error) {
	temporaryDirectory := packageDirectory + ".partial"
	_ = publisher.archive.RemoveAll(temporaryDirectory)
	if err := publisher.archive.MkdirAll(temporaryDirectory, 0o755); err != nil {
		return Package{}, err
	}
	defer publisher.archive.RemoveAll(temporaryDirectory)
	videoChecksum, err := publisher.copyVerified(video, path.Join(temporaryDirectory, metadata.VideoFileName))
	if err != nil {
		return Package{}, err
	}
	metadata.VideoPath = path.Join(packageDirectory, metadata.VideoFileName)
	metadata.VideoInfoPath = path.Join(packageDirectory, "video-info.csv")
	payloads, checksums, manifestDigest, err := publisher.v2Payloads(metadata, map[string]string{metadata.VideoFileName: videoChecksum}, nil)
	if err != nil {
		return Package{}, err
	}
	for name, payload := range payloads {
		if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, name), payload, 0o600); err != nil {
			return Package{}, err
		}
	}
	for name, payload := range revisionPayloads(metadata.Revision, payloads) {
		if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, name), payload, 0o600); err != nil {
			return Package{}, err
		}
	}
	current := currentDocument{
		PackageID: metadata.PackageID, LayoutVersion: FlatLayoutVersion, Revision: metadata.Revision,
		State: archive.PackageStored, ManifestDigest: manifestDigest, packageDirectory: packageDirectory,
	}
	currentPayload, err := json.Marshal(current)
	if err != nil {
		return Package{}, err
	}
	if err := publisher.archive.WriteFile(path.Join(temporaryDirectory, "current.json"), currentPayload, 0o600); err != nil {
		return Package{}, err
	}
	for name, checksum := range checksums {
		_, actual, verifyErr := publisher.archive.FileSHA256(path.Join(temporaryDirectory, name))
		if verifyErr != nil || actual != checksum {
			return Package{}, errors.Join(ErrInvalidRequest, verifyErr)
		}
	}
	entries, err := publisher.archive.ReadDir(temporaryDirectory)
	if err != nil {
		return Package{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return Package{}, ErrInvalidRequest
		}
	}
	if err := publisher.archive.Rename(temporaryDirectory, packageDirectory); err != nil {
		return Package{}, err
	}
	return publisher.loadPackage(current), nil
}

func (publisher *Publisher) publishRevisionV2(current currentDocument, metadata Metadata, additions map[string]FileSource, resuming bool) (Package, error) {
	directory := current.packageDirectory
	videoPath := path.Join(directory, metadata.VideoFileName)
	_, videoChecksum, err := publisher.archive.FileSHA256(videoPath)
	if err != nil {
		return Package{}, err
	}
	additionPayloads := make(map[string][]byte, len(additions))
	additionChecksums := make(map[string]string, len(additions))
	for _, name := range []string{"transcript.json", "transcript.txt", "subtitle.srt"} {
		source, ok := additions[name]
		if !ok {
			return Package{}, ErrInvalidRequest
		}
		payload, checksum, readErr := publisher.readVerifiedSource(source)
		if readErr != nil {
			return Package{}, readErr
		}
		additionPayloads[name] = payload
		additionChecksums[name] = checksum
	}
	metadata.VideoPath = videoPath
	metadata.VideoInfoPath = path.Join(directory, "video-info.csv")
	metadata.TranscriptPath = path.Join(directory, "transcript.json")
	metadata.TranscriptTextPath = path.Join(directory, "transcript.txt")
	metadata.SubtitlePath = path.Join(directory, "subtitle.srt")
	baseChecksums := map[string]string{metadata.VideoFileName: videoChecksum}
	for name, checksum := range additionChecksums {
		baseChecksums[name] = checksum
	}
	previousEvents, err := publisher.archive.ReadFile(path.Join(directory, revisionFileName(current.Revision, "events.jsonl")))
	if err != nil {
		return Package{}, err
	}
	payloads, _, manifestDigest, err := publisher.v2Payloads(metadata, baseChecksums, previousEvents)
	if err != nil {
		return Package{}, err
	}
	for name, payload := range additionPayloads {
		payloads[name] = payload
	}
	if err := publisher.stageRevisionV2(directory, metadata.Revision, payloads); err != nil {
		return Package{}, err
	}
	publicNames := make([]string, 0, len(payloads))
	for name := range payloads {
		if name == metadata.VideoFileName {
			continue
		}
		publicNames = append(publicNames, name)
	}
	sort.Strings(publicNames)
	for _, name := range publicNames {
		target := path.Join(directory, name)
		if resuming {
			published, err := publisher.archive.ReadFile(target)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return Package{}, err
			}
			if err == nil && bytes.Equal(published, payloads[name]) {
				continue
			}
		}
		if err := publisher.writeCurrentFile(target, payloads[name]); err != nil {
			return Package{}, err
		}
	}
	next := currentDocument{
		PackageID: metadata.PackageID, LayoutVersion: FlatLayoutVersion, Revision: metadata.Revision,
		State: archive.PackageStored, ManifestDigest: manifestDigest, packageDirectory: directory,
	}
	if err := publisher.writeCurrent(next); err != nil {
		return Package{}, err
	}
	return publisher.loadPackage(next), nil
}

func (publisher *Publisher) v2Payloads(metadata Metadata, fileChecksums map[string]string, previousEvents []byte) (map[string][]byte, map[string]string, string, error) {
	videoInfo, err := videoInfoCSV(metadata.DecisionID, metadata.Sources)
	if err != nil {
		return nil, nil, "", err
	}
	metadataPayload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, nil, "", err
	}
	metadataPayload = append(metadataPayload, '\n')
	event := []byte(fmt.Sprintf("{\"event_type\":\"ArchivePackageRevisionStored\",\"package_id\":%q,\"revision\":%d}\n", metadata.PackageID, metadata.Revision))
	eventsPayload := append(append([]byte(nil), previousEvents...), event...)
	readmePayload := []byte(fmt.Sprintf("# %s\n\n- Package: `%s`\n- Layout: 2\n- Archive date: %s\n- Media folder: `%s`\n- Result video: `%s`\n- Revision: %d · STORED\n",
		metadata.DisplayName, metadata.PackageID, metadata.ArchiveDate, metadata.MediaFolder, metadata.VideoFileName, metadata.Revision))
	payloads := map[string][]byte{
		"metadata.json": metadataPayload, "video-info.csv": videoInfo,
		"events.jsonl": eventsPayload, "README.md": readmePayload,
	}
	checksums := make(map[string]string, len(fileChecksums)+len(payloads)+2)
	for name, checksum := range fileChecksums {
		checksums[name] = checksum
	}
	for name, payload := range payloads {
		checksums[name] = digest(payload)
	}
	filesPayload, err := filesCSV(checksums)
	if err != nil {
		return nil, nil, "", err
	}
	payloads["files.csv"] = filesPayload
	checksums["files.csv"] = digest(filesPayload)
	entries := make([]archive.ManifestEntry, 0, len(checksums))
	for name, checksum := range checksums {
		entries = append(entries, archive.ManifestEntry{Path: name, Checksum: checksum})
	}
	manifest, err := archive.BuildManifest(entries)
	if err != nil {
		return nil, nil, "", err
	}
	manifestPayload := []byte(manifest)
	payloads["manifest-sha256.txt"] = manifestPayload
	checksums["manifest-sha256.txt"] = digest(manifestPayload)
	return payloads, checksums, digest(manifestPayload), nil
}

func (publisher *Publisher) readVerifiedSource(source FileSource) ([]byte, string, error) {
	source.Path = normalizePath(source.Path)
	source.Checksum = strings.ToLower(strings.TrimSpace(source.Checksum))
	if source.Path == "" || !checksumPattern.MatchString(source.Checksum) {
		return nil, "", ErrInvalidRequest
	}
	var root *archive.StorageRoot
	switch source.Root {
	case RootSource:
		root = publisher.source
	case RootArchive:
		root = publisher.archive
	default:
		return nil, "", ErrInvalidRequest
	}
	payload, err := root.ReadFile(source.Path)
	if err != nil || digest(payload) != source.Checksum {
		return nil, "", errors.Join(ErrInvalidRequest, err)
	}
	return payload, source.Checksum, nil
}

func (publisher *Publisher) stageRevisionV2(directory string, revision uint64, payloads map[string][]byte) error {
	// Check every surviving immutable file before adding any missing ones.
	// Older publishers staged the files in unspecified order.
	names := []string{"metadata.json", "manifest-sha256.txt"}
	for name := range payloads {
		if name != "metadata.json" && name != "manifest-sha256.txt" {
			names = append(names, name)
		}
	}
	var missing []string
	for _, name := range names {
		target := path.Join(directory, revisionFileName(revision, name))
		current, err := publisher.archive.ReadFile(target)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, name)
		case err != nil:
			return err
		case !bytes.Equal(current, payloads[name]):
			return ErrPackageExists
		}
	}
	for _, name := range missing {
		if err := publisher.writeCurrentFile(path.Join(directory, revisionFileName(revision, name)), payloads[name]); err != nil {
			return err
		}
	}
	return nil
}

func (publisher *Publisher) writeCurrentFile(target string, payload []byte) error {
	temporary := target + ".partial"
	defer publisher.archive.Remove(temporary)
	if err := publisher.archive.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	verified, err := publisher.archive.ReadFile(temporary)
	if err != nil || !bytes.Equal(verified, payload) {
		return errors.Join(ErrInvalidRequest, err)
	}
	return publisher.archive.Rename(temporary, target)
}

func revisionPayloads(revision uint64, payloads map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(payloads))
	for name, payload := range payloads {
		result[revisionFileName(revision, name)] = payload
	}
	return result
}

func revisionFileName(revision uint64, name string) string {
	return fmt.Sprintf(".r%06d.%s", revision, name)
}

func videoInfoCSV(decisionID string, sources []SourceReference) ([]byte, error) {
	decisionID = strings.TrimSpace(decisionID)
	if decisionID == "" || len(sources) == 0 {
		return nil, ErrInvalidRequest
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write(videoInfoHeader); err != nil {
		return nil, err
	}
	for _, source := range sources {
		createdAt := ""
		if !source.SourceCreatedAt.IsZero() {
			createdAt = source.SourceCreatedAt.UTC().Format(time.RFC3339Nano)
		}
		frameRate := ""
		if source.FrameRateNumerator > 0 && source.FrameRateDenominator > 0 {
			frameRate = strconv.FormatInt(source.FrameRateNumerator, 10) + "/" + strconv.FormatInt(source.FrameRateDenominator, 10)
		}
		record := []string{
			createdAt, source.OriginalName, source.RelativePath, source.Checksum,
			strconv.FormatInt(source.DurationMillis, 10), strconv.FormatInt(source.SizeBytes, 10), source.Container,
			strconv.FormatInt(source.BitRateBPS, 10), source.VideoCodec, source.VideoProfile,
			strconv.Itoa(source.Width), strconv.Itoa(source.Height), frameRate, source.VideoPixelFormat,
			strconv.FormatInt(source.VideoBitRateBPS, 10), source.AudioCodec, source.AudioProfile,
			strconv.FormatInt(source.AudioBitRateBPS, 10), strconv.Itoa(source.AudioSampleRateHz),
			strconv.Itoa(source.AudioChannels), source.AudioChannelLayout, decisionID, source.OriginalAssetID,
		}
		if err := writer.Write(record); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func ParseVideoInfoCount(payload []byte) (int, error) {
	records, err := parseVideoInfo(payload)
	return len(records), err
}

func parseVideoInfo(payload []byte) ([][]string, error) {
	reader := csv.NewReader(bytes.NewReader(payload))
	records, err := reader.ReadAll()
	if err != nil || len(records) < 2 || !equalStringSlices(records[0], videoInfoHeader) {
		return nil, ErrInvalidRequest
	}
	seen := make(map[string]bool, len(records)-1)
	for _, record := range records[1:] {
		if len(record) != len(videoInfoHeader) || strings.TrimSpace(record[1]) == "" ||
			!checksumPattern.MatchString(strings.TrimSpace(record[3])) || strings.TrimSpace(record[22]) == "" || seen[record[22]] {
			return nil, ErrInvalidRequest
		}
		seen[record[22]] = true
	}
	return records[1:], nil
}

func verifyVideoInfo(metadata Metadata, payload []byte) error {
	rows, err := parseVideoInfo(payload)
	if err != nil || len(rows) != len(metadata.Sources) {
		return fmt.Errorf("video-info.csv source records: %w", ErrInvalidRequest)
	}
	expected, err := videoInfoCSV(metadata.DecisionID, metadata.Sources)
	if err != nil || bytes.Equal(payload, expected) {
		return err
	}
	// Alternate CSV quoting or line endings can represent the same records.
	want, err := parseVideoInfo(expected)
	if err != nil {
		return fmt.Errorf("invalid recovery document sources: %w", err)
	}
	for i, row := range rows {
		for j, value := range row {
			if value != want[i][j] {
				return &SourceMetadataConflict{PackageID: metadata.PackageID, Row: i + 1, Column: videoInfoHeader[j]}
			}
		}
	}
	return nil
}

// SourceMetadataConflict identifies a conflicting CSV field without disclosing
// source paths or values in Worker failure evidence.
type SourceMetadataConflict struct {
	PackageID string
	Row       int
	Column    string
}

func (conflict *SourceMetadataConflict) Error() string {
	return fmt.Sprintf("video-info.csv source %d field %s disagrees with recovery document", conflict.Row, conflict.Column)
}

func (*SourceMetadataConflict) Unwrap() error { return ErrInvalidRequest }

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
