package archivecatalog

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
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

var ErrCatalogReconciliation = errors.New("archive catalog reconciliation failed")

type ConsistencyState string

const (
	ConsistencyCurrent         ConsistencyState = "CURRENT"
	ConsistencyCatalogPending  ConsistencyState = "CATALOG_PENDING"
	ConsistencyCatalogMissing  ConsistencyState = "CATALOG_MISSING"
	ConsistencyCatalogOrphan   ConsistencyState = "CATALOG_ORPHAN"
	ConsistencyCatalogMismatch ConsistencyState = "CATALOG_MISMATCH"
	ConsistencyLayoutInvalid   ConsistencyState = "LAYOUT_INVALID"
)

type PackageRow struct {
	ArchiveDate        string
	ResultTitle        string
	LayoutVersion      archive.PackageLayoutVersion
	ResultFolder       string
	ResultVideoPath    string
	Revision           uint64
	ManifestDigest     string
	ArchiveState       archive.PackageState
	Disposition        vocabulary.OrganizationDisposition
	ProfileReference   string
	SourceCount        int
	TranscriptPath     string
	TranscriptTextPath string
	SubtitlePath       string
	CompletedAt        time.Time
	WorkspaceDriveID   string
	DecisionID         string
	PackageID          string
}

type VerificationEntry struct {
	PackageID       string           `json:"package_id,omitempty"`
	ResultFolder    string           `json:"result_folder,omitempty"`
	State           ConsistencyState `json:"state"`
	Detail          string           `json:"detail,omitempty"`
	CatalogRevision uint64           `json:"catalog_revision,omitempty"`
	PackageRevision uint64           `json:"package_revision,omitempty"`
	CatalogDigest   string           `json:"catalog_manifest_digest,omitempty"`
	PackageDigest   string           `json:"package_manifest_digest,omitempty"`
	Differences     []string         `json:"differences,omitempty"`
	sourceConflict  *archivebundle.SourceMetadataConflict
}

type Verification struct {
	VolumeID         string              `json:"volume_id,omitempty"`
	Coverage         Coverage            `json:"coverage"`
	CatalogSHA256    string              `json:"catalog_sha256,omitempty"`
	ArchiveStorageID string              `json:"archive_storage_id,omitempty"`
	ObservedAt       time.Time           `json:"observed_at,omitzero"`
	Entries          []VerificationEntry `json:"entries"`
}

func (verification Verification) Current() bool {
	if verification.Coverage.State != "COMPLETE" || (len(verification.Entries) == 0 && verification.Coverage.LegacyPackages == 0) {
		return false
	}
	for _, entry := range verification.Entries {
		if entry.State != ConsistencyCurrent {
			return false
		}
	}
	return true
}

var packageHeader = []string{
	"archive_date", "result_title", "layout_version", "result_folder", "result_video_path",
	"package_revision", "manifest_digest", "archive_state", "organization_disposition", "profile_reference",
	"source_count", "transcript_path", "transcript_text_path", "subtitle_path", "completed_at",
	"workspace_drive_id", "organization_decision_id", "archive_package_id",
}

func PackageRowFrom(metadata archivebundle.Metadata, current archivebundle.Package) (PackageRow, error) {
	directory := path.Dir(current.MetadataPath)
	row := PackageRow{
		ArchiveDate: metadata.ArchiveDate, ResultTitle: metadata.VideoFileName,
		LayoutVersion: metadata.LayoutVersion, ResultFolder: directory, ResultVideoPath: metadata.VideoPath,
		Revision: current.Revision, ManifestDigest: current.ManifestDigest, ArchiveState: current.State,
		Disposition: metadata.Disposition, ProfileReference: metadata.ProfileReference, SourceCount: len(metadata.Sources),
		TranscriptPath: metadata.TranscriptPath, TranscriptTextPath: metadata.TranscriptTextPath,
		SubtitlePath: metadata.SubtitlePath, CompletedAt: metadata.CreatedAt,
		WorkspaceDriveID: metadata.WorkspaceDriveID, DecisionID: metadata.DecisionID, PackageID: metadata.PackageID,
	}
	if !normalizePackageRow(&row) {
		return PackageRow{}, ErrInvalidRequest
	}
	return row, nil
}

