package archivecatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

type eventChannel struct {
	PackageID    string `json:"package_id"`
	MetadataFile string `json:"metadata_file"`
	Digest       string `json:"metadata_sha256"`
}
type channelDocument struct {
	archivebundle.Metadata
	Timeline      *organization.SourceTimeline     `json:"source_timeline,omitempty"`
	Quality       *transcription.QualityAssessment `json:"transcript_quality,omitempty"`
	SelfDigest    string                           `json:"manifest_digest,omitempty"`
	Schema        int                              `json:"schema_version"`
	State         archive.PackageState             `json:"state"`
	RunID         string                           `json:"archive_run_id"`
	InputRevision uint64                           `json:"input_revision"`
	Inspection    struct {
		ID        string `json:"id"`
		Duration  int64  `json:"duration_millis"`
		Size      int64  `json:"size_bytes"`
		Container string `json:"container"`
		Codec     string `json:"video_codec"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		BitRate   int64  `json:"video_bitrate_bps"`
	} `json:"output_inspection"`
	Files []struct {
		Kind   string `json:"kind"`
		Name   string `json:"path"`
		Digest string `json:"sha256"`
		Size   int64  `json:"size_bytes"`
	} `json:"files"`
}

func directFile(name string) bool {
	return validPackageFileName(name) && archive.SafeWindowsFileStem(strings.SplitN(name, ".", 2)[0]) == strings.SplitN(name, ".", 2)[0] && archive.SafeWindowsFileStem(name) == name && !strings.ContainsAny(name, "/\\:")
}

func (r *Reconciler) eventRows(dir string) ([]PackageRow, error) {
	data, err := r.root.ReadFile(path.Join(dir, "event.json"))
	if err != nil {
		return nil, err
	}
	var event struct {
		Layout   archive.PackageLayoutVersion `json:"layout_version"`
		Revision uint64                       `json:"revision"`
		Occasion string                       `json:"occasion_id"`
		Channels []eventChannel               `json:"channels"`
	}
	if json.Unmarshal(data, &event) != nil || event.Layout != archive.PackageLayoutEventFolder || event.Revision == 0 || event.Occasion == "" || len(event.Channels) == 0 {
		return nil, ErrCatalogReconciliation
	}
	entries, err := r.root.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if names[n] {
			return nil, fmt.Errorf("%w: conflicting filename %s", ErrCatalogReconciliation, e.Name())
		}
		names[n] = true
	}
	seen := map[string]bool{"event.json": true}
	ids := map[string]bool{}
	for _, ch := range event.Channels {
		name := strings.ToLower(ch.MetadataFile)
		if !directFile(ch.MetadataFile) || !strings.HasSuffix(name, ".json") || seen[name] || ch.PackageID == "" || ids[ch.PackageID] || !checksumPattern.MatchString(ch.Digest) {
			return nil, ErrCatalogReconciliation
		}
		seen[name] = true
		ids[ch.PackageID] = true
	}
	var rows []PackageRow
	var invalid error
	for _, ch := range event.Channels {
		row, err := r.channelRow(dir, ch, seen)
		if err != nil {
			invalid = errors.Join(invalid, fmt.Errorf("%w: %s", err, ch.MetadataFile))
			continue
		}
		rows = append(rows, row)
	}
	return rows, invalid
}
func (r *Reconciler) channelRow(dir string, ch eventChannel, seen map[string]bool) (PackageRow, error) {
	file := path.Join(dir, ch.MetadataFile)
	data, err := r.root.ReadFile(file)
	if err != nil {
		return PackageRow{}, err
	}
	var d channelDocument
	if payloadDigest(data) != ch.Digest || json.Unmarshal(data, &d) != nil || d.Schema != 1 || d.LayoutVersion != archive.PackageLayoutEventFolder || d.PackageID != ch.PackageID || d.RunID == "" || d.InputRevision == 0 || d.Revision == 0 || !d.State.Valid() || len(d.Sources) == 0 {
		return PackageRow{}, ErrCatalogReconciliation
	}
	qc := d.Inspection
	if qc.ID == "" || qc.Duration <= 0 || qc.Size <= 0 || qc.Width <= 0 || qc.Height <= 0 || qc.BitRate <= 0 || qc.Container == "" || qc.Codec == "" {
		return PackageRow{}, ErrCatalogReconciliation
	}
	stem := strings.TrimSuffix(ch.MetadataFile, path.Ext(ch.MetadataFile))
	if !directFile(d.VideoFileName) || d.VideoFileName != stem+".mp4" || d.VideoPath != path.Join(dir, d.VideoFileName) {
		return PackageRow{}, ErrCatalogReconciliation
	}
	if d.SelfDigest != "" {
		return PackageRow{}, ErrCatalogReconciliation
	}
	if d.Disposition == vocabulary.OrganizationCompileSourceSequence {
		if d.Timeline == nil || d.Timeline.Validate() != nil || d.Timeline.DecisionID != d.DecisionID || len(d.Timeline.Clips) != len(d.Sources) {
			return PackageRow{}, ErrCatalogReconciliation
		}
		for i, clip := range d.Timeline.Clips {
			if clip.OriginalAssetID != d.Sources[i].OriginalAssetID {
				return PackageRow{}, ErrCatalogReconciliation
			}
		}
	}
	if d.TranscriptPath != "" && (d.Quality == nil || d.Quality.Validate() != nil) {
		return PackageRow{}, ErrCatalogReconciliation
	}
	sourceIDs := map[string]bool{}
	for _, source := range d.Sources {
		if _, ok := normalizePath(source.RelativePath); !ok {
			return PackageRow{}, ErrCatalogReconciliation
		}
		if source.OriginalAssetID == "" || sourceIDs[source.OriginalAssetID] || source.OriginalName == "" || source.RelativePath == "" || !checksumPattern.MatchString(source.Checksum) {
			return PackageRow{}, ErrCatalogReconciliation
		}
		sourceIDs[source.OriginalAssetID] = true
	}
	verified := map[string]string{}
	for _, f := range d.Files {
		name := strings.ToLower(f.Name)
		if !directFile(f.Name) || seen[name] || f.Kind == "" || f.Size < 0 || !checksumPattern.MatchString(f.Digest) {
			return PackageRow{}, ErrCatalogReconciliation
		}
		seen[name] = true
		size, hash, err := r.root.FileSHA256(path.Join(dir, f.Name))
		if err != nil || hash != f.Digest || size != f.Size {
			return PackageRow{}, ErrCatalogReconciliation
		}
		if f.Kind == "VIDEO" && (f.Name != d.VideoFileName || size != qc.Size) {
			return PackageRow{}, ErrCatalogReconciliation
		}
		verified[path.Join(dir, f.Name)] = f.Kind
	}
	if verified[d.VideoPath] != "VIDEO" {
		return PackageRow{}, ErrCatalogReconciliation
	}
	for kind, file := range map[string]string{"TRANSCRIPT": d.TranscriptPath, "TRANSCRIPT_TEXT": d.TranscriptTextPath, "SUBTITLE": d.SubtitlePath} {
		if file != "" && verified[file] != kind {
			return PackageRow{}, ErrCatalogReconciliation
		}
	}
	return PackageRowFrom(d.Metadata, archivebundle.Package{PackageID: d.PackageID, Revision: d.Revision, State: d.State, MetadataPath: file, ManifestDigest: ch.Digest})
}
