package archivecatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
	root *archive.StorageRoot
}

var checksumPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

var header = []string{
	"workspace_drive_id", "media_folder", "original_asset_id", "source_name", "source_relative_path", "source_checksum",
	"source_duration_ms", "source_video_codec", "source_width", "source_height", "source_video_bitrate_bps",
	"organization_disposition", "organization_decision_id", "archive_package_id", "output_media_id", "output_video_path", "output_profile",
	"transcript_path", "transcript_text_path", "subtitle_path", "status", "hold_reason",
}

func NewPublisher(root *archive.StorageRoot) (*Publisher, error) {
	if root == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{root: root}, nil
}

func (publisher *Publisher) Publish(request Request) (organization.ArchiveCatalog, error) {
	workspaceDriveID := strings.TrimSpace(request.WorkspaceDriveID)
	temporaryPath, temporaryOK := normalizePath(request.TemporaryPath)
	publishedPath, publishedOK := normalizePath(request.PublishedPath)
	if workspaceDriveID == "" || len(request.Rows) == 0 || !temporaryOK || !publishedOK || temporaryPath == publishedPath ||
		path.Dir(temporaryPath) != path.Dir(publishedPath) {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	rows := append([]organization.ArchiveCatalogRow(nil), request.Rows...)
	for index := range rows {
		if !normalizeRow(&rows[index], workspaceDriveID) {
			return organization.ArchiveCatalog{}, ErrInvalidRequest
		}
	}
	sort.Slice(rows, func(i, j int) bool {
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
// the supplied OriginalAssets, and republishes the complete CSV atomically.
// ArchiveRun execution serializes catalog Steps for one source drive.
func (publisher *Publisher) PublishMerged(request Request) (organization.ArchiveCatalog, error) {
	publishedPath, publishedOK := normalizePath(request.PublishedPath)
	if !publishedOK {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	exists, err := publisher.root.Exists(publishedPath)
	if err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	if exists {
		payload, readErr := publisher.root.ReadFile(publishedPath)
		if readErr != nil {
			return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, readErr)
		}
		existing, parseErr := parseRows(payload)
		if parseErr != nil {
			return organization.ArchiveCatalog{}, parseErr
		}
		merged := make(map[string]organization.ArchiveCatalogRow, len(existing)+len(request.Rows))
		for _, row := range existing {
			merged[row.OriginalAssetID] = row
		}
		for _, row := range request.Rows {
			merged[strings.TrimSpace(row.OriginalAssetID)] = row
		}
		request.Rows = request.Rows[:0]
		for _, row := range merged {
			request.Rows = append(request.Rows, row)
		}
	}
	return publisher.Publish(request)
}

func parseRows(payload []byte) ([]organization.ArchiveCatalogRow, error) {
	reader := csv.NewReader(bytes.NewReader(payload))
	reader.FieldsPerRecord = len(header)
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 || !equalStrings(records[0], header) {
		return nil, errors.Join(ErrPublishFailed, err)
	}
	rows := make([]organization.ArchiveCatalogRow, 0, len(records)-1)
	for _, record := range records[1:] {
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
		if !normalizeRow(&row, row.WorkspaceDriveID) {
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

func normalizeRow(row *organization.ArchiveCatalogRow, workspaceDriveID string) bool {
	row.WorkspaceDriveID = strings.TrimSpace(row.WorkspaceDriveID)
	row.OriginalAssetID = strings.TrimSpace(row.OriginalAssetID)
	row.SourceName = strings.TrimSpace(row.SourceName)
	row.SourceChecksum = strings.ToLower(strings.TrimSpace(row.SourceChecksum))
	row.SourceVideoCodec = strings.ToLower(strings.TrimSpace(row.SourceVideoCodec))
	row.DecisionID = strings.TrimSpace(row.DecisionID)
	row.ArchivePackageID = strings.TrimSpace(row.ArchivePackageID)
	row.OutputMediaID = strings.TrimSpace(row.OutputMediaID)
	row.OutputProfile = strings.TrimSpace(row.OutputProfile)
	row.Status = strings.TrimSpace(row.Status)
	row.HoldReason = strings.TrimSpace(row.HoldReason)
	var folderOK, sourcePathOK bool
	row.MediaFolder, folderOK = normalizePath(row.MediaFolder)
	row.SourceRelativePath, sourcePathOK = normalizePath(row.SourceRelativePath)
	if row.WorkspaceDriveID != workspaceDriveID || row.OriginalAssetID == "" || row.SourceName == "" || row.DecisionID == "" ||
		!folderOK || !sourcePathOK || !checksumPattern.MatchString(row.SourceChecksum) || row.SourceDurationMillis < 0 ||
		row.SourceWidth < 0 || row.SourceHeight < 0 || row.SourceVideoBitRateBPS < 0 || !row.Disposition.Valid() || row.Status == "" {
		return false
	}
	if row.Disposition == vocabulary.OrganizationHold {
		return row.ArchivePackageID == "" && row.OutputMediaID == "" && row.OutputVideoPath == "" && row.OutputProfile == "" &&
			row.TranscriptPath == "" && row.TranscriptTextPath == "" && row.SubtitlePath == "" && row.Status == "HOLD" &&
			vocabulary.OrganizationHoldReason(row.HoldReason).Valid()
	}
	outputPath, outputOK := normalizePath(row.OutputVideoPath)
	row.OutputVideoPath = outputPath
	if row.ArchivePackageID == "" || row.OutputMediaID == "" || row.OutputProfile == "" || !outputOK || row.HoldReason != "" || row.Status == "HOLD" {
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
	return []string{
		row.WorkspaceDriveID, row.MediaFolder, row.OriginalAssetID, row.SourceName, row.SourceRelativePath, row.SourceChecksum,
		strconv.FormatInt(row.SourceDurationMillis, 10), row.SourceVideoCodec, strconv.Itoa(row.SourceWidth), strconv.Itoa(row.SourceHeight),
		strconv.FormatInt(row.SourceVideoBitRateBPS, 10), string(row.Disposition), row.DecisionID, row.ArchivePackageID,
		row.OutputMediaID, row.OutputVideoPath, row.OutputProfile, row.TranscriptPath, row.TranscriptTextPath, row.SubtitlePath,
		row.Status, row.HoldReason,
	}
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
