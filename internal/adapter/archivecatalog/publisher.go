package archivecatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest = errors.New("invalid archive catalog publish request")
	ErrPublishFailed  = errors.New("archive catalog publish failed")
)

type Request struct {
	WorkspaceDriveID string
	Rows             []organization.ArchiveCatalogRow
	TemporaryPath    string
	PublishedPath    string
}

type Publisher struct {
	root          *archive.StorageRoot
	beforeReplace func() error
}

var checksumPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

var legacyHeader = []string{
	"workspace_drive_id", "media_folder", "original_asset_id", "source_name", "source_relative_path", "source_checksum",
	"source_duration_ms", "source_video_codec", "source_width", "source_height", "source_video_bitrate_bps",
	"organization_disposition", "organization_decision_id", "archive_package_id", "output_media_id", "output_video_path", "output_profile",
	"transcript_path", "transcript_text_path", "subtitle_path", "status", "hold_reason",
}

var header = []string{
	"source_created_at", "source_name", "archive_date", "output_video_name",
	"media_folder", "source_relative_path", "status", "organization_disposition", "hold_reason",
	"source_duration_ms", "source_size_bytes", "source_container", "source_bitrate_bps",
	"source_video_codec", "source_video_profile", "source_width", "source_height", "source_frame_rate",
	"source_video_pixel_format", "source_video_bitrate_bps",
	"source_audio_codec", "source_audio_profile", "source_audio_bitrate_bps", "source_audio_sample_rate_hz",
	"source_audio_channels", "source_audio_channel_layout",
	"output_profile", "output_video_path", "transcript_path", "transcript_text_path", "subtitle_path",
	"workspace_drive_id", "original_asset_id", "source_checksum", "organization_decision_id", "archive_package_id", "output_media_id",
}

func NewPublisher(root *archive.StorageRoot) (*Publisher, error) {
	if root == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{root: root}, nil
}

func (publisher *Publisher) Publish(request Request) (organization.ArchiveCatalog, error) {
	return publisher.publish(request, true)
}

