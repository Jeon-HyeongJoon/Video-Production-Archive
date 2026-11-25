package archivecatalog

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/mediafolder"
	"videoarchive/internal/core/archive"
)

type Coverage struct {
	State            string      `json:"state"`
	CandidateVideos  int         `json:"candidate_videos"`
	UnresolvedVideos []string    `json:"unresolved_videos"`
	LayoutPackages   map[int]int `json:"layout_packages"`
	LegacyPackages   int         `json:"legacy_packages"`
	LegacySourceRows int         `json:"legacy_source_rows"`
	Exclusions       []string    `json:"excluded,omitempty"`
	Issues           []string    `json:"issues,omitempty"`
	// Small publication documents are checked again before accepting a scan.
	witnesses   map[string]string
	directories map[string]string
	legacyRoot  bool
}

func (r *Reconciler) catalogDigest() (string, error) {
	exists, err := r.root.Exists("archive-catalog.csv")
	if err != nil || !exists {
		return "", err
	}
	_, hash, err := r.root.FileSHA256("archive-catalog.csv")
	return hash, err
}

func inventoryListing(entries []fs.DirEntry, dir string) string {
	var names []string
	for _, e := range entries {
		if dir == "." && (e.Name() == ".archive-catalog.csv.rebuild.partial" || e.Name() == "archive-catalog.csv" || e.Name() == ".archive-catalog.csv.lock") {
			continue
		}
		names = append(names, e.Name()+"\x00"+e.Type().String())
	}
	sort.Strings(names)
	return strings.Join(names, "\x00")
}
func (r *Reconciler) readDirectory(dir string, c *Coverage) ([]fs.DirEntry, error) {
	entries, err := r.root.ReadDir(dir)
	if err == nil {
		c.directories[dir] = inventoryListing(entries, dir)
	}
	return entries, err
}
func (r *Reconciler) stable(c Coverage) error {
	for dir, initial := range c.directories {
		entries, err := r.root.ReadDir(dir)
		if err != nil || inventoryListing(entries, dir) != initial {
			return fmt.Errorf("%w: directory changed at %s", ErrCatalogReconciliation, dir)
		}
	}
	for file, hash := range c.witnesses {
		_, after, err := r.root.FileSHA256(file)
		if err != nil || hash != after {
			return fmt.Errorf("%w: publication changed at %s", ErrCatalogReconciliation, file)
		}
	}
	return nil
}