func (publisher *Publisher) PublishPackageRowsMerged(rows []PackageRow, temporaryPath, publishedPath string) (organization.ArchiveCatalog, error) {
	if publisher == nil || publisher.root == nil || len(rows) == 0 {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	temporaryPath, temporaryOK := normalizePath(temporaryPath)
	publishedPath, publishedOK := normalizePath(publishedPath)
	if !temporaryOK || !publishedOK || temporaryPath == publishedPath || path.Dir(temporaryPath) != path.Dir(publishedPath) {
		return organization.ArchiveCatalog{}, ErrInvalidRequest
	}
	normalized := append([]PackageRow(nil), rows...)
	for index := range normalized {
		if !normalizePackageRow(&normalized[index]) {
			return organization.ArchiveCatalog{}, ErrInvalidRequest
		}
	}
	lock, err := publisher.root.AcquireExclusiveLock(context.Background(), ".archive-catalog.csv.lock")
	if err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	defer lock.Close()
	var existing []PackageRow
	var existingDigest string
	exists, err := publisher.root.Exists(publishedPath)
	if err != nil {
		return organization.ArchiveCatalog{}, err
	}
	if exists {
		payload, readErr := publisher.root.ReadFile(publishedPath)
		if readErr != nil {
			return organization.ArchiveCatalog{}, readErr
		}
		existingDigest = payloadDigest(payload)
		existing, err = parsePackageRows(payload)
		if err != nil {
			return organization.ArchiveCatalog{}, err
		}
	}
	merged := make(map[string]PackageRow, len(existing)+len(normalized))
	for _, row := range existing {
		merged[row.PackageID] = row
	}
	changed := false
	for _, row := range normalized {
		if prior, found := merged[row.PackageID]; found {
			if prior.Revision > row.Revision || (prior.Revision == row.Revision && !samePackageRow(prior, row)) {
				return organization.ArchiveCatalog{}, ErrCatalogReconciliation
			}
			if row.Revision == prior.Revision {
				continue
			}
			if row.Revision > prior.Revision {
				r := Reconciler{root: publisher.root}
				current, err := r.currentRow(row)
				if err != nil || !samePackageRow(current, row) {
					return organization.ArchiveCatalog{}, errors.Join(ErrCatalogReconciliation, err)
				}
			}
		}
		merged[row.PackageID] = row
		changed = true
	}
	if !changed {
		return publishedCatalog(existing, publishedPath, existingDigest), nil
	}
	all := make([]PackageRow, 0, len(merged))
	for _, row := range merged {
		all = append(all, row)
	}
	return publisher.publishPackageRows(all, temporaryPath, publishedPath)
}

func (publisher *Publisher) publishPackageRows(rows []PackageRow, temporaryPath, publishedPath string) (organization.ArchiveCatalog, error) {
	payload, err := encodePackageRows(rows)
	if err != nil {
		return organization.ArchiveCatalog{}, err
	}
	defer publisher.root.Remove(temporaryPath)
	if err := publisher.root.WriteFile(temporaryPath, payload, 0o600); err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	verified, err := publisher.root.ReadFile(temporaryPath)
	if err != nil {
		return organization.ArchiveCatalog{}, err
	}
	parsed, err := parsePackageRows(verified)
	if err != nil || len(parsed) != len(rows) {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	if publisher.beforeReplace != nil {
		if err := publisher.beforeReplace(); err != nil {
			return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
		}
	}
	if err := publisher.root.Rename(temporaryPath, publishedPath); err != nil {
		return organization.ArchiveCatalog{}, errors.Join(ErrPublishFailed, err)
	}
	return publishedCatalog(rows, publishedPath, payloadDigest(verified)), nil
}

func publishedCatalog(rows []PackageRow, publishedPath, checksum string) organization.ArchiveCatalog {
	workspaceDriveID := "MULTIPLE"
	if len(rows) == 1 {
		workspaceDriveID = rows[0].WorkspaceDriveID
	}
	return organization.ArchiveCatalog{
		WorkspaceDriveID: workspaceDriveID, Path: publishedPath, RowCount: len(rows), Checksum: checksum,
	}
}

type Reconciler struct {
	root          *archive.StorageRoot
	beforeReplace func() error
}

func NewReconciler(root *archive.StorageRoot) (*Reconciler, error) {
	if root == nil {
		return nil, ErrInvalidRequest
	}
	return &Reconciler{root: root}, nil
}

func (reconciler *Reconciler) Verify() (Verification, error) {
	before, err := reconciler.catalogDigest()
	if err != nil {
		return Verification{}, err
	}
	packages, invalid, coverage, err := reconciler.scanPackages()
	if err != nil {
		return Verification{}, err
	}
	var rows []PackageRow
	if !coverage.legacyRoot {
		rows, err = reconciler.currentPackageRows()
		if err != nil {
			coverage.State = "INCOMPLETE"
			coverage.Issues = append(coverage.Issues, "archive-catalog.csv could not be read as package rows")
		}
	}
	after, err := reconciler.catalogDigest()
	if err != nil {
		return Verification{}, err
	}
	if after != before {
		coverage.State = "INCOMPLETE"
		coverage.Issues = append(coverage.Issues, "archive-catalog.csv changed during verification")
	}
	if err := reconciler.stable(coverage); err != nil {
		coverage.State = "INCOMPLETE"
		coverage.Issues = append(coverage.Issues, err.Error())
	}
	return compareRows(packages, rows, invalid, coverage, after), nil
}

func compareRows(packages, rows []PackageRow, invalid []VerificationEntry, coverage Coverage, digest string) Verification {
	if coverage.State == "EMPTY" && len(rows) > 0 {
		coverage.State = "COMPLETE"
	}
	result := Verification{Coverage: coverage, CatalogSHA256: digest, Entries: append([]VerificationEntry(nil), invalid...)}
	rowsByID := make(map[string][]PackageRow, len(rows))
	for _, row := range rows {
		rowsByID[row.PackageID] = append(rowsByID[row.PackageID], row)
	}
	for i := range result.Entries {
		entry := &result.Entries[i]
		matching := rowsByID[entry.PackageID]
		if entry.sourceConflict != nil && len(matching) == 1 && matching[0].ResultFolder == entry.ResultFolder {
			entry.CatalogRevision, entry.CatalogDigest = matching[0].Revision, matching[0].ManifestDigest
			delete(rowsByID, entry.PackageID)
		}
	}
	packageCounts := make(map[string]int, len(packages))
	for _, candidate := range packages {
		packageCounts[candidate.PackageID]++
	}
	for _, candidate := range packages {
		entry := VerificationEntry{PackageID: candidate.PackageID, ResultFolder: candidate.ResultFolder, State: ConsistencyCurrent}
		switch {
		case packageCounts[candidate.PackageID] != 1:
			entry.State = ConsistencyCatalogMismatch
			entry.Detail = "duplicate package identity"
		case len(rowsByID[candidate.PackageID]) == 0:
			entry.State = ConsistencyCatalogMissing
			entry.Detail = "catalog row is missing"
			entry.PackageRevision = candidate.Revision
			entry.PackageDigest = candidate.ManifestDigest
		case len(rowsByID[candidate.PackageID]) != 1 || !samePackageRow(candidate, rowsByID[candidate.PackageID][0]):
			entry.State = ConsistencyCatalogMismatch
			entry.Detail = "catalog row does not match package"
			old := rowsByID[candidate.PackageID][0]
			entry.CatalogRevision, entry.PackageRevision = old.Revision, candidate.Revision
			entry.CatalogDigest, entry.PackageDigest = old.ManifestDigest, candidate.ManifestDigest
			oldFields, newFields := packageRowRecord(old), packageRowRecord(candidate)
			for i := range newFields {
				if newFields[i] != oldFields[i] {
					entry.Differences = append(entry.Differences, packageHeader[i])
				}
			}
		}
		result.Entries = append(result.Entries, entry)
		delete(rowsByID, candidate.PackageID)
	}
	for packageID, orphanRows := range rowsByID {
		for _, row := range orphanRows {
			result.Entries = append(result.Entries, VerificationEntry{
				PackageID: packageID, ResultFolder: row.ResultFolder, State: ConsistencyCatalogOrphan, Detail: "package folder is missing", CatalogRevision: row.Revision, CatalogDigest: row.ManifestDigest,
			})
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].ResultFolder != result.Entries[j].ResultFolder {
			return result.Entries[i].ResultFolder < result.Entries[j].ResultFolder
		}
		if result.Entries[i].PackageID != result.Entries[j].PackageID {
			return result.Entries[i].PackageID < result.Entries[j].PackageID
		}
		return result.Entries[i].State < result.Entries[j].State
	})
	return result
}

type RebuildEvidence struct {
	Catalog       organization.ArchiveCatalog
	Before, After Verification
}

func (reconciler *Reconciler) Rebuild() (organization.ArchiveCatalog, error) {
	repair, err := reconciler.RebuildWithEvidence()
	return repair.Catalog, err
}

func (reconciler *Reconciler) RebuildWithEvidence() (RebuildEvidence, error) {
	before, err := reconciler.catalogDigest()
	if err != nil {
		return RebuildEvidence{}, err
	}
	rows, err := reconciler.currentPackageRows()
	if err != nil {
		return RebuildEvidence{}, err
	}
	packages, invalid, coverage, err := reconciler.scanPackages()
	if err != nil {
		return RebuildEvidence{}, err
	}
	repair := RebuildEvidence{Before: compareRows(packages, rows, invalid, coverage, before)}
	for _, entry := range invalid {
		if entry.sourceConflict != nil {
			return repair, fmt.Errorf("%w: %s: %w", ErrCatalogReconciliation, entry.ResultFolder, entry.sourceConflict)
		}
	}
	if len(invalid) != 0 || coverage.State == "INCOMPLETE" {
		return repair, ErrCatalogReconciliation
	}
	seen := make(map[string]bool, len(packages))
	for _, row := range packages {
		if seen[row.PackageID] {
			return RebuildEvidence{}, ErrCatalogReconciliation
		}
		seen[row.PackageID] = true
	}
	lock, err := reconciler.root.AcquireExclusiveLock(context.Background(), ".archive-catalog.csv.lock")
	if err != nil {
		return RebuildEvidence{}, err
	}
	defer lock.Close()
	publisher := &Publisher{root: reconciler.root, beforeReplace: func() error {
		if reconciler.beforeReplace != nil {
			if err := reconciler.beforeReplace(); err != nil {
				return err
			}
		}
		if err := reconciler.stable(coverage); err != nil {
			return err
		}
		after, err := reconciler.catalogDigest()
		if err != nil || after != before {
			return errors.Join(ErrCatalogReconciliation, err)
		}
		return nil
	}}
	repair.Catalog, err = publisher.publishPackageRows(packages, ".archive-catalog.csv.rebuild.partial", "archive-catalog.csv")
	if err != nil {
		return repair, err
	}
	rows, err = reconciler.currentPackageRows()
	if err != nil {
		return repair, err
	}
	after, err := reconciler.catalogDigest()
	if err != nil || after != repair.Catalog.Checksum {
		return repair, errors.Join(ErrCatalogReconciliation, err)
	}
	if err := reconciler.stable(coverage); err != nil {
		return repair, err
	}
	repair.After = compareRows(packages, rows, nil, coverage, after)
	if len(packages) > 0 && !repair.After.Current() {
		return repair, ErrCatalogReconciliation
	}
	return repair, nil
}

func (reconciler *Reconciler) currentPackageRows() ([]PackageRow, error) {
	exists, err := reconciler.root.Exists("archive-catalog.csv")
	if err != nil || !exists {
		return nil, err
	}
	payload, err := reconciler.root.ReadFile("archive-catalog.csv")
	if err != nil {
		return nil, err
	}
	return parsePackageRowsForVerification(payload)
}

func (reconciler *Reconciler) inspectPackage(directory string) (PackageRow, error) {
	compact, err := reconciler.root.Exists(path.Join(directory, archivebundle.CompactPackageDocumentName))
	if err != nil {
		return PackageRow{}, err
	}
	if compact {
		return reconciler.inspectCompactPackage(directory)
	}
	entries, err := reconciler.root.ReadDir(directory)
	if err != nil {
		return PackageRow{}, err
	}
	videoCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			return PackageRow{}, fmt.Errorf("nested directory %s", entry.Name())
		}
		if !strings.HasPrefix(entry.Name(), ".") && strings.EqualFold(path.Ext(entry.Name()), ".mp4") {
			videoCount++
		}
	}
	if videoCount != 1 {
		return PackageRow{}, fmt.Errorf("expected one result video, got %d", videoCount)
	}
	metadataPayload, err := reconciler.root.ReadFile(path.Join(directory, "metadata.json"))
	if err != nil {
		return PackageRow{}, err
	}
	var metadata archivebundle.Metadata
	if json.Unmarshal(metadataPayload, &metadata) != nil || metadata.LayoutVersion != archive.PackageLayoutFlatResultFolder ||
		metadata.PackageID == "" || metadata.Revision == 0 || metadata.VideoFileName == "" || len(metadata.Sources) == 0 ||
		metadata.VideoPath != path.Join(directory, metadata.VideoFileName) || metadata.VideoInfoPath != path.Join(directory, "video-info.csv") {
		return PackageRow{}, ErrCatalogReconciliation
	}
	currentPayload, err := reconciler.root.ReadFile(path.Join(directory, "current.json"))
	if err != nil {
		return PackageRow{}, err
	}
	var current struct {
		PackageID      string                       `json:"package_id"`
		LayoutVersion  archive.PackageLayoutVersion `json:"layout_version"`
		Revision       uint64                       `json:"revision"`
		State          archive.PackageState         `json:"state"`
		ManifestDigest string                       `json:"manifest_digest"`
	}
	if json.Unmarshal(currentPayload, &current) != nil || current.PackageID != metadata.PackageID ||
		current.LayoutVersion != archive.PackageLayoutFlatResultFolder || current.Revision != metadata.Revision ||
		!current.State.Valid() {
		return PackageRow{}, ErrCatalogReconciliation
	}
	manifestPayload, err := reconciler.root.ReadFile(path.Join(directory, "manifest-sha256.txt"))
	if err != nil || !manifestContains(manifestPayload, metadata.VideoFileName) || !manifestContains(manifestPayload, "metadata.json") || !manifestContains(manifestPayload, "video-info.csv") || payloadDigest(manifestPayload) != current.ManifestDigest || reconciler.verifyManifest(directory, manifestPayload) != nil {
		return PackageRow{}, errors.Join(ErrCatalogReconciliation, err)
	}
	videoInfo, err := reconciler.root.ReadFile(path.Join(directory, "video-info.csv"))
	if err != nil {
		return PackageRow{}, err
	}
	sourceCount, err := archivebundle.ParseVideoInfoCount(videoInfo)
	if err != nil || sourceCount != len(metadata.Sources) {
		return PackageRow{}, ErrCatalogReconciliation
	}
	packageView := archivebundle.Package{
		PackageID: metadata.PackageID, LayoutVersion: metadata.LayoutVersion, Revision: current.Revision,
		State: current.State, ManifestDigest: current.ManifestDigest, MetadataPath: path.Join(directory, "metadata.json"),
	}
	return PackageRowFrom(metadata, packageView)
}

