package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrOriginalImmutable  = asset.ErrOriginalImmutable
	ErrInvalidProvenance  = errors.New("derived artifact provenance is incomplete")
	ErrCommitPrecondition = errors.New("archive commit precondition failed")
	ErrUnsafeStoragePath  = errors.New("storage path escapes registered root")
	ErrInvalidSnapshot    = errors.New("archive snapshot is invalid")
)

type OriginalAsset = asset.OriginalAsset

type Supersession = asset.Supersession

func SupersedeOriginal(previous, replacement OriginalAsset) (Supersession, error) {
	return asset.SupersedeOriginal(previous, replacement)
}

type Provenance struct {
	OriginalAssetID  string
	InputArtifactIDs []string
	ProcessingKind   vocabulary.ProcessingKind
	ProcessorVersion string
	ProfileVersion   string
	CreatedBy        string
	CreatedAt        time.Time
}

type Artifact struct {
	ID       string
	Path     string
	Size     int64
	Checksum string
	Origin   Provenance
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var windowsDrivePattern = regexp.MustCompile(`^[a-zA-Z]:/`)

func (a Artifact) ValidateDerived() error {
	if a.ID == "" || a.Path == "" || a.Size < 0 || !sha256Pattern.MatchString(a.Checksum) ||
		a.Origin.OriginalAssetID == "" || a.Origin.InputArtifactIDs == nil || !a.Origin.ProcessingKind.Valid() || a.Origin.ProcessorVersion == "" ||
		a.Origin.ProfileVersion == "" || a.Origin.CreatedBy == "" || a.Origin.CreatedAt.IsZero() {
		return ErrInvalidProvenance
	}
	return nil
}

type ManifestEntry struct {
	Path     string
	Checksum string
}

func BuildManifest(entries []ManifestEntry) (string, error) {
	normalizedEntries := make([]ManifestEntry, 0, len(entries))
	seenPaths := make(map[string]bool, len(entries))
	for _, entry := range entries {
		normalized := strings.ReplaceAll(entry.Path, "\\", "/")
		clean := path.Clean(normalized)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
			strings.HasPrefix(normalized, "//") || windowsDrivePattern.MatchString(normalized) || !sha256Pattern.MatchString(entry.Checksum) {
			return "", fmt.Errorf("invalid manifest entry %q", entry.Path)
		}
		if seenPaths[clean] {
			return "", fmt.Errorf("duplicate normalized manifest path %q", clean)
		}
		seenPaths[clean] = true
		normalizedEntries = append(normalizedEntries, ManifestEntry{Path: clean, Checksum: strings.ToLower(entry.Checksum)})
	}
	sort.Slice(normalizedEntries, func(i, j int) bool { return normalizedEntries[i].Path < normalizedEntries[j].Path })
	var builder strings.Builder
	for _, entry := range normalizedEntries {
		fmt.Fprintf(&builder, "%s  %s\n", entry.Checksum, entry.Path)
	}
	return builder.String(), nil
}

type ArchiveCopyEvidence struct {
	ID               string
	StorageID        string
	PhysicalDeviceID string
	ManifestDigest   string
	VerifiedAt       time.Time
}

type VerificationOutcome string

const (
	VerificationPassed VerificationOutcome = "PASSED"
	VerificationFailed VerificationOutcome = "FAILED"
)

type QCVerification struct {
	ID              string
	PackageID       string
	PackageRevision uint64
	Outcome         VerificationOutcome
	VerifiedAt      time.Time
}

type SidecarVerification struct {
	ID              string
	PackageID       string
	PackageRevision uint64
	Required        []string
	Present         []string
	VerifiedAt      time.Time
}

type ManifestVerification struct {
	ID              string
	PackageID       string
	PackageRevision uint64
	ManifestDigest  string
	VerifiedAt      time.Time
}

type CommitEvidence struct {
	PackageID       string
	PackageRevision uint64
	ManifestDigest  string
	QC              QCVerification
	Approval        approval.Record
	Sidecars        SidecarVerification
	Manifest        ManifestVerification
	Copies          []ArchiveCopyEvidence
}

func ValidateCommit(evidence CommitEvidence) error {
	uniqueDevices := make(map[string]bool)
	for _, copyEvidence := range evidence.Copies {
		if copyEvidence.ID == "" || copyEvidence.StorageID == "" || copyEvidence.PhysicalDeviceID == "" ||
			copyEvidence.ManifestDigest != evidence.ManifestDigest || copyEvidence.VerifiedAt.IsZero() {
			return ErrCommitPrecondition
		}
		uniqueDevices[copyEvidence.PhysicalDeviceID] = true
	}
	validApproval := evidence.Approval.ValidFor(evidence.PackageID, evidence.PackageRevision, evidence.ManifestDigest) &&
		evidence.Approval.Decision == vocabulary.ApprovalApproved
	validQC := evidence.QC.ID != "" && evidence.QC.PackageID == evidence.PackageID &&
		evidence.QC.PackageRevision == evidence.PackageRevision && evidence.QC.Outcome == VerificationPassed && !evidence.QC.VerifiedAt.IsZero()
	validManifest := evidence.Manifest.ID != "" && evidence.Manifest.PackageID == evidence.PackageID &&
		evidence.Manifest.PackageRevision == evidence.PackageRevision && evidence.Manifest.ManifestDigest == evidence.ManifestDigest &&
		!evidence.Manifest.VerifiedAt.IsZero()
	validSidecars := evidence.Sidecars.ID != "" && evidence.Sidecars.PackageID == evidence.PackageID &&
		evidence.Sidecars.PackageRevision == evidence.PackageRevision && !evidence.Sidecars.VerifiedAt.IsZero() &&
		containsEvery(evidence.Sidecars.Required, RequiredPackageSidecars()) && containsEvery(evidence.Sidecars.Present, RequiredPackageSidecars())
	if evidence.PackageID == "" || evidence.PackageRevision == 0 || !sha256Pattern.MatchString(evidence.ManifestDigest) ||
		!validQC || !validApproval || !validSidecars || !validManifest ||
		len(evidence.Copies) < 2 || len(uniqueDevices) < 2 {
		return ErrCommitPrecondition
	}
	return nil
}

func containsEvery(values, required []string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func RequiredPackageSidecars() []string {
	return []string{"metadata.json", "files.csv", "events.jsonl", "manifest-sha256.txt", "README.md"}
}

type ArchiveSnapshot struct {
	OriginalAssets []OriginalAsset `json:"original_assets"`
	Artifacts      []Artifact      `json:"artifacts"`
}

func ExportSnapshot(snapshot ArchiveSnapshot) ([]byte, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}
	return json.Marshal(snapshot)
}

func ImportSnapshot(payload []byte) (ArchiveSnapshot, error) {
	var snapshot ArchiveSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ArchiveSnapshot{}, ErrInvalidSnapshot
	}
	if err := validateSnapshot(snapshot); err != nil {
		return ArchiveSnapshot{}, err
	}
	return snapshot, nil
}