func (r *Reconciler) scanPackages() ([]PackageRow, []VerificationEntry, Coverage, error) {
	c := Coverage{LayoutPackages: map[int]int{}, UnresolvedVideos: []string{}, witnesses: map[string]string{}, directories: map[string]string{}}
	entries, err := r.readDirectory(".", &c)
	if err != nil {
		return nil, nil, c, err
	}
	var rows []PackageRow
	var invalid []VerificationEntry
	candidates := map[string]bool{}
	legacy := false
	for _, e := range entries {
		name := e.Name()
		switch name {
		case ".archive-locks", ".archive-receipts", "staging", ".archive-catalog.csv.lock":
			c.Exclusions = append(c.Exclusions, name)
			continue
		case "archive-catalog.csv":
			file, err := r.root.Open(name)
			if err != nil {
				c.Issues = append(c.Issues, name)
				continue
			}
			first, readErr := csv.NewReader(file).Read()
			file.Close()
			c.legacyRoot = readErr == nil && (equalStrings(first, legacyHeader) || equalStrings(first, header))
			continue
		case "packages", "catalog":
			legacy = true
			continue
		}
		if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			c.Issues = append(c.Issues, name)
			continue
		}
		if !e.IsDir() {
			if mediafolder.SupportedVideoExtension(path.Ext(name)) {
				candidates[name] = false
			}
			continue
		}
		children, err := r.readDirectory(name, &c)
		if err != nil {
			c.Issues = append(c.Issues, name)
			continue
		}
		marker := ""
		hasV2, hasV3, hasEvent := false, false, false
		for _, f := range children {
			file := path.Join(name, f.Name())
			if f.IsDir() || f.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				c.Issues = append(c.Issues, file)
				continue
			}
			if mediafolder.SupportedVideoExtension(path.Ext(file)) {
				candidates[file] = false
			}
			switch f.Name() {
			case "current.json", "metadata.json":
				hasV2 = true
			case archivebundle.CompactPackageDocumentName:
				hasV3 = true
			case "event.json":
				hasEvent = true
			}
		}
		var found []PackageRow
		switch {
		case hasEvent:
			marker = "event.json"
		case hasV3:
			marker = archivebundle.CompactPackageDocumentName
		case hasV2:
			marker = "current.json"
		default:
			continue
		}
		reference := path.Join(name, marker)
		_, hash, err := r.root.FileSHA256(reference)
		if err == nil {
			c.witnesses[reference] = hash
			switch {
			case (hasV2 && hasV3) || (hasEvent && (hasV2 || hasV3)):
				err = ErrCatalogReconciliation
			case hasEvent:
				found, err = r.eventRows(name)
			default:
				var row PackageRow
				row, err = r.inspectPackage(name)
				if err == nil {
					found = []PackageRow{row}
				}
			}
		}
		if err != nil {
			entry := VerificationEntry{ResultFolder: name, State: ConsistencyLayoutInvalid, Detail: err.Error()}
			if errors.As(err, &entry.sourceConflict) {
				entry.PackageID = entry.sourceConflict.PackageID
				entry.Differences = []string{entry.sourceConflict.Column}
			}
			invalid = append(invalid, entry)
		}
		for _, row := range found {
			if _, ok := candidates[row.ResultVideoPath]; !ok {
				invalid = append(invalid, VerificationEntry{PackageID: row.PackageID, ResultFolder: name, State: ConsistencyLayoutInvalid, Detail: "video missing from inventory"})
				continue
			}
			candidates[row.ResultVideoPath] = true
			c.LayoutPackages[int(row.LayoutVersion)]++
			rows = append(rows, row)
		}
	}
	if legacy || c.legacyRoot {
		r.inspectLegacy(&c)
	}
	c.CandidateVideos += len(candidates)
	for file, ok := range candidates {
		if !ok {
			c.UnresolvedVideos = append(c.UnresolvedVideos, file)
		}
	}
	sort.Strings(c.UnresolvedVideos)
	sort.Strings(c.Issues)
	switch {
	case len(c.UnresolvedVideos)+len(c.Issues)+len(invalid) > 0:
		c.State = "INCOMPLETE"
	case c.CandidateVideos == 0:
		c.State = "EMPTY"
	default:
		c.State = "COMPLETE"
	}
	return rows, invalid, c, nil
}

func (r *Reconciler) currentRow(row PackageRow) (PackageRow, error) {
	if row.LayoutVersion != archive.PackageLayoutEventFolder {
		return r.inspectPackage(row.ResultFolder)
	}
	rows, err := r.eventRows(row.ResultFolder)
	if err != nil {
		return PackageRow{}, err
	}
	for _, v := range rows {
		if v.PackageID == row.PackageID {
			return v, nil
		}
	}
	return PackageRow{}, ErrCatalogReconciliation
}

