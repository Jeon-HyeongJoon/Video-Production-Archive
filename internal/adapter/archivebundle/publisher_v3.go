package archivebundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
)

const CompactLayoutVersion = archive.PackageLayoutCompactResultFolder

type CompactFile struct {
	Path     string  `json:"path"`
	Checksum string  `json:"sha256"`
	Size     int64   `json:"size_bytes"`
	Embedded bool    `json:"embedded,omitempty"`
	Content  *string `json:"content,omitempty"`
}

type CompactRevision struct {
	Revision       uint64          `json:"revision"`
	CreatedAt      time.Time       `json:"created_at"`
	ManifestDigest string          `json:"manifest_digest"`
	Files          []CompactFile   `json:"files"`
	Transcript     json.RawMessage `json:"transcript,omitempty"`
}

// CompactDocument is the private, DB-independent recovery record for layout v3.
// Human-facing files stay beside it; prior logical revisions stay in Revisions.
type CompactDocument struct {
	SchemaVersion int `json:"schema_version"`
	Metadata
	State          archive.PackageState `json:"state"`
	ManifestDigest string               `json:"manifest_digest"`
	Revisions      []CompactRevision    `json:"revisions"`
}

func (publisher *Publisher) PublishInitialCompact(request InitialRequest) (Package, error) {
	request.PackageID = strings.TrimSpace(request.PackageID)
	request.WorkspaceDriveID = strings.TrimSpace(request.WorkspaceDriveID)
	request.DecisionID = strings.TrimSpace(request.DecisionID)
	if request.LayoutVersion != CompactLayoutVersion || !packageIDPattern.MatchString(request.PackageID) ||
		request.WorkspaceDriveID == "" || request.DecisionID == "" || len(request.Sources) == 0 ||
		request.CreatedAt.IsZero() || !request.Disposition.Valid() || request.Disposition == vocabulary.OrganizationHold ||
		strings.TrimSpace(request.ProfileReference) == "" {
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
		PackageID: request.PackageID, LayoutVersion: CompactLayoutVersion, Revision: 1,
		WorkspaceDriveID: request.WorkspaceDriveID, DecisionID: request.DecisionID,
		ArchiveDate: presentation.ArchiveDate, MediaFolder: presentation.MediaFolder,
		DisplayName: presentation.DisplayName, VideoFileName: presentation.VideoFileName,
		Disposition: request.Disposition, ProfileReference: strings.TrimSpace(request.ProfileReference),
		Sources: sources, CreatedAt: request.CreatedAt.UTC(),
	}
	return publisher.publishInitialCompact(metadata, request.Video, presentation.PackageDirectory)
}

func (publisher *Publisher) ReviseCompact(request RevisionRequest) (Package, error) {
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
	if current.LayoutVersion != CompactLayoutVersion || current.State != archive.PackageStored {
		return Package{}, ErrPackageState
	}
	if replayed, ok, replayErr := publisher.replayedStoredRevision(current, request); replayErr != nil || ok {
		return replayed, replayErr
	}
	document, err := publisher.readCompactDocument(current.packageDirectory)
	if err != nil || document.PackageID != request.PackageID || document.Revision != current.Revision {
		return Package{}, fmt.Errorf("read compact package: %w", errors.Join(ErrInvalidRequest, err))
	}
	transcript, _, err := publisher.readVerifiedSource(request.Transcript)
	if err != nil || !json.Valid(transcript) {
		return Package{}, fmt.Errorf("read structured transcript: %w", errors.Join(ErrInvalidRequest, err))
	}
	var compactTranscript bytes.Buffer
	if err := json.Compact(&compactTranscript, transcript); err != nil {
		return Package{}, fmt.Errorf("normalize structured transcript: %w", errors.Join(ErrInvalidRequest, err))
	}
	transcript = compactTranscript.Bytes()
	transcriptText, _, err := publisher.readVerifiedSource(request.TranscriptText)
	if err != nil {
		return Package{}, err
	}
	subtitle, _, err := publisher.readVerifiedSource(request.Subtitle)
	if err != nil {
		return Package{}, err
	}
	document.Revision++
	document.CreatedAt = request.CreatedAt.UTC()
	document.TranscriptPath = path.Join(current.packageDirectory, CompactPackageDocumentName)
	document.TranscriptTextPath = path.Join(current.packageDirectory, "transcript.txt")
	document.SubtitlePath = path.Join(current.packageDirectory, "subtitle.srt")
	revision, err := compactRevision(document.Metadata, request.CreatedAt.UTC(), transcript, transcriptText, subtitle, publisher.archive, current.packageDirectory)
	if err != nil {
		return Package{}, err
	}
	document.ManifestDigest = revision.ManifestDigest
	document.Revisions = append(document.Revisions, revision)
	if !document.valid() {
		return Package{}, fmt.Errorf("validate compact revision: %w", ErrInvalidRequest)
	}
	if err := publisher.writeCurrentFile(path.Join(current.packageDirectory, "transcript.txt"), transcriptText); err != nil {
		return Package{}, err
	}
	if err := publisher.writeCurrentFile(path.Join(current.packageDirectory, "subtitle.srt"), subtitle); err != nil {
		return Package{}, err
	}
	if err := publisher.writeCompactDocument(current.packageDirectory, document); err != nil {
		return Package{}, fmt.Errorf("publish compact document: %w", err)
	}
	result, err := InspectCompactPackage(publisher.archive, current.packageDirectory)
	if err != nil {
		return Package{}, fmt.Errorf("verify compact revision: %w", err)
	}
	return result, nil
}