func validateSnapshot(snapshot ArchiveSnapshot) error {
	assetIDs := make(map[string]bool, len(snapshot.OriginalAssets))
	for _, asset := range snapshot.OriginalAssets {
		if asset.Validate() != nil || assetIDs[asset.ID] {
			return ErrInvalidSnapshot
		}
		assetIDs[asset.ID] = true
	}
	artifactIDs := make(map[string]bool, len(snapshot.Artifacts))
	for _, artifact := range snapshot.Artifacts {
		if artifactIDs[artifact.ID] || artifact.ValidateDerived() != nil || !assetIDs[artifact.Origin.OriginalAssetID] {
			return ErrInvalidSnapshot
		}
		artifactIDs[artifact.ID] = true
	}
	return nil
}

func ResolveStoragePath(root, requested string) (string, error) {
	rootResolved, clean, err := normalizeStoragePath(root, requested)
	if err != nil {
		return "", ErrUnsafeStoragePath
	}
	candidate := filepath.Join(rootResolved, clean)
	relative, err := filepath.Rel(rootResolved, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeStoragePath
	}
	if err := rejectLinkLikeComponents(rootResolved, relative); err != nil {
		return "", err
	}
	return candidate, nil
}

// StorageRoot keeps actual file access beneath a registered root. Callers must
// use this capability instead of resolving a path and opening it separately.
type StorageRoot struct {
	root *os.Root
}