// Legacy indexes contain one row per source. Compare the current revision only.
func (r *Reconciler) inspectLegacy(c *Coverage) {
	type sourceKey struct{ pkg, id string }
	sources := map[sourceKey]archivebundle.SourceReference{}
	sourceCatalogs := []string{}
	var walk func(string, int)
	walk = func(dir string, depth int) {
		entries, err := r.readDirectory(dir, c)
		if err != nil {
			c.Issues = append(c.Issues, dir)
			return
		}
		for _, e := range entries {
			file := path.Join(dir, e.Name())
			if !e.IsDir() || e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				c.Issues = append(c.Issues, file)
				continue
			}
			if !strings.HasPrefix(e.Name(), "VP-") {
				if depth < 3 {
					walk(file, depth+1)
				} else {
					c.Issues = append(c.Issues, file)
				}
				continue
			}
			pointer := path.Join(file, "current.json")
			raw, err := r.root.ReadFile(pointer)
			var cur archivebundle.Package
			canonical, dirErr := archive.PackageDirectory(e.Name())
			if err != nil || dirErr != nil || (file != canonical && file != path.Join("packages", e.Name())) || json.Unmarshal(raw, &cur) != nil || cur.PackageID != e.Name() || cur.Revision == 0 || !cur.State.Valid() || cur.LayoutVersion > 1 {
				c.Issues = append(c.Issues, pointer)
				continue
			}
			c.witnesses[pointer] = payloadDigest(raw)
			active := path.Join(file, "revisions", fmt.Sprintf("%06d", cur.Revision))
			candidates, err := r.legacyVideos(active, c)
			if err != nil {
				c.Issues = append(c.Issues, active)
				continue
			}
			c.CandidateVideos += len(candidates)
			meta, err := r.legacyMetadata(active, cur)
			if err != nil {
				c.Issues = append(c.Issues, active)
				c.UnresolvedVideos = append(c.UnresolvedVideos, candidates...)
				continue
			}
			for _, video := range candidates {
				if video != meta.VideoPath {
					c.UnresolvedVideos = append(c.UnresolvedVideos, video)
				}
			}
			if !slices.Contains(candidates, meta.VideoPath) {
				c.Issues = append(c.Issues, meta.VideoPath)
				continue
			}

			c.LegacyPackages++
			c.LayoutPackages[1]++
			for _, s := range meta.Sources {
				k := sourceKey{cur.PackageID, s.OriginalAssetID}
				if _, dup := sources[k]; dup {
					c.Issues = append(c.Issues, file)
				}
				sources[k] = s
			}
		}
	}
	if exists, err := r.root.Exists("packages"); err != nil {
		c.Issues = append(c.Issues, "packages")
	} else if exists {
		walk("packages", 0)
	}
	if exists, err := r.root.Exists("catalog"); err != nil {
		c.Issues = append(c.Issues, "catalog")
	} else if exists {
		entries, err := r.readDirectory("catalog", c)
		if err != nil {
			c.Issues = append(c.Issues, "catalog")
		}
		for _, e := range entries {
			file := path.Join("catalog", e.Name())
			if !e.Type().IsRegular() || !strings.EqualFold(path.Ext(file), ".csv") {
				c.Issues = append(c.Issues, file)
				continue
			}
			sourceCatalogs = append(sourceCatalogs, file)
		}
	}
	if c.legacyRoot {
		sourceCatalogs = []string{"archive-catalog.csv"}
	}
	for _, file := range sourceCatalogs {
		data, err := r.root.ReadFile(file)
		if err != nil {
			c.Issues = append(c.Issues, file)
			continue
		}
		rows, err := parseRows(data)
		if err != nil {
			c.Issues = append(c.Issues, file)
			continue
		}
		c.witnesses[file] = payloadDigest(data)
		for _, row := range rows {
			c.LegacySourceRows++
			key := sourceKey{row.ArchivePackageID, row.OriginalAssetID}
			s, ok := sources[key]
			if !ok || s.Checksum != row.SourceChecksum || s.RelativePath != row.SourceRelativePath {
				c.Issues = append(c.Issues, file+" "+row.OriginalAssetID)
			}
			delete(sources, key)
		}
	}
	for k := range sources {
		c.Issues = append(c.Issues, "missing legacy source row "+k.pkg+"/"+k.id)
	}
}

func (r *Reconciler) legacyVideos(dir string, c *Coverage) ([]string, error) {
	entries, err := r.readDirectory(dir, c)
	if err != nil {
		return nil, err
	}
	var videos []string
	for _, entry := range entries {
		name := path.Join(dir, entry.Name())
		if !entry.Type().IsRegular() {
			c.Issues = append(c.Issues, name)
			continue
		}
		if mediafolder.SupportedVideoExtension(path.Ext(name)) {
			videos = append(videos, name)
		}
	}
	return videos, nil
}

func (r *Reconciler) legacyMetadata(dir string, cur archivebundle.Package) (archivebundle.Metadata, error) {
	var meta archivebundle.Metadata
	manifest, err := r.root.ReadFile(path.Join(dir, "manifest-sha256.txt"))
	if err != nil || payloadDigest(manifest) != cur.ManifestDigest || !manifestContains(manifest, "metadata.json") {
		return meta, ErrCatalogReconciliation
	}
	raw, err := r.root.ReadFile(path.Join(dir, "metadata.json"))
	if err != nil || json.Unmarshal(raw, &meta) != nil || meta.PackageID != cur.PackageID || meta.Revision != cur.Revision || len(meta.Sources) == 0 {
		return meta, ErrCatalogReconciliation
	}
	if path.Dir(meta.VideoPath) != dir || !manifestContains(manifest, path.Base(meta.VideoPath)) {
		return meta, ErrCatalogReconciliation
	}
	return meta, r.verifyManifest(dir, manifest)
}

func manifestContains(payload []byte, name string) bool {
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		_, file, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if ok && file == name {
			return true
		}
	}
	return false
}