func (publisher *Publisher) publishInitialCompact(metadata Metadata, video FileSource, directory string) (Package, error) {
	temporary := directory + ".partial"
	_ = publisher.archive.RemoveAll(temporary)
	if err := publisher.archive.MkdirAll(temporary, 0o755); err != nil {
		return Package{}, err
	}
	defer publisher.archive.RemoveAll(temporary)
	videoChecksum, err := publisher.copyVerified(video, path.Join(temporary, metadata.VideoFileName))
	if err != nil {
		return Package{}, err
	}
	videoSize, observedVideoChecksum, err := publisher.archive.FileSHA256(path.Join(temporary, metadata.VideoFileName))
	if err != nil || observedVideoChecksum != videoChecksum {
		return Package{}, errors.Join(ErrInvalidRequest, err)
	}
	metadata.VideoPath = path.Join(directory, metadata.VideoFileName)
	metadata.VideoInfoPath = path.Join(directory, "video-info.csv")
	videoInfo, err := videoInfoCSV(metadata.DecisionID, metadata.Sources)
	if err != nil {
		return Package{}, err
	}
	if err := publisher.archive.WriteFile(path.Join(temporary, "video-info.csv"), videoInfo, 0o600); err != nil {
		return Package{}, err
	}
	videoInfoText := string(videoInfo)
	revision := CompactRevision{
		Revision: metadata.Revision, CreatedAt: metadata.CreatedAt,
		Files: []CompactFile{
			{Path: metadata.VideoFileName, Checksum: videoChecksum, Size: videoSize},
			{Path: "video-info.csv", Checksum: digest(videoInfo), Size: int64(len(videoInfo)), Content: &videoInfoText},
		},
	}
	revision.ManifestDigest = compactManifestDigest(metadata.PackageID, revision)
	document := CompactDocument{
		SchemaVersion: 1, Metadata: metadata, State: archive.PackageStored,
		ManifestDigest: revision.ManifestDigest, Revisions: []CompactRevision{revision},
	}
	payload, err := encodeCompactDocument(document)
	if err != nil {
		return Package{}, err
	}
	if err := publisher.archive.WriteFile(path.Join(temporary, CompactPackageDocumentName), payload, 0o600); err != nil {
		return Package{}, err
	}
	if _, err := inspectCompactAt(publisher.archive, temporary, directory); err != nil {
		return Package{}, err
	}
	if err := publisher.archive.Rename(temporary, directory); err != nil {
		return Package{}, err
	}
	return InspectCompactPackage(publisher.archive, directory)
}

