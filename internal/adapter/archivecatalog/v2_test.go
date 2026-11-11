package archivecatalog

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-015
func TestPackageCatalogKeepsExactlyOneRootRowPerSingleOrMultiSourcePackage(t *testing.T) {
	fixture := newV2CatalogFixture(t)
	defer fixture.close()
	_, singleMetadata, singleRow := fixture.publish(t, "VP-20261108-CATALOG1", "DECISION-CATALOG-1", "single", []archivebundle.SourceReference{{
		OriginalAssetID: "ASSET-CATALOG-1", OriginalName: "one.mov", RelativePath: "single/one.mov", Checksum: fixture.checksum,
	}})
	_, multiMetadata, multiRow := fixture.publish(t, "VP-20261108-CATALOG2", "DECISION-CATALOG-2", "multi", []archivebundle.SourceReference{
		{OriginalAssetID: "ASSET-CATALOG-2A", OriginalName: "A001.mov", RelativePath: "multi/A001.mov", Checksum: fixture.checksum},
		{OriginalAssetID: "ASSET-CATALOG-2B", OriginalName: "A002.mov", RelativePath: "multi/A002.mov", Checksum: strings.Repeat("b", 64)},
	})
	publisher, err := NewPublisher(fixture.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishPackageRowsMerged([]PackageRow{singleRow}, ".catalog-1.partial", "archive-catalog.csv"); err != nil {
		t.Fatal(err)
	}
	result, err := publisher.PublishPackageRowsMerged([]PackageRow{multiRow}, ".catalog-2.partial", "archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixture.archiveRoot.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parsePackageRows(payload)
	if err != nil {
		t.Fatal(err)
	}
	rowsByID := make(map[string]PackageRow, len(rows))
	for _, row := range rows {
		rowsByID[row.PackageID] = row
	}
	if len(rows) != 2 || result.RowCount != 2 || rowsByID[singleMetadata.PackageID].SourceCount != 1 ||
		rowsByID[multiMetadata.PackageID].SourceCount != 2 ||
		rowsByID[singleMetadata.PackageID].ResultFolder != path.Dir(singleRow.ResultVideoPath) ||
		rowsByID[multiMetadata.PackageID].ResultFolder != path.Dir(multiRow.ResultVideoPath) {
		t.Fatalf("root catalog did not preserve one row per package: result=%+v rows=%+v", result, rows)
	}
}

// Requirement: ARC-017
func TestArchiveCatalogVerifyReportsBidirectionalStatesWithoutChangingStorage(t *testing.T) {
	fixture := newV2CatalogFixture(t)
	defer fixture.close()
	_, _, currentRow := fixture.publish(t, "VP-20261108-VERIFY1", "DECISION-VERIFY-1", "current", catalogSource(fixture.checksum, "CURRENT"))
	_, _, _ = fixture.publish(t, "VP-20261108-VERIFY2", "DECISION-VERIFY-2", "missing", catalogSource(fixture.checksum, "MISSING"))
	_, _, mismatchRow := fixture.publish(t, "VP-20261108-VERIFY3", "DECISION-VERIFY-3", "mismatch", catalogSource(fixture.checksum, "MISMATCH"))
	mismatchRow.Revision++
	orphanRow := currentRow
	orphanRow.PackageID = "VP-20261108-ORPHAN"
	orphanRow.ResultTitle = "orphan.mp4"
	orphanRow.ResultFolder = "2026-11-08_orphan__ORPHAN"
	orphanRow.ResultVideoPath = path.Join(orphanRow.ResultFolder, orphanRow.ResultTitle)
	orphanRow.DecisionID = "DECISION-ORPHAN"
	publisher, err := NewPublisher(fixture.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishPackageRowsMerged([]PackageRow{currentRow, mismatchRow, orphanRow}, ".verify.partial", "archive-catalog.csv"); err != nil {
		t.Fatal(err)
	}
	catalogPayload, err := fixture.archiveRoot.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(catalogPayload)).ReadAll()
	if err != nil || len(records) != 4 {
		t.Fatal(err)
	}
	for _, record := range records[1:] {
		if record[len(record)-1] == mismatchRow.PackageID {
			records = append(records, append([]string(nil), record...))
			break
		}
	}
	if len(records) != 5 {
		t.Fatal("duplicate package identity fixture was not created")
	}
	var duplicated bytes.Buffer
	writer := csv.NewWriter(&duplicated)
	writer.UseCRLF = true
	if err := writer.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	if err := fixture.archiveRoot.WriteFile("archive-catalog.csv", duplicated.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.archiveRoot.MkdirAll("2026-11-08_invalid__INVALID/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fixture.archiveRoot.WriteFile("2026-11-08_invalid__INVALID/"+archivebundle.CompactPackageDocumentName, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotStorage(t, fixture.archiveRoot, ".")
	reconciler, err := NewReconciler(fixture.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.Verify()
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotStorage(t, fixture.archiveRoot, ".")
	states := make(map[ConsistencyState]int)
	for _, entry := range report.Entries {
		states[entry.State]++
	}
	for _, expected := range []ConsistencyState{
		ConsistencyCurrent, ConsistencyCatalogMissing, ConsistencyCatalogOrphan,
		ConsistencyCatalogMismatch, ConsistencyLayoutInvalid,
	} {
		if states[expected] == 0 {
			t.Fatalf("verification omitted %s: %+v", expected, report)
		}
	}
	if !equalSnapshots(before, after) {
		t.Fatalf("read-only verification changed Archive Storage: before=%v after=%v", before, after)
	}
}

// Requirement: ARC-018
func TestArchiveCatalogRebuildAtomicallyReplacesOnlyCatalogAfterPackageVerification(t *testing.T) {
	fixture := newV2CatalogFixture(t)
	defer fixture.close()
	firstPackage, _, firstRow := fixture.publish(t, "VP-20261108-REBUILD1", "DECISION-REBUILD-1", "first", catalogSource(fixture.checksum, "FIRST"))
	secondPackage, _, secondRow := fixture.publish(t, "VP-20261108-REBUILD2", "DECISION-REBUILD-2", "second", catalogSource(fixture.checksum, "SECOND"))
	stale := firstRow
	stale.Revision++
	orphan := firstRow
	orphan.PackageID = "VP-20261108-STALE"
	orphan.ResultTitle = "stale.mp4"
	orphan.ResultFolder = "2026-11-08_stale__STALE"
	orphan.ResultVideoPath = path.Join(orphan.ResultFolder, orphan.ResultTitle)
	orphan.DecisionID = "DECISION-STALE"
	publisher, err := NewPublisher(fixture.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishPackageRowsMerged([]PackageRow{stale, orphan}, ".stale.partial", "archive-catalog.csv"); err != nil {
		t.Fatal(err)
	}
	oldCatalog, err := fixture.archiveRoot.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	packagesBefore := map[string]map[string]string{
		path.Dir(firstPackage.MetadataPath):  snapshotStorage(t, fixture.archiveRoot, path.Dir(firstPackage.MetadataPath)),
		path.Dir(secondPackage.MetadataPath): snapshotStorage(t, fixture.archiveRoot, path.Dir(secondPackage.MetadataPath)),
	}
	reconciler, err := NewReconciler(fixture.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected before catalog replace")
	reconciler.beforeReplace = func() error { return injected }
	if _, err := reconciler.Rebuild(); !errors.Is(err, injected) {
		t.Fatalf("injected replacement failure was not returned: %v", err)
	}
	afterFailure, err := fixture.archiveRoot.ReadFile("archive-catalog.csv")
	if err != nil || !bytes.Equal(oldCatalog, afterFailure) {
		t.Fatalf("failed rebuild changed the current catalog: err=%v", err)
	}
	reconciler.beforeReplace = nil
	result, err := reconciler.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixture.archiveRoot.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parsePackageRows(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || result.RowCount != 2 || rows[0].PackageID != firstRow.PackageID || rows[0].Revision != firstRow.Revision ||
		rows[1].PackageID != secondRow.PackageID || rows[1].Revision != secondRow.Revision {
		t.Fatalf("rebuilt catalog is not a 1:1 package projection: result=%+v rows=%+v", result, rows)
	}
	for directory, before := range packagesBefore {
		if after := snapshotStorage(t, fixture.archiveRoot, directory); !equalSnapshots(before, after) {
			t.Fatalf("catalog rebuild changed package %s: before=%v after=%v", directory, before, after)
		}
	}
}

type v2CatalogFixture struct {
	sourceRoot  *archive.StorageRoot
	archiveRoot *archive.StorageRoot
	publisher   *archivebundle.Publisher
	checksum    string
}

func newV2CatalogFixture(t *testing.T) *v2CatalogFixture {
	t.Helper()
	sourceRoot, err := archive.OpenStorageRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot, err := archive.OpenStorageRoot(t.TempDir())
	if err != nil {
		sourceRoot.Close()
		t.Fatal(err)
	}
	if err := sourceRoot.MkdirAll("source", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sourceRoot.WriteFile("source/video.mov", []byte("catalog-video"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, checksum, err := sourceRoot.FileSHA256("source/video.mov")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := archivebundle.NewPublisher(sourceRoot, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &v2CatalogFixture{sourceRoot: sourceRoot, archiveRoot: archiveRoot, publisher: publisher, checksum: checksum}
}

func (fixture *v2CatalogFixture) close() {
	_ = fixture.sourceRoot.Close()
	_ = fixture.archiveRoot.Close()
}

func (fixture *v2CatalogFixture) publish(t *testing.T, packageID, decisionID, folder string, sources []archivebundle.SourceReference) (archivebundle.Package, archivebundle.Metadata, PackageRow) {
	t.Helper()
	stored, err := fixture.publisher.PublishInitialCompact(archivebundle.InitialRequest{
		LayoutVersion: archive.PackageLayoutCompactResultFolder, PackageID: packageID, WorkspaceDriveID: "SOURCE-DRIVE-1",
		DecisionID: decisionID, MediaFolder: folder, Disposition: vocabulary.OrganizationAsIs,
		ProfileReference: "archive-1080p-6mbps@v1", Sources: sources,
		Video:     archivebundle.FileSource{Root: archivebundle.RootSource, Path: "source/video.mov", Checksum: fixture.checksum},
		CreatedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixture.archiveRoot.ReadFile(stored.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archivebundle.Metadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatal(err)
	}
	row, err := PackageRowFrom(metadata, stored)
	if err != nil {
		t.Fatal(err)
	}
	return stored, metadata, row
}

func catalogSource(checksum, suffix string) []archivebundle.SourceReference {
	return []archivebundle.SourceReference{{
		OriginalAssetID: "ASSET-" + suffix, OriginalName: strings.ToLower(suffix) + ".mov",
		RelativePath: strings.ToLower(suffix) + "/video.mov", Checksum: checksum,
	}}
}

func snapshotStorage(t *testing.T, root *archive.StorageRoot, directory string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	var visit func(string)
	visit = func(current string) {
		entries, err := root.ReadDir(current)
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			relative := entry.Name()
			if current != "." && current != "" {
				relative = path.Join(current, entry.Name())
			}
			if entry.IsDir() {
				result[relative+"/"] = "directory"
				visit(relative)
				continue
			}
			_, checksum, err := root.FileSHA256(relative)
			if err != nil {
				t.Fatal(err)
			}
			result[relative] = checksum
		}
	}
	visit(directory)
	return result
}

func equalSnapshots(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, checksum := range left {
		if right[name] != checksum {
			return false
		}
	}
	return true
}