func (reconciler *Reconciler) inspectCompactPackage(directory string) (PackageRow, error) {
	packageView, metadata, err := archivebundle.InspectCompactMetadata(reconciler.root, directory)
	if err != nil {
		return PackageRow{}, errors.Join(ErrCatalogReconciliation, err)
	}
	return PackageRowFrom(metadata, packageView)
}

func (reconciler *Reconciler) verifyManifest(directory string, payload []byte) error {
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	if len(lines) == 0 {
		return ErrCatalogReconciliation
	}
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || !checksumPattern.MatchString(parts[0]) || !validPackageFileName(parts[1]) || seen[parts[1]] {
			return ErrCatalogReconciliation
		}
		seen[parts[1]] = true
		_, actual, err := reconciler.root.FileSHA256(path.Join(directory, parts[1]))
		if err != nil || actual != strings.ToLower(parts[0]) {
			return ErrCatalogReconciliation
		}
	}
	return nil
}

func validPackageFileName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\/:") && name == path.Base(name) && name != "." && !strings.HasPrefix(name, ".")
}

func normalizePackageRow(row *PackageRow) bool {
	row.ArchiveDate = strings.TrimSpace(row.ArchiveDate)
	row.ResultTitle = strings.TrimSpace(row.ResultTitle)
	row.ResultFolder = strings.TrimSpace(row.ResultFolder)
	row.ResultVideoPath = strings.TrimSpace(row.ResultVideoPath)
	row.ManifestDigest = strings.ToLower(strings.TrimSpace(row.ManifestDigest))
	row.ProfileReference = strings.TrimSpace(row.ProfileReference)
	row.TranscriptPath = strings.TrimSpace(row.TranscriptPath)
	row.TranscriptTextPath = strings.TrimSpace(row.TranscriptTextPath)
	row.SubtitlePath = strings.TrimSpace(row.SubtitlePath)
	row.WorkspaceDriveID = strings.TrimSpace(row.WorkspaceDriveID)
	row.DecisionID = strings.TrimSpace(row.DecisionID)
	row.PackageID = strings.TrimSpace(row.PackageID)
	if !row.CompletedAt.IsZero() {
		row.CompletedAt = row.CompletedAt.UTC()
	}
	if (row.LayoutVersion != archive.PackageLayoutFlatResultFolder && row.LayoutVersion != archive.PackageLayoutCompactResultFolder && row.LayoutVersion != archive.PackageLayoutEventFolder) ||
		row.PackageID == "" || row.Revision == 0 ||
		!checksumPattern.MatchString(row.ManifestDigest) || !row.ArchiveState.Valid() || !row.Disposition.ProducesMedia() || row.SourceCount <= 0 || row.ResultTitle == "" ||
		row.ResultFolder == "" || row.ResultFolder != path.Base(row.ResultFolder) || strings.HasPrefix(row.ResultFolder, ".") ||
		row.ResultVideoPath != path.Join(row.ResultFolder, row.ResultTitle) || row.ProfileReference == "" ||
		row.CompletedAt.IsZero() || row.WorkspaceDriveID == "" || row.DecisionID == "" {
		return false
	}
	if (row.TranscriptPath == "") != (row.TranscriptTextPath == "") || (row.TranscriptPath == "") != (row.SubtitlePath == "") {
		return false
	}
	return true
}

