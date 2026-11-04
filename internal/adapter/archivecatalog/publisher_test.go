package archivecatalog

import (
	"encoding/csv"
	"strings"
	"testing"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-009
func TestPublisherWritesVerifiedRFC4180CatalogWithEveryOriginalInDeterministicOrder(t *testing.T) {
	directory := t.TempDir()
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := NewPublisher(root)
	if err != nil {
		t.Fatal(err)
	}
	rows := []organization.ArchiveCatalogRow{
		catalogRow("DRIVE-1", "recordings", "ASSET-4", "Independent.mp4", vocabulary.OrganizationAsIs, "DECISION-3", "MEDIA-2"),
		catalogRow("DRIVE-1", "source", "ASSET-2", "CAM, \"B\" 0002.MP4", vocabulary.OrganizationCompileSourceSequence, "DECISION-1", "MEDIA-1"),
		catalogRow("DRIVE-1", "source", "ASSET-1", "CAM 0001.MP4", vocabulary.OrganizationCompileSourceSequence, "DECISION-1", "MEDIA-1"),
		catalogRow("DRIVE-1", "unknown", "ASSET-3", "보류, 원본.mov", vocabulary.OrganizationHold, "DECISION-2", ""),
	}
	rows[3].OutputVideoPath = ""
	rows[3].ArchivePackageID = ""
	rows[3].OutputProfile = ""
	rows[3].Status = "HOLD"
	rows[3].HoldReason = string(vocabulary.OrganizationHoldMissingInspection)
	result, err := publisher.Publish(Request{
		WorkspaceDriveID: "DRIVE-1", Rows: rows,
		TemporaryPath: "catalog/DRIVE-1.csv.partial",
		PublishedPath: "catalog/DRIVE-1.csv",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := root.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(payload))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{
		"workspace_drive_id", "media_folder", "original_asset_id", "source_name", "source_relative_path", "source_checksum",
		"source_duration_ms", "source_video_codec", "source_width", "source_height", "source_video_bitrate_bps",
		"organization_disposition", "organization_decision_id", "archive_package_id", "output_media_id", "output_video_path", "output_profile",
		"transcript_path", "transcript_text_path", "subtitle_path", "status", "hold_reason",
	}
	if len(records) != 5 || !equalRecord(records[0], wantHeader) || result.RowCount != 4 {
		t.Fatalf("catalog shape mismatch: result=%+v records=%q", result, records)
	}
	order := []string{records[1][2], records[2][2], records[3][2], records[4][2]}
	if !equalRecord(order, []string{"ASSET-4", "ASSET-1", "ASSET-2", "ASSET-3"}) ||
		records[2][13] != "VP-20261031-MEDIA-1" || records[3][13] != "VP-20261031-MEDIA-1" || records[3][3] != "CAM, \"B\" 0002.MP4" ||
		records[4][20] != "HOLD" || records[4][21] != string(vocabulary.OrganizationHoldMissingInspection) ||
		strings.Contains(string(payload), directory) {
		t.Fatalf("catalog values or ordering mismatch: %q", records)
	}
	if _, err := root.ReadFile("catalog/DRIVE-1.csv.partial"); err == nil {
		t.Fatal("temporary catalog remained after publish")
	}
}

func TestPublishMergedKeepsUnchangedRowsAndReplacesCurrentOriginalProjection(t *testing.T) {
	root, err := archive.OpenStorageRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := NewPublisher(root)
	if err != nil {
		t.Fatal(err)
	}
	first := catalogRow("DRIVE-1", "a", "ASSET-1", "one.mp4", vocabulary.OrganizationAsIs, "DECISION-1", "MEDIA-1")
	second := catalogRow("DRIVE-1", "b", "ASSET-2", "two.mp4", vocabulary.OrganizationRenderRecordings, "DECISION-2", "MEDIA-2")
	request := func(rows []organization.ArchiveCatalogRow) Request {
		return Request{
			WorkspaceDriveID: "DRIVE-1", Rows: rows,
			TemporaryPath: "catalog/DRIVE-1.csv.partial", PublishedPath: "catalog/DRIVE-1.csv",
		}
	}
	if _, err := publisher.PublishMerged(request([]organization.ArchiveCatalogRow{first})); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishMerged(request([]organization.ArchiveCatalogRow{second})); err != nil {
		t.Fatal(err)
	}
	second.Status = "STT_COMPLETED"
	second.TranscriptPath = "packages/VP-20261031-MEDIA-2/revisions/000002/transcript.json"
	second.TranscriptTextPath = "packages/VP-20261031-MEDIA-2/revisions/000002/transcript.txt"
	second.SubtitlePath = "packages/VP-20261031-MEDIA-2/revisions/000002/subtitle.srt"
	if result, err := publisher.PublishMerged(request([]organization.ArchiveCatalogRow{second})); err != nil || result.RowCount != 2 {
		t.Fatalf("merged projection failed: result=%+v err=%v", result, err)
	}
	payload, err := root.ReadFile("catalog/DRIVE-1.csv")
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(payload))).ReadAll()
	if err != nil || len(records) != 3 || records[1][2] != "ASSET-1" || records[1][20] != "STORED" ||
		records[2][2] != "ASSET-2" || records[2][20] != "STT_COMPLETED" || records[2][18] == "" {
		t.Fatalf("merged catalog lost or duplicated rows: records=%q err=%v", records, err)
	}
}

func catalogRow(workspaceDriveID, folder, assetID, name string, disposition vocabulary.OrganizationDisposition, decisionID, mediaID string) organization.ArchiveCatalogRow {
	return organization.ArchiveCatalogRow{
		WorkspaceDriveID: workspaceDriveID, MediaFolder: folder, OriginalAssetID: assetID,
		SourceName: name, SourceRelativePath: folder + "/" + name,
		SourceChecksum:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceDurationMillis: 10_000, SourceVideoCodec: "h264", SourceWidth: 1920, SourceHeight: 1080, SourceVideoBitRateBPS: 6_000_000,
		Disposition: disposition, DecisionID: decisionID, OutputMediaID: mediaID,
		ArchivePackageID: "VP-20261031-" + mediaID,
		OutputVideoPath:  "packages/VP-20261031-" + mediaID + "/revisions/000001/video.mp4",
		OutputProfile:    "archive-1080p-6mbps@v1", Status: "STORED",
	}
}

func equalRecord(left, right []string) bool {
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
