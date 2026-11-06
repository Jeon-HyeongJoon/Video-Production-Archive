package archivecatalog

import (
	"encoding/csv"
	"strings"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-009
func TestPublisherWritesVerifiedRFC4180CatalogWithEveryOriginalInDeterministicOrder(t *testing.T) {
	rootDirectory := t.TempDir()
	root, err := archive.OpenStorageRoot(rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := NewPublisher(root)
	if err != nil {
		t.Fatal(err)
	}

	driveOne := []organization.ArchiveCatalogRow{
		catalogRow("DRIVE-1", "recordings", "ASSET-4", "Independent.mp4", vocabulary.OrganizationAsIs, "DECISION-3", "MEDIA-2"),
		catalogRow("DRIVE-1", "source", "ASSET-2", "CAM, \"B\" 0002.MP4", vocabulary.OrganizationCompileSourceSequence, "DECISION-1", "MEDIA-1"),
	}
	driveOne[0].SourceCreatedAt = time.Date(2026, 3, 29, 8, 0, 0, 0, time.UTC)
	driveOne[1].SourceCreatedAt = time.Date(2026, 3, 28, 8, 54, 54, 0, time.UTC)
	driveTwo := []organization.ArchiveCatalogRow{
		catalogRow("DRIVE-2", "source", "ASSET-1", "CAM 0001.MP4", vocabulary.OrganizationCompileSourceSequence, "DECISION-4", "MEDIA-3"),
		catalogRow("DRIVE-2", "unknown", "ASSET-3", "보류, 원본.mov", vocabulary.OrganizationHold, "DECISION-5", ""),
	}
	driveTwo[0].SourceCreatedAt = time.Date(2026, 3, 30, 8, 0, 0, 0, time.UTC)
	driveTwo[1].ArchiveDate = ""
	driveTwo[1].OutputVideoName = ""
	driveTwo[1].OutputVideoPath = ""
	driveTwo[1].ArchivePackageID = ""
	driveTwo[1].OutputMediaID = ""
	driveTwo[1].OutputProfile = ""
	driveTwo[1].Status = "HOLD"
	driveTwo[1].HoldReason = string(vocabulary.OrganizationHoldMissingInspection)

	request := func(workspaceDriveID string, rows []organization.ArchiveCatalogRow) Request {
		return Request{
			WorkspaceDriveID: workspaceDriveID, Rows: rows,
			TemporaryPath: "archive-catalog.csv.partial", PublishedPath: "archive-catalog.csv",
		}
	}
	if _, err := publisher.PublishMerged(request("DRIVE-1", driveOne)); err != nil {
		t.Fatal(err)
	}
	result, err := publisher.PublishMerged(request("DRIVE-2", driveTwo))
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
	if len(records) != 5 || !equalRecord(records[0], header) || result.RowCount != 4 || result.Path != "archive-catalog.csv" {
		t.Fatalf("catalog shape mismatch: result=%+v records=%q", result, records)
	}
	// The human-facing date/name pairs lead the sheet while machine IDs remain at the tail.
	if !equalRecord(records[1][:4], []string{"2026-03-28T08:54:54Z", "CAM, \"B\" 0002.MP4", "20261031", "CAM, \"B\" 0002.mp4"}) ||
		records[1][31] != "DRIVE-1" || records[1][32] != "ASSET-2" ||
		records[2][1] != "Independent.mp4" || records[3][31] != "DRIVE-2" || records[3][32] != "ASSET-1" ||
		records[4][6] != "HOLD" || records[4][8] != string(vocabulary.OrganizationHoldMissingInspection) ||
		strings.Contains(string(payload), "C:\\ArchiveStorage") {
		t.Fatalf("catalog values or ordering mismatch: %q", records)
	}
	if _, err := root.ReadFile("archive-catalog.csv.partial"); err == nil {
		t.Fatal("temporary catalog remained after publish")
	}

	secondRoot, err := archive.OpenStorageRoot(rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer secondRoot.Close()
	secondPublisher, err := NewPublisher(secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	concurrentRows := []organization.ArchiveCatalogRow{
		catalogRow("DRIVE-3", "event-c", "ASSET-5", "five.mp4", vocabulary.OrganizationAsIs, "DECISION-6", "MEDIA-5"),
		catalogRow("DRIVE-4", "event-d", "ASSET-6", "six.mp4", vocabulary.OrganizationAsIs, "DECISION-7", "MEDIA-6"),
	}
	publishers := []*Publisher{publisher, secondPublisher}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for index := range concurrentRows {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, publishErr := publishers[index].PublishMerged(request(concurrentRows[index].WorkspaceDriveID, []organization.ArchiveCatalogRow{concurrentRows[index]}))
			errorsFound <- publishErr
		}()
	}
	wait.Wait()
	close(errorsFound)
	for publishErr := range errorsFound {
		if publishErr != nil {
			t.Fatal(publishErr)
		}
	}
	mergedPayload, err := root.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	mergedRecords, err := csv.NewReader(strings.NewReader(string(mergedPayload))).ReadAll()
	if err != nil || len(mergedRecords) != 7 {
		t.Fatalf("concurrent StorageRoot publishers lost catalog rows: records=%d err=%v", len(mergedRecords), err)
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
			TemporaryPath: "archive-catalog.csv.partial", PublishedPath: "archive-catalog.csv",
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
	payload, err := root.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(payload))).ReadAll()
	if err != nil || len(records) != 3 || records[1][32] != "ASSET-1" || records[1][6] != "STORED" ||
		records[2][32] != "ASSET-2" || records[2][6] != "STT_COMPLETED" || records[2][29] == "" {
		t.Fatalf("merged catalog lost or duplicated rows: records=%q err=%v", records, err)
	}
}

func TestPublishMergedMigratesLegacyPerDriveCatalogOnFirstRootPublish(t *testing.T) {
	root, err := archive.OpenStorageRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	legacy := catalogRow("DRIVE-OLD", "old", "ASSET-OLD", "old.mov", vocabulary.OrganizationAsIs, "DECISION-OLD", "MEDIA-OLD")
	legacyRecord := []string{
		legacy.WorkspaceDriveID, legacy.MediaFolder, legacy.OriginalAssetID, legacy.SourceName, legacy.SourceRelativePath, legacy.SourceChecksum,
		"10000", legacy.SourceVideoCodec, "1920", "1080", "6000000", string(legacy.Disposition), legacy.DecisionID,
		legacy.ArchivePackageID, legacy.OutputMediaID, legacy.OutputVideoPath, legacy.OutputProfile, "", "", "", legacy.Status, "",
	}
	var payload strings.Builder
	writer := csv.NewWriter(&payload)
	writer.WriteAll([][]string{legacyHeader, legacyRecord})
	if err := root.MkdirAll("catalog", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("catalog/DRIVE-OLD.csv", []byte(payload.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := NewPublisher(root)
	if err != nil {
		t.Fatal(err)
	}
	current := catalogRow("DRIVE-NEW", "new", "ASSET-NEW", "new.mp4", vocabulary.OrganizationAsIs, "DECISION-NEW", "MEDIA-NEW")
	result, err := publisher.PublishMerged(Request{
		WorkspaceDriveID: "DRIVE-NEW", Rows: []organization.ArchiveCatalogRow{current},
		TemporaryPath: "archive-catalog.csv.partial", PublishedPath: "archive-catalog.csv",
	})
	if err != nil || result.RowCount != 2 {
		t.Fatalf("legacy migration failed: result=%+v err=%v", result, err)
	}
	if _, err := root.ReadFile("catalog/DRIVE-OLD.csv"); err != nil {
		t.Fatal("legacy recovery source must remain intact")
	}
}

func catalogRow(workspaceDriveID, folder, assetID, name string, disposition vocabulary.OrganizationDisposition, decisionID, mediaID string) organization.ArchiveCatalogRow {
	outputName := strings.TrimSuffix(name, ".MP4")
	outputName = strings.TrimSuffix(outputName, ".mp4") + ".mp4"
	return organization.ArchiveCatalogRow{
		SourceName: name, ArchiveDate: "20261031", OutputVideoName: outputName,
		WorkspaceDriveID: workspaceDriveID, MediaFolder: folder, OriginalAssetID: assetID,
		SourceRelativePath:   folder + "/" + name,
		SourceChecksum:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceDurationMillis: 10_000, SourceSizeBytes: 8_000_000, SourceContainer: "mov,mp4", SourceBitRateBPS: 6_300_000,
		SourceVideoCodec: "h264", SourceVideoProfile: "High", SourceWidth: 1920, SourceHeight: 1080,
		SourceFrameRateNumerator: 30_000, SourceFrameRateDenominator: 1_001, SourceVideoPixelFormat: "yuv420p",
		SourceVideoBitRateBPS: 6_000_000, SourceAudioCodec: "aac", SourceAudioProfile: "LC",
		SourceAudioBitRateBPS: 256_000, SourceAudioSampleRateHz: 48_000, SourceAudioChannels: 2, SourceAudioChannelLayout: "stereo",
		Disposition: disposition, DecisionID: decisionID, OutputMediaID: mediaID,
		ArchivePackageID: "VP-20261031-" + mediaID,
		OutputVideoPath:  "packages/VP-20261031-" + mediaID + "/revisions/000001/" + outputName,
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