func compactRevision(metadata Metadata, createdAt time.Time, transcript, transcriptText, subtitle []byte, root *archive.StorageRoot, directory string) (CompactRevision, error) {
	previous, err := root.ReadFile(path.Join(directory, "video-info.csv"))
	if err != nil {
		return CompactRevision{}, err
	}
	videoSize, videoChecksum, err := root.FileSHA256(path.Join(directory, metadata.VideoFileName))
	if err != nil {
		return CompactRevision{}, err
	}
	videoInfoText := string(previous)
	transcriptTextValue := string(transcriptText)
	subtitleValue := string(subtitle)
	revision := CompactRevision{
		Revision: metadata.Revision, CreatedAt: createdAt, Transcript: append(json.RawMessage(nil), transcript...),
		Files: []CompactFile{
			{Path: metadata.VideoFileName, Checksum: videoChecksum, Size: videoSize},
			{Path: "video-info.csv", Checksum: digest(previous), Size: int64(len(previous)), Content: &videoInfoText},
			{Path: "transcript.json", Checksum: digest(transcript), Size: int64(len(transcript)), Embedded: true},
			{Path: "transcript.txt", Checksum: digest(transcriptText), Size: int64(len(transcriptText)), Content: &transcriptTextValue},
			{Path: "subtitle.srt", Checksum: digest(subtitle), Size: int64(len(subtitle)), Content: &subtitleValue},
		},
	}
	revision.ManifestDigest = compactManifestDigest(metadata.PackageID, revision)
	return revision, nil
}

func (publisher *Publisher) readCompactDocument(directory string) (CompactDocument, error) {
	payload, err := publisher.archive.ReadFile(path.Join(directory, CompactPackageDocumentName))
	if err != nil {
		return CompactDocument{}, err
	}
	return decodeCompactDocument(payload)
}

func (publisher *Publisher) writeCompactDocument(directory string, document CompactDocument) error {
	payload, err := encodeCompactDocument(document)
	if err != nil {
		return err
	}
	return publisher.writeCurrentFile(path.Join(directory, CompactPackageDocumentName), payload)
}

func (publisher *Publisher) commitCompact(current currentDocument) error {
	document, err := publisher.readCompactDocument(current.packageDirectory)
	if err != nil || document.Revision != current.Revision || document.ManifestDigest != current.ManifestDigest {
		return errors.Join(ErrInvalidRequest, err)
	}
	document.State = archive.PackageCommitted
	return publisher.writeCompactDocument(current.packageDirectory, document)
}

func InspectCompactPackage(root *archive.StorageRoot, directory string) (Package, error) {
	return inspectCompactAt(root, directory, directory)
}

func inspectCompactAt(root *archive.StorageRoot, readDirectory, logicalDirectory string) (Package, error) {
	payload, err := root.ReadFile(path.Join(readDirectory, CompactPackageDocumentName))
	if err != nil {
		return Package{}, err
	}
	document, err := decodeCompactDocument(payload)
	if err != nil {
		return Package{}, err
	}
	entries, err := root.ReadDir(readDirectory)
	if err != nil {
		return Package{}, err
	}
	current := document.Revisions[len(document.Revisions)-1]
	allowed := map[string]bool{CompactPackageDocumentName: true}
	for _, file := range current.Files {
		if !file.Embedded {
			allowed[file.Path] = true
		}
	}
	videoCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			return Package{}, ErrInvalidRequest
		}
		if !allowed[entry.Name()] {
			return Package{}, fmt.Errorf("unexpected compact package file %s: %w", entry.Name(), ErrInvalidRequest)
		}
		if strings.EqualFold(path.Ext(entry.Name()), ".mp4") && !strings.HasPrefix(entry.Name(), ".") {
			videoCount++
		}
	}
	if videoCount != 1 || document.VideoPath != path.Join(logicalDirectory, document.VideoFileName) ||
		document.VideoInfoPath != path.Join(logicalDirectory, "video-info.csv") {
		return Package{}, fmt.Errorf("compact projection paths: %w", ErrInvalidRequest)
	}
	for _, file := range current.Files {
		var payload []byte
		switch {
		case file.Embedded && file.Path == "transcript.json":
			var compact bytes.Buffer
			if err := json.Compact(&compact, current.Transcript); err != nil {
				return Package{}, errors.Join(ErrInvalidRequest, err)
			}
			payload = compact.Bytes()
		case file.Embedded:
			return Package{}, fmt.Errorf("unsupported embedded file %s: %w", file.Path, ErrInvalidRequest)
		default:
			payload, err = root.ReadFile(path.Join(readDirectory, file.Path))
			if err != nil {
				return Package{}, err
			}
		}
		if int64(len(payload)) != file.Size || digest(payload) != file.Checksum {
			return Package{}, fmt.Errorf("compact file %s checksum or size: %w", file.Path, ErrInvalidRequest)
		}
	}
	return Package{
		PackageID: document.PackageID, LayoutVersion: document.LayoutVersion, Revision: document.Revision,
		State: document.State, ManifestDigest: document.ManifestDigest, ArchiveDate: document.ArchiveDate,
		MediaFolder: document.MediaFolder, DisplayName: document.DisplayName, VideoFileName: document.VideoFileName,
		VideoPath: document.VideoPath, TranscriptPath: document.TranscriptPath,
		TranscriptTextPath: document.TranscriptTextPath, SubtitlePath: document.SubtitlePath,
		MetadataPath: path.Join(logicalDirectory, CompactPackageDocumentName), VideoInfoPath: document.VideoInfoPath,
	}, nil
}

