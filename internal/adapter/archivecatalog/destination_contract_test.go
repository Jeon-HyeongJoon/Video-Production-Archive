//go:build destination_catalog_contract

package archivecatalog

import (
	"bytes"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"

	"videoarchive/internal/core/archive"
)

func placeDestinationFile(t *testing.T, root *archive.StorageRoot, name string, contents []byte) string {
	t.Helper()
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.WriteFile(name, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return payloadDigest(contents)
}

func placeDestinationJSON(t *testing.T, root *archive.StorageRoot, name string, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return placeDestinationFile(t, root, name, payload)
}

// Requirement: ARC-022
func TestDestinationVerificationAccountsForUnindexedVideos(t *testing.T) {
	for _, tc := range []struct {
		name       string
		files      []string
		state      string
		candidates int
	}{
		{"empty", nil, "EMPTY", 0},
		{"reserved staging", []string{"staging/job/render.mp4"}, "EMPTY", 0},
		{"bare output", []string{"loose.mp4"}, "INCOMPLETE", 1},
		{"rehearsal", []string{"occasion/event.json", "occasion/PGM.json", "occasion/PGM.mp4", "occasion/side.mp4"}, "INCOMPLETE", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newV2CatalogFixture(t)
			defer f.close()
			for _, name := range tc.files {
				placeDestinationFile(t, f.archiveRoot, name, []byte("{}"))
			}
			before := snapshotStorage(t, f.archiveRoot, ".")
			reconciler, err := NewReconciler(f.archiveRoot)
			if err != nil {
				t.Fatal(err)
			}
			report, err := reconciler.Verify()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Coverage struct {
					State      string   `json:"state"`
					Candidates int      `json:"candidate_videos"`
					Unresolved []string `json:"unresolved_videos"`
				} `json:"coverage"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			c := result.Coverage
			if c.State != tc.state || c.Candidates != tc.candidates || len(c.Unresolved) != tc.candidates || report.Current() {
				t.Fatalf("coverage=%+v current=%v, want %s with %d unresolved videos", c, report.Current(), tc.state, tc.candidates)
			}
			var wantPaths []string
			for _, item := range tc.files {
				if strings.HasSuffix(item, ".mp4") && !strings.HasPrefix(item, "staging/") {
					wantPaths = append(wantPaths, item)
				}
			}
			slices.Sort(c.Unresolved)
			slices.Sort(wantPaths)
			if !slices.Equal(c.Unresolved, wantPaths) {
				t.Fatalf("unresolved files %v, want %v", c.Unresolved, wantPaths)
			}
			if !equalSnapshots(before, snapshotStorage(t, f.archiveRoot, ".")) {
				t.Fatal("verification changed the destination")
			}
		})
	}
}

// Requirement: ARC-023
func TestDestinationRebuildRetainsCatalogWhenCoverageIsIncomplete(t *testing.T) {
	f := newV2CatalogFixture(t)
	defer f.close()
	stored, _, stale := f.publish(t, "VP-20261124-KNOWN", "DECISION", "recording", catalogSource(f.checksum, "PRIMARY"))
	_, _, other := f.publish(t, "VP-20261124-OTHER", "DECISION-OTHER", "side", catalogSource(f.checksum, "SIDE"))
	pub, err := NewPublisher(f.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pub.PublishPackageRowsMerged([]PackageRow{stale, other}, ".initial.partial", "archive-catalog.csv"); err != nil {
		t.Fatal(err)
	}
	current := advanceDestinationPackage(t, f, stored)
	rec, err := NewReconciler(f.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, unresolved := range []bool{true, false} {
		if unresolved {
			placeDestinationFile(t, f.archiveRoot, "extra/PGM.mp4", []byte("unidentified result"))
		}
		before := snapshotStorage(t, f.archiveRoot, ".")
		_, err = rec.Rebuild()
		if unresolved {
			if err == nil {
				t.Error("rebuild accepted an incomplete inventory")
			}
			if !equalSnapshots(before, snapshotStorage(t, f.archiveRoot, ".")) {
				t.Error("incomplete rebuild changed archive contents")
			}
			// Resolve the isolated fixture before exercising the complete-scan case.
			if err := f.archiveRoot.Remove("extra/PGM.mp4"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := f.archiveRoot.ReadFile("archive-catalog.csv")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := parsePackageRows(data)
		if err != nil || len(rows) != 2 {
			t.Fatalf("rebuilt rows: %v %v", rows, err)
		}
		got := map[string]PackageRow{}
		for _, row := range rows {
			got[row.PackageID] = row
		}
		if !samePackageRow(got[current.PackageID], current) || !samePackageRow(got[other.PackageID], other) {
			t.Errorf("repair did not retain the other package and restore revision 4: %+v", rows)
		}
		after := snapshotStorage(t, f.archiveRoot, ".")
		delete(before, "archive-catalog.csv")
		delete(after, "archive-catalog.csv")
		if !equalSnapshots(before, after) {
			t.Error("repair changed package bytes")
		}
		report, err := rec.Verify()
		if err != nil || !report.Current() || len(report.Entries) != 2 {
			t.Fatalf("post-repair verification: %+v %v", report, err)
		}
	}
}

// Produce real package revisions so the comparison is valid even if the
// publisher verifies the disk manifest as well as the existing catalog row.
func advanceDestinationPackage(t *testing.T, f *v2CatalogFixture, current archivebundle.Package) PackageRow {
	t.Helper()
	source := func(name, body string) archivebundle.FileSource {
		file := path.Join("staging", name)
		checksum := placeDestinationFile(t, f.archiveRoot, file, []byte(body))
		return archivebundle.FileSource{Root: archivebundle.RootArchive, Path: file, Checksum: checksum}
	}
	transcript := source("transcript.json", `{"segments":[]}`)
	text := source("transcript.txt", "transcript")
	subtitle := source("subtitle.srt", "1\n00:00:00,000 --> 00:00:01,000\ntext\n")
	for current.Revision < 4 {
		next, err := f.publisher.ReviseStored(archivebundle.RevisionRequest{
			PackageID: current.PackageID, BaseRevision: current.Revision,
			Transcript: transcript, TranscriptText: text, Subtitle: subtitle,
			CreatedAt: time.Date(2026, 11, 25, 0, int(current.Revision), 0, 0, time.UTC),
		})
		if err != nil || next.Revision != current.Revision+1 {
			t.Fatalf("advance package: %+v %v", next, err)
		}
		current = next
	}
	data, err := f.archiveRoot.ReadFile(current.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archivebundle.Metadata
	if err = json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	row, err := PackageRowFrom(metadata, current)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Requirement: ARC-024
func TestDestinationCatalogRefusesOlderOrConflictingPublication(t *testing.T) {
	for _, variant := range []string{"older revision", "divergent digest", "same publication"} {
		t.Run(variant, func(t *testing.T) {
			f := newV2CatalogFixture(t)
			defer f.close()
			stored, _, firstRow := f.publish(t, "VP-20261124-MERGE", "DECISION", "recording", catalogSource(f.checksum, "PRIMARY"))
			row := advanceDestinationPackage(t, f, stored)
			pub, err := NewPublisher(f.archiveRoot)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := pub.PublishPackageRowsMerged([]PackageRow{row}, ".initial.partial", "archive-catalog.csv")
			if err != nil {
				t.Fatal(err)
			}
			old, err := f.archiveRoot.ReadFile(initial.Path)
			if err != nil {
				t.Fatal(err)
			}
			incoming := row
			switch variant {
			case "older revision":
				incoming = firstRow
			case "divergent digest":
				incoming.ManifestDigest = strings.Repeat("f", 64)
			}
			replay, err := pub.PublishPackageRowsMerged([]PackageRow{incoming}, ".incoming.partial", initial.Path)
			if variant == "same publication" {
				if err != nil || replay.Checksum != initial.Checksum {
					t.Errorf("identical receipt: %+v %v", replay, err)
				}
			} else if err == nil {
				t.Errorf("accepted %s", variant)
			}
			after, err := f.archiveRoot.ReadFile(initial.Path)
			if err != nil || !bytes.Equal(old, after) {
				t.Fatal("prior catalog bytes lost", err)
			}
		})
	}
}

// Requirement: ARC-025
func TestDestinationEventCatalogUsesOneRowPerVerifiedChannel(t *testing.T) {
	f := newV2CatalogFixture(t)
	defer f.close()
	_, _, oldRow := f.publish(t, "VP-20261124-OLD", "DECISION-OLD", "old", catalogSource(f.checksum, "OLD"))
	const folder = "261124-office"
	type channel struct {
		Package string `json:"package_id"`
		File    string `json:"metadata_file"`
		Digest  string `json:"metadata_sha256"`
	}
	var channels []channel
	for _, stem := range []string{"PGM", "CLN"} {
		body := []byte("verified media " + stem)
		name := stem + ".mp4"
		hash := placeDestinationFile(t, f.archiveRoot, path.Join(folder, name), body)
		id := "VP-20261124-" + stem
		meta := map[string]any{
			"schema_version": 1, "layout_version": 4,
			"package_id": id, "revision": 1, "state": "STORED",
			"archive_run_id": "RUN", "input_revision": 7, "workspace_drive_id": "VOLUME",
			"organization_decision_id": "DECISION-" + stem,
			"archive_date":             "2026-11-24", "created_at": "2026-11-24T00:00:00Z",
			"media_folder": "source/" + stem, "video_file_name": name, "video_path": path.Join(folder, name),
			"organization_disposition": "RENDER_RECORDINGS", "profile_reference": "archive-1080p-6mbps@v1",
			"sources": catalogSource(f.checksum, stem),
			"output_inspection": map[string]any{
				"id": "QC-" + stem, "duration_millis": 1000, "size_bytes": len(body),
				"container": "mp4", "video_codec": "h264", "width": 1920, "height": 1080, "video_bitrate_bps": 6000000,
			},
			"files": []map[string]any{{"kind": "VIDEO", "path": name, "sha256": hash, "size_bytes": len(body)}},
		}
		digest := placeDestinationJSON(t, f.archiveRoot, path.Join(folder, stem+".json"), meta)
		channels = append(channels, channel{id, stem + ".json", digest})
	}
	placeDestinationJSON(t, f.archiveRoot, path.Join(folder, "event.json"), map[string]any{"layout_version": 4, "revision": 1, "occasion_id": "OCC-1", "channels": channels})
	before := snapshotStorage(t, f.archiveRoot, folder)
	rec, err := NewReconciler(f.archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := rec.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	data, err := f.archiveRoot.ReadFile(catalog.Path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parsePackageRows(data)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]PackageRow{}
	for _, r := range rows {
		byID[r.PackageID] = r
	}
	if len(rows) != 3 || !samePackageRow(byID[oldRow.PackageID], oldRow) {
		t.Errorf("mixed layout rebuild produced %d rows", len(rows))
	}
	for _, c := range channels {
		r := byID[c.Package]
		if r.ResultFolder != folder || r.ResultVideoPath != path.Join(folder, strings.TrimSuffix(c.File, ".json")+".mp4") || r.SourceCount != 1 || r.Revision != 1 || r.LayoutVersion != 4 || r.ManifestDigest != c.Digest {
			t.Errorf("channel %s projected incorrectly: %+v", c.Package, r)
		}
	}
	report, err := rec.Verify()
	if err != nil || !report.Current() || len(report.Entries) != 3 {
		t.Errorf("verification omitted a package: %+v %v", report, err)
	}
	if !equalSnapshots(before, snapshotStorage(t, f.archiveRoot, folder)) {
		t.Fatal("indexing modified the event")
	}

	placeDestinationFile(t, f.archiveRoot, path.Join(folder, "PGM.mp4"), []byte("corrupt"))
	report, err = rec.Verify()
	if err != nil || report.Current() {
		t.Fatalf("damaged channel treated as verified: %+v %v", report, err)
	}
}