func OpenStorageRoot(root string) (*StorageRoot, error) {
	rootResolved, _, err := normalizeStoragePath(root, "storage-root-check")
	if err != nil {
		return nil, ErrUnsafeStoragePath
	}
	handle, err := os.OpenRoot(rootResolved)
	if err != nil {
		return nil, ErrUnsafeStoragePath
	}
	return &StorageRoot{root: handle}, nil
}

func (root *StorageRoot) Close() error { return root.root.Close() }

func (root *StorageRoot) Open(requested string) (*os.File, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	_, clean, err := normalizeStoragePath(root.root.Name(), requested)
	if err != nil {
		return nil, ErrUnsafeStoragePath
	}
	if err := rejectLinkLikeComponents(root.root.Name(), clean); err != nil {
		return nil, err
	}
	file, err := root.root.Open(clean)
	if err != nil {
		return nil, ErrUnsafeStoragePath
	}
	return file, nil
}

func (root *StorageRoot) ReadFile(requested string) ([]byte, error) {
	file, err := root.Open(requested)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (root *StorageRoot) WriteFile(requested string, data []byte, permission os.FileMode) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	file, err := root.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permission)
	if err != nil {
		return ErrUnsafeStoragePath
	}
	written, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || written != len(data) || closeErr != nil {
		return ErrUnsafeStoragePath
	}
	return nil
}

func (root *StorageRoot) MkdirAll(requested string, permission os.FileMode) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	current := ""
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err := root.root.Mkdir(current, permission); err != nil {
			info, statErr := root.root.Stat(current)
			if statErr != nil || !info.IsDir() {
				return ErrUnsafeStoragePath
			}
		}
	}
	return nil
}

func (root *StorageRoot) Rename(oldPath, newPath string) error {
	oldClean, err := root.safeRelative(oldPath)
	if err != nil {
		return err
	}
	newClean, err := root.safeRelative(newPath)
	if err != nil {
		return err
	}
	if _, err := root.root.Stat(oldClean); err != nil {
		return ErrUnsafeStoragePath
	}
	oldResolved := filepath.Join(root.root.Name(), oldClean)
	newResolved := filepath.Join(root.root.Name(), newClean)
	if err := os.Rename(oldResolved, newResolved); err != nil {
		return ErrUnsafeStoragePath
	}
	return nil
}

func (root *StorageRoot) Remove(requested string) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	if err := root.root.Remove(clean); err != nil {
		return ErrUnsafeStoragePath
	}
	return nil
}

func (root *StorageRoot) safeRelative(requested string) (string, error) {
	if root == nil || root.root == nil {
		return "", ErrUnsafeStoragePath
	}
	_, clean, err := normalizeStoragePath(root.root.Name(), requested)
	if err != nil {
		return "", ErrUnsafeStoragePath
	}
	if err := rejectLinkLikeComponents(root.root.Name(), clean); err != nil {
		return "", err
	}
	return clean, nil
}

func normalizeStoragePath(root, requested string) (string, string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(requested) == "" || filepath.IsAbs(requested) ||
		strings.HasPrefix(requested, `\\`) || regexp.MustCompile(`^[a-zA-Z]:[\\/]`).MatchString(requested) {
		return "", "", ErrUnsafeStoragePath
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", ErrUnsafeStoragePath
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", ErrUnsafeStoragePath
	}
	normalized := strings.NewReplacer("\\", string(filepath.Separator), "/", string(filepath.Separator)).Replace(requested)
	clean := filepath.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", ErrUnsafeStoragePath
	}
	return rootResolved, clean, nil
}

func rejectLinkLikeComponents(root, relative string) error {
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		// Windows junctions and other reparse points may be reported as
		// irregular instead of symlinks. Reject both classes conservatively.
		if statErr != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return ErrUnsafeStoragePath
		}
	}
	return nil
}