func decodeCompactDocument(payload []byte) (CompactDocument, error) {
	var document CompactDocument
	if json.Unmarshal(payload, &document) != nil || !document.valid() {
		return CompactDocument{}, ErrInvalidRequest
	}
	return document, nil
}

func encodeCompactDocument(document CompactDocument) ([]byte, error) {
	if !document.valid() {
		return nil, ErrInvalidRequest
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func (document CompactDocument) valid() bool {
	if document.SchemaVersion != 1 || document.LayoutVersion != CompactLayoutVersion ||
		!packageIDPattern.MatchString(document.PackageID) || document.Revision == 0 || !document.State.Valid() ||
		len(document.Revisions) == 0 || len(document.Sources) == 0 || document.VideoFileName == "" ||
		!checksumPattern.MatchString(document.ManifestDigest) {
		return false
	}
	for index, revision := range document.Revisions {
		if revision.Revision != uint64(index+1) || revision.CreatedAt.IsZero() || len(revision.Files) < 2 ||
			!checksumPattern.MatchString(revision.ManifestDigest) ||
			revision.ManifestDigest != compactManifestDigest(document.PackageID, revision) {
			return false
		}
		seen := make(map[string]bool, len(revision.Files))
		for _, file := range revision.Files {
			if file.Path == "" || file.Path != path.Base(file.Path) || seen[file.Path] ||
				!checksumPattern.MatchString(file.Checksum) || file.Size < 0 {
				return false
			}
			seen[file.Path] = true
			if file.Content != nil && (file.Embedded || int64(len([]byte(*file.Content))) != file.Size || digest([]byte(*file.Content)) != file.Checksum) {
				return false
			}
			if file.Embedded {
				if file.Path != "transcript.json" || len(revision.Transcript) == 0 {
					return false
				}
				var compact bytes.Buffer
				if json.Compact(&compact, revision.Transcript) != nil || int64(compact.Len()) != file.Size || digest(compact.Bytes()) != file.Checksum {
					return false
				}
			}
		}
	}
	last := document.Revisions[len(document.Revisions)-1]
	return last.Revision == document.Revision && last.ManifestDigest == document.ManifestDigest
}

func compactManifestDigest(packageID string, revision CompactRevision) string {
	files := append([]CompactFile(nil), revision.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	type manifestFile struct {
		Path     string `json:"path"`
		SHA      string `json:"sha256"`
		Size     int64  `json:"size_bytes"`
		Embedded bool   `json:"embedded,omitempty"`
	}
	manifest := struct {
		PackageID string         `json:"package_id"`
		Revision  uint64         `json:"revision"`
		Files     []manifestFile `json:"files"`
	}{PackageID: packageID, Revision: revision.Revision}
	for _, file := range files {
		manifest.Files = append(manifest.Files, manifestFile{Path: file.Path, SHA: file.Checksum, Size: file.Size, Embedded: file.Embedded})
	}
	payload, _ := json.Marshal(manifest)
	return digest(payload)
}