func (publisher *Publisher) publish(request Request, requireTriggerDrive bool) (organization.ArchiveCatalog, error) {
	workspaceDriveID := strings.TrimSpace(request.WorkspaceDriveID)
	temporaryPath, temporaryOK := normalizePath(request.TemporaryPath)
	publishedPath, publishedOK := normalizePath(request.PublishedPath)
	if workspaceDriveID == "" || len(request.Rows) == 0 || !temporaryOK || !publishedOK || temporaryPath == publishedPath ||
		path.Dir(temporaryPath) != path.Dir(publishedPath) {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	rows := append([]organization.ArchiveCatalogRow(nil), request.Rows...)
	for index := range rows {
		if !normalizeRow(&rows[index]) || (requireTriggerDrive && rows[index].WorkspaceDriveID != workspaceDriveID) {
			return organization.ArchiveCatalog{}, ErrInvalidRequest
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].SourceCreatedAt.Equal(rows[j].SourceCreatedAt) {
			if rows[i].SourceCreatedAt.IsZero() {
				return false
			}
			if rows[j].SourceCreatedAt.IsZero() {
				return true
			}
			return rows[i].SourceCreatedAt.Before(rows[j].SourceCreatedAt)
		}
		if rows[i].SourceName != rows[j].SourceName {
			return rows[i].SourceName < rows[j].SourceName
		}
		if rows[i].WorkspaceDriveID != rows[j].WorkspaceDriveID {
			return rows[i].WorkspaceDriveID < rows[j].WorkspaceDriveID
		}
		if rows[i].MediaFolder != rows[j].MediaFolder {
			return rows[i].MediaFolder < rows[j].MediaFolder
		}
		return rows[i].OriginalAssetID < rows[j].OriginalAssetID
	})
	records := make([][]string, 0, len(rows)+1)
	records = append(records, append([]string(nil), header...))
	for _, row := range rows {
		records = append(records, rowRecord(row))
	}
	var payload bytes.Buffer
	writer := csv.NewWriter(&payload)
	writer.UseCRLF = true
	if err := writer.WriteAll(records); err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	if directory := path.Dir(publishedPath); directory != "." {
		if err := publisher.root.MkdirAll(directory, 0o755); err != nil {
			return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
		}
	}
	defer publisher.root.Remove(temporaryPath)
	if err := publisher.root.WriteFile(temporaryPath, payload.Bytes(), 0o600); err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	verified, err := publisher.root.ReadFile(temporaryPath)
	if err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	reader := csv.NewReader(bytes.NewReader(verified))
	reader.FieldsPerRecord = len(header)
	parsed, err := reader.ReadAll()
	if err != nil || !equalRecords(parsed, records) {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	if err := publisher.root.Rename(temporaryPath, publishedPath); err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	digest := sha256.Sum256(verified)
	return organization.ArchiveCatalog{
		WorkspaceDriveID: workspaceDriveID, Path: publishedPath, RowCount: len(rows), Checksum: hex.EncodeToString(digest[:]),
	}, nil
}

// PublishMerged reads the last verified current projection, replaces rows for
// the supplied source-drive/OriginalAsset pairs, and republishes the complete
// archive-root CSV atomically. The StorageRoot lock prevents concurrent Jobs
// in different Worker processes from losing each other's rows.
func (publisher *Publisher) PublishMerged(request Request) (organization.ArchiveCatalog, error) {
	request.WorkspaceDriveID = strings.TrimSpace(request.WorkspaceDriveID)
	publishedPath, publishedOK := normalizePath(request.PublishedPath)
	if request.WorkspaceDriveID == "" || len(request.Rows) == 0 || !publishedOK {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	for index := range request.Rows {
		row := request.Rows[index]
		if !normalizeRow(&row) || row.WorkspaceDriveID != request.WorkspaceDriveID {
			return organization.ArchiveCatalog{}, ErrInvalidRequest
		}
	}
	directory := path.Dir(publishedPath)
	if directory != "." {
		if err := publisher.root.MkdirAll(directory, 0o755); err != nil {
			return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
		}
	}
	lockPath := path.Join(directory, "."+path.Base(publishedPath)+".lock")
	lock, err := publisher.root.AcquireExclusiveLock(context.Background(), lockPath)
	if err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	defer lock.Close()

	existing, err := publisher.currentRows(publishedPath)
	if err != nil {
		return organization.ArchiveCatalog{}, err
	}
	merged := make(map[string]organization.ArchiveCatalogRow, len(existing)+len(request.Rows))
	for _, row := range existing {
		merged[catalogRowKey(row)] = row
	}
	for _, row := range request.Rows {
		merged[catalogRowKey(row)] = row
	}
	request.Rows = request.Rows[:0]
	for _, row := range merged {
		request.Rows = append(request.Rows, row)
	}
	return publisher.publish(request, false)
}

func (publisher *Publisher) currentRows(publishedPath string) ([]organization.ArchiveCatalogRow, error) {
	exists, err := publisher.root.Exists(publishedPath)
	if err != nil {
		return nil, errors.Join(ErrPublishFailed, err)
	}
	if exists {
		payload, readErr := publisher.root.ReadFile(publishedPath)
		if readErr != nil {
			return nil, errors.Join(ErrPublishFailed, readErr)
		}
		return parseRows(payload)
	}

	legacyDirectoryExists, err := publisher.root.Exists("catalog")
	if err != nil || !legacyDirectoryExists {
		return nil, err
	}
	entries, err := publisher.root.ReadDir("catalog")
	if err != nil {
		return nil, errors.Join(ErrPublishFailed, err)
	}
	legacyRows := make([]organization.ArchiveCatalogRow, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".csv") {
			continue
		}
		payload, readErr := publisher.root.ReadFile(path.Join("catalog", entry.Name()))
		if readErr != nil {
			return nil, errors.Join(ErrPublishFailed, readErr)
		}
		rows, parseErr := parseRows(payload)
		if parseErr != nil {
			return nil, parseErr
		}
		legacyRows = append(legacyRows, rows...)
	}
	return legacyRows, nil
}

func parseRows(payload []byte) ([]organization.ArchiveCatalogRow, error) {
	reader := csv.NewReader(bytes.NewReader(payload))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return nil, errors.Join(ErrPublishFailed, err)
	}
	if equalStrings(records[0], legacyHeader) {
		return parseLegacyRows(records[1:])
	}
	if !equalStrings(records[0], header) {
		return nil, ErrPublishFailed
	}
	rows := make([]organization.ArchiveCatalogRow, 0, len(records)-1)
	for _, record := range records[1:] {
		if len(record) != len(header) {
			return nil, ErrPublishFailed
		}
		createdAt, createdErr := parseOptionalTime(record[0])
		duration, durationErr := strconv.ParseInt(record[9], 10, 64)
		size, sizeErr := strconv.ParseInt(record[10], 10, 64)
		containerBitRate, containerBitRateErr := strconv.ParseInt(record[12], 10, 64)
		width, widthErr := strconv.Atoi(record[15])
		height, heightErr := strconv.Atoi(record[16])
		frameRateNumerator, frameRateDenominator, frameRateErr := parseFrameRate(record[17])
		videoBitRate, videoBitRateErr := strconv.ParseInt(record[19], 10, 64)
		audioBitRate, audioBitRateErr := strconv.ParseInt(record[22], 10, 64)
		audioSampleRate, audioSampleRateErr := strconv.Atoi(record[23])
		audioChannels, audioChannelsErr := strconv.Atoi(record[24])
		if createdErr != nil || durationErr != nil || sizeErr != nil || containerBitRateErr != nil || widthErr != nil ||
			heightErr != nil || frameRateErr != nil || videoBitRateErr != nil || audioBitRateErr != nil ||
			audioSampleRateErr != nil || audioChannelsErr != nil {
			return nil, ErrPublishFailed
		}
		row := organization.ArchiveCatalogRow{
			SourceCreatedAt: createdAt, SourceName: record[1], ArchiveDate: record[2], OutputVideoName: record[3],
			MediaFolder: record[4], SourceRelativePath: record[5], Status: record[6],
			Disposition: vocabulary.OrganizationDisposition(record[7]), HoldReason: record[8],
			SourceDurationMillis: duration, SourceSizeBytes: size, SourceContainer: record[11], SourceBitRateBPS: containerBitRate,
			SourceVideoCodec: record[13], SourceVideoProfile: record[14], SourceWidth: width, SourceHeight: height,
			SourceFrameRateNumerator: frameRateNumerator, SourceFrameRateDenominator: frameRateDenominator,
			SourceVideoPixelFormat: record[18], SourceVideoBitRateBPS: videoBitRate,
			SourceAudioCodec: record[20], SourceAudioProfile: record[21], SourceAudioBitRateBPS: audioBitRate,
			SourceAudioSampleRateHz: audioSampleRate, SourceAudioChannels: audioChannels, SourceAudioChannelLayout: record[25],
			OutputProfile: record[26], OutputVideoPath: record[27], TranscriptPath: record[28],
			TranscriptTextPath: record[29], SubtitlePath: record[30], WorkspaceDriveID: record[31],
			OriginalAssetID: record[32], SourceChecksum: record[33], DecisionID: record[34],
			ArchivePackageID: record[35], OutputMediaID: record[36],
		}
		if !normalizeRow(&row) {
			return nil, ErrPublishFailed
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseLegacyRows(records [][]string) ([]organization.ArchiveCatalogRow, error) {
	rows := make([]organization.ArchiveCatalogRow, 0, len(records))
	for _, record := range records {
		if len(record) != len(legacyHeader) {
			return nil, ErrPublishFailed
		}
		duration, durationErr := strconv.ParseInt(record[6], 10, 64)
		width, widthErr := strconv.Atoi(record[8])
		height, heightErr := strconv.Atoi(record[9])
		bitRate, bitRateErr := strconv.ParseInt(record[10], 10, 64)
		if durationErr != nil || widthErr != nil || heightErr != nil || bitRateErr != nil {
			return nil, ErrPublishFailed
		}
		row := organization.ArchiveCatalogRow{
			WorkspaceDriveID: record[0], MediaFolder: record[1], OriginalAssetID: record[2], SourceName: record[3],
			SourceRelativePath: record[4], SourceChecksum: record[5], SourceDurationMillis: duration,
			SourceVideoCodec: record[7], SourceWidth: width, SourceHeight: height, SourceVideoBitRateBPS: bitRate,
			Disposition: vocabulary.OrganizationDisposition(record[11]), DecisionID: record[12], ArchivePackageID: record[13],
			OutputMediaID: record[14], OutputVideoPath: record[15], OutputProfile: record[16], TranscriptPath: record[17],
			TranscriptTextPath: record[18], SubtitlePath: record[19], Status: record[20], HoldReason: record[21],
		}
		row.ArchiveDate = archiveDateFromPackageID(row.ArchivePackageID)
		if row.OutputVideoPath != "" {
			row.OutputVideoName = path.Base(strings.ReplaceAll(row.OutputVideoPath, "\\", "/"))
		}
		if !normalizeRow(&row) {
			return nil, ErrPublishFailed
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func equalStrings(left, right []string) bool {
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

func normalizeRow(row *organization.ArchiveCatalogRow) bool {
	if !row.SourceCreatedAt.IsZero() {
		row.SourceCreatedAt = row.SourceCreatedAt.UTC()
	}
	row.ArchiveDate = strings.TrimSpace(row.ArchiveDate)
	row.OutputVideoName = strings.TrimSpace(row.OutputVideoName)
	row.WorkspaceDriveID = strings.TrimSpace(row.WorkspaceDriveID)
	row.OriginalAssetID = strings.TrimSpace(row.OriginalAssetID)
	row.SourceName = strings.TrimSpace(row.SourceName)
	row.SourceChecksum = strings.ToLower(strings.TrimSpace(row.SourceChecksum))
	row.SourceVideoCodec = strings.ToLower(strings.TrimSpace(row.SourceVideoCodec))
	row.SourceVideoProfile = strings.TrimSpace(row.SourceVideoProfile)
	row.SourceVideoPixelFormat = strings.ToLower(strings.TrimSpace(row.SourceVideoPixelFormat))
	row.SourceContainer = strings.ToLower(strings.TrimSpace(row.SourceContainer))
	row.SourceAudioCodec = strings.ToLower(strings.TrimSpace(row.SourceAudioCodec))
	row.SourceAudioProfile = strings.TrimSpace(row.SourceAudioProfile)
	row.SourceAudioChannelLayout = strings.ToLower(strings.TrimSpace(row.SourceAudioChannelLayout))
	row.DecisionID = strings.TrimSpace(row.DecisionID)
	row.ArchivePackageID = strings.TrimSpace(row.ArchivePackageID)
	row.OutputMediaID = strings.TrimSpace(row.OutputMediaID)
	row.OutputProfile = strings.TrimSpace(row.OutputProfile)
	row.Status = strings.TrimSpace(row.Status)
	row.HoldReason = strings.TrimSpace(row.HoldReason)
	var folderOK, sourcePathOK bool
	row.MediaFolder, folderOK = normalizePath(row.MediaFolder)
	row.SourceRelativePath, sourcePathOK = normalizePath(row.SourceRelativePath)
	if row.WorkspaceDriveID == "" || row.OriginalAssetID == "" || row.SourceName == "" || row.DecisionID == "" ||
		!folderOK || !sourcePathOK || !checksumPattern.MatchString(row.SourceChecksum) || row.SourceDurationMillis < 0 ||
		row.SourceSizeBytes < 0 || row.SourceBitRateBPS < 0 || row.SourceWidth < 0 || row.SourceHeight < 0 ||
		row.SourceFrameRateNumerator < 0 || row.SourceFrameRateDenominator < 0 ||
		((row.SourceFrameRateNumerator == 0) != (row.SourceFrameRateDenominator == 0)) ||
		row.SourceVideoBitRateBPS < 0 || row.SourceAudioBitRateBPS < 0 || row.SourceAudioSampleRateHz < 0 ||
		row.SourceAudioChannels < 0 || !row.Disposition.Valid() || row.Status == "" {
		return false
	}
	if row.Disposition == vocabulary.OrganizationHold {
		if row.ArchiveDate != "" || row.OutputVideoName != "" {
			return false
		}
		return row.ArchivePackageID == "" && row.OutputMediaID == "" && row.OutputVideoPath == "" && row.OutputProfile == "" &&
			row.TranscriptPath == "" && row.TranscriptTextPath == "" && row.SubtitlePath == "" && row.Status == "HOLD" &&
			vocabulary.OrganizationHoldReason(row.HoldReason).Valid()
	}
	outputPath, outputOK := normalizePath(row.OutputVideoPath)
	row.OutputVideoPath = outputPath
	if !validArchiveDate(row.ArchiveDate) || row.OutputVideoName == "" || row.ArchivePackageID == "" || row.OutputMediaID == "" ||
		row.OutputProfile == "" || !outputOK || row.HoldReason != "" || row.Status == "HOLD" {
		return false
	}
	if (row.TranscriptPath == "") != (row.SubtitlePath == "") || (row.TranscriptPath == "") != (row.TranscriptTextPath == "") {
		return false
	}
	if row.TranscriptPath != "" {
		var transcriptOK, transcriptTextOK, subtitleOK bool
		row.TranscriptPath, transcriptOK = normalizePath(row.TranscriptPath)
		row.TranscriptTextPath, transcriptTextOK = normalizePath(row.TranscriptTextPath)
		row.SubtitlePath, subtitleOK = normalizePath(row.SubtitlePath)
		if !transcriptOK || !transcriptTextOK || !subtitleOK {
			return false
		}
	}
	return true
}

func rowRecord(row organization.ArchiveCatalogRow) []string {
	createdAt := ""
	if !row.SourceCreatedAt.IsZero() {
		createdAt = row.SourceCreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return []string{
		createdAt, row.SourceName, row.ArchiveDate, row.OutputVideoName,
		row.MediaFolder, row.SourceRelativePath, row.Status, string(row.Disposition), row.HoldReason,
		strconv.FormatInt(row.SourceDurationMillis, 10), strconv.FormatInt(row.SourceSizeBytes, 10), row.SourceContainer,
		strconv.FormatInt(row.SourceBitRateBPS, 10), row.SourceVideoCodec, row.SourceVideoProfile,
		strconv.Itoa(row.SourceWidth), strconv.Itoa(row.SourceHeight), frameRateString(row.SourceFrameRateNumerator, row.SourceFrameRateDenominator),
		row.SourceVideoPixelFormat, strconv.FormatInt(row.SourceVideoBitRateBPS, 10),
		row.SourceAudioCodec, row.SourceAudioProfile, strconv.FormatInt(row.SourceAudioBitRateBPS, 10),
		strconv.Itoa(row.SourceAudioSampleRateHz), strconv.Itoa(row.SourceAudioChannels), row.SourceAudioChannelLayout,
		row.OutputProfile, row.OutputVideoPath, row.TranscriptPath, row.TranscriptTextPath, row.SubtitlePath,
		row.WorkspaceDriveID, row.OriginalAssetID, row.SourceChecksum, row.DecisionID, row.ArchivePackageID, row.OutputMediaID,
	}
}

func catalogRowKey(row organization.ArchiveCatalogRow) string {
	return strings.TrimSpace(row.WorkspaceDriveID) + "\x00" + strings.TrimSpace(row.OriginalAssetID)
}

func frameRateString(numerator, denominator int64) string {
	if numerator == 0 || denominator == 0 {
		return ""
	}
	return strconv.FormatInt(numerator, 10) + "/" + strconv.FormatInt(denominator, 10)
}

func parseFrameRate(value string) (int64, int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, nil
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0, 0, ErrPublishFailed
	}
	numerator, numeratorErr := strconv.ParseInt(parts[0], 10, 64)
	denominator, denominatorErr := strconv.ParseInt(parts[1], 10, 64)
	if numeratorErr != nil || denominatorErr != nil || numerator <= 0 || denominator <= 0 {
		return 0, 0, ErrPublishFailed
	}
	return numerator, denominator, nil
}

func parseOptionalTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, ErrPublishFailed
	}
	return parsed.UTC(), nil
}

func archiveDateFromPackageID(packageID string) string {
	parts := strings.Split(strings.TrimSpace(packageID), "-")
	if len(parts) >= 3 && parts[0] == "VP" && validArchiveDate(parts[1]) {
		return parts[1]
	}
	return ""
}

func validArchiveDate(value string) bool {
	if len(value) != 8 {
		return false
	}
	_, err := time.Parse("20060102", value)
	return err == nil
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

func equalRecords(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if len(left[index]) != len(right[index]) {
			return false
		}
		for field := range left[index] {
			if left[index][field] != right[index][field] {
				return false
			}
		}
	}
	return true
}