func encodePackageRows(rows []PackageRow) ([]byte, error) {
	rows = append([]PackageRow(nil), rows...)
	seen := make(map[string]bool, len(rows))
	paths := make(map[string]bool, len(rows))
	for index := range rows {
		if !normalizePackageRow(&rows[index]) || seen[rows[index].PackageID] || paths[strings.ToLower(rows[index].ResultVideoPath)] {
			return nil, ErrInvalidRequest
		}
		seen[rows[index].PackageID] = true
		paths[strings.ToLower(rows[index].ResultVideoPath)] = true
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ArchiveDate != rows[j].ArchiveDate {
			return rows[i].ArchiveDate < rows[j].ArchiveDate
		}
		if rows[i].ResultTitle != rows[j].ResultTitle {
			return rows[i].ResultTitle < rows[j].ResultTitle
		}
		return rows[i].PackageID < rows[j].PackageID
	})
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write(packageHeader); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write(packageRowRecord(row)); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func parsePackageRows(payload []byte) ([]PackageRow, error) {
	return parsePackageRowsWithPolicy(payload, true)
}

func parsePackageRowsForVerification(payload []byte) ([]PackageRow, error) {
	return parsePackageRowsWithPolicy(payload, false)
}

func parsePackageRowsWithPolicy(payload []byte, requireUnique bool) ([]PackageRow, error) {
	reader := csv.NewReader(bytes.NewReader(payload))
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 || !equalStrings(records[0], packageHeader) {
		return nil, ErrCatalogReconciliation
	}
	rows := make([]PackageRow, 0, len(records)-1)
	seen := make(map[string]bool, len(records)-1)
	for _, record := range records[1:] {
		if len(record) != len(packageHeader) {
			return nil, ErrCatalogReconciliation
		}
		layout, layoutErr := strconv.ParseUint(record[2], 10, 8)
		revision, revisionErr := strconv.ParseUint(record[5], 10, 64)
		sourceCount, sourceErr := strconv.Atoi(record[10])
		completedAt, timeErr := time.Parse(time.RFC3339Nano, record[14])
		if layoutErr != nil || revisionErr != nil || sourceErr != nil || timeErr != nil {
			return nil, ErrCatalogReconciliation
		}
		row := PackageRow{
			ArchiveDate: record[0], ResultTitle: record[1], LayoutVersion: archive.PackageLayoutVersion(layout),
			ResultFolder: record[3], ResultVideoPath: record[4], Revision: revision, ManifestDigest: record[6],
			ArchiveState: archive.PackageState(record[7]), Disposition: vocabulary.OrganizationDisposition(record[8]),
			ProfileReference: record[9], SourceCount: sourceCount, TranscriptPath: record[11],
			TranscriptTextPath: record[12], SubtitlePath: record[13], CompletedAt: completedAt,
			WorkspaceDriveID: record[15], DecisionID: record[16], PackageID: record[17],
		}
		if !normalizePackageRow(&row) || (requireUnique && seen[row.PackageID]) {
			return nil, ErrCatalogReconciliation
		}
		seen[row.PackageID] = true
		rows = append(rows, row)
	}
	return rows, nil
}

func packageRowRecord(row PackageRow) []string {
	return []string{
		row.ArchiveDate, row.ResultTitle, strconv.Itoa(int(row.LayoutVersion)), row.ResultFolder, row.ResultVideoPath,
		strconv.FormatUint(row.Revision, 10), row.ManifestDigest, string(row.ArchiveState), string(row.Disposition),
		row.ProfileReference, strconv.Itoa(row.SourceCount), row.TranscriptPath, row.TranscriptTextPath, row.SubtitlePath,
		row.CompletedAt.UTC().Format(time.RFC3339Nano), row.WorkspaceDriveID, row.DecisionID, row.PackageID,
	}
}

func samePackageRow(left, right PackageRow) bool {
	return equalStrings(packageRowRecord(left), packageRowRecord(right))
}

func payloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
