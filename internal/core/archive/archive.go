package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sync"
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
	ErrStorageUnavailable = errors.New("registered storage is unavailable")
	ErrStorageFull        = errors.New("registered storage has insufficient space")
	ErrInvalidSnapshot    = errors.New("archive snapshot is invalid")
)

type PackageState string

const (
	PackageStored    PackageState = "STORED"
	PackageCommitted PackageState = "COMMITTED"
)

func (state PackageState) Valid() bool {
	return state == PackageStored || state == PackageCommitted
}

type OriginalAsset = asset.OriginalAsset

type Supersession = asset.Supersession

func SupersedeOriginal(previous, replacement OriginalAsset) (Supersession, error) {
	return asset.SupersedeOriginal(previous, replacement)
}

type Provenance struct {
	OriginalAssetID      string
	RootOriginalAssetIDs []string
	InputTimelineID      string
	InputArtifactIDs     []string
	ProcessingKind       vocabulary.ProcessingKind
	ProcessorVersion     string
	ProfileVersion       string
	CreatedBy            string
	CreatedAt            time.Time
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
var windowsRequestedDrivePattern = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)

func (a Artifact) ValidateDerived() error {
	if a.ID == "" || a.Path == "" || a.Size < 0 || !sha256Pattern.MatchString(a.Checksum) ||
		a.Origin.InputArtifactIDs == nil || !a.Origin.ProcessingKind.Valid() || a.Origin.ProcessorVersion == "" ||
		a.Origin.ProfileVersion == "" || a.Origin.CreatedBy == "" || a.Origin.CreatedAt.IsZero() {
		return ErrInvalidProvenance
	}
	if _, err := normalizeProvenance(a.Origin); err != nil {
		return err
	}
	return nil
}

func normalizeProvenance(source Provenance) (Provenance, error) {
	provenance := source
	provenance.RootOriginalAssetIDs = append([]string(nil), source.RootOriginalAssetIDs...)
	singleOriginal := provenance.OriginalAssetID != "" && len(provenance.RootOriginalAssetIDs) == 0 && provenance.InputTimelineID == ""
	multipleOriginals := provenance.OriginalAssetID == "" && len(provenance.RootOriginalAssetIDs) >= 2 && provenance.InputTimelineID != ""
	if !singleOriginal && !multipleOriginals {
		return Provenance{}, ErrInvalidProvenance
	}
	if multipleOriginals {
		seen := make(map[string]bool, len(provenance.RootOriginalAssetIDs))
		for _, assetID := range provenance.RootOriginalAssetIDs {
			if assetID == "" || seen[assetID] {
				return Provenance{}, ErrInvalidProvenance
			}
			seen[assetID] = true
		}
		sort.Strings(provenance.RootOriginalAssetIDs)
	}
	return provenance, nil
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
	normalized, err := normalizeSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func ImportSnapshot(payload []byte) (ArchiveSnapshot, error) {
	var snapshot ArchiveSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ArchiveSnapshot{}, ErrInvalidSnapshot
	}
	normalized, err := normalizeSnapshot(snapshot)
	if err != nil {
		return ArchiveSnapshot{}, err
	}
	return normalized, nil
}

func normalizeSnapshot(snapshot ArchiveSnapshot) (ArchiveSnapshot, error) {
	normalized := ArchiveSnapshot{
		OriginalAssets: append([]OriginalAsset(nil), snapshot.OriginalAssets...),
		Artifacts:      append([]Artifact(nil), snapshot.Artifacts...),
	}
	assetIDs := make(map[string]bool, len(snapshot.OriginalAssets))
	for _, asset := range normalized.OriginalAssets {
		if asset.Validate() != nil || assetIDs[asset.ID] {
			return ArchiveSnapshot{}, ErrInvalidSnapshot
		}
		assetIDs[asset.ID] = true
	}
	artifactIDs := make(map[string]bool, len(normalized.Artifacts))
	for index, artifact := range normalized.Artifacts {
		provenance, err := normalizeProvenance(artifact.Origin)
		if artifactIDs[artifact.ID] || artifact.ValidateDerived() != nil || err != nil {
			return ArchiveSnapshot{}, ErrInvalidSnapshot
		}
		if provenance.OriginalAssetID != "" {
			if !assetIDs[provenance.OriginalAssetID] {
				return ArchiveSnapshot{}, ErrInvalidSnapshot
			}
		} else {
			for _, assetID := range provenance.RootOriginalAssetIDs {
				if !assetIDs[assetID] {
					return ArchiveSnapshot{}, ErrInvalidSnapshot
				}
			}
		}
		artifactIDs[artifact.ID] = true
		normalized.Artifacts[index].Origin = provenance
	}
	return normalized, nil
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

type StorageLock struct {
	file         *os.File
	releaseLocal func()
}

type processStorageLockEntry struct {
	semaphore  chan struct{}
	references int
}

var processStorageLocks = struct {
	sync.Mutex
	entries map[string]*processStorageLockEntry
}{entries: make(map[string]*processStorageLockEntry)}

// ReadOnlyStorageRoot deliberately exposes no write or rename operations. It
// is the filesystem capability handed to a selected ArchiveIntakeSession.
type ReadOnlyStorageRoot struct {
	root *StorageRoot
}

func OpenStorageRoot(root string) (*StorageRoot, error) {
	rootResolved, _, err := normalizeStoragePath(root, "storage-root-check")
	if err != nil {
		return nil, ErrUnsafeStoragePath
	}
	handle, err := os.OpenRoot(rootResolved)
	if err != nil {
		return nil, storageIOError(err)
	}
	return &StorageRoot{root: handle}, nil
}

func OpenReadOnlyStorageRoot(root string) (*ReadOnlyStorageRoot, error) {
	storageRoot, err := OpenStorageRoot(root)
	if err != nil {
		return nil, err
	}
	return &ReadOnlyStorageRoot{root: storageRoot}, nil
}

func (root *ReadOnlyStorageRoot) Close() error {
	if root == nil || root.root == nil {
		return ErrUnsafeStoragePath
	}
	return root.root.Close()
}

func (root *ReadOnlyStorageRoot) Open(requested string) (*os.File, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	return root.root.Open(requested)
}

func (root *ReadOnlyStorageRoot) ReadFile(requested string) ([]byte, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	return root.root.ReadFile(requested)
}

func (root *ReadOnlyStorageRoot) ReadDir(requested string) ([]os.DirEntry, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	return root.root.ReadDir(requested)
}

func (root *ReadOnlyStorageRoot) FileSHA256(requested string) (int64, string, error) {
	if root == nil || root.root == nil {
		return 0, "", ErrUnsafeStoragePath
	}
	return root.root.FileSHA256(requested)
}

// ExternalProcessPath resolves a subprocess input beneath the read-only
// capability. The subprocess must not receive any writable output path under
// this root.
func (root *ReadOnlyStorageRoot) ExternalProcessPath(requested string) (string, error) {
	if root == nil || root.root == nil {
		return "", ErrUnsafeStoragePath
	}
	return root.root.ExternalProcessPath(requested)
}

// Overlaps reports whether capabilities address the same directory tree.
func (root *StorageRoot) Overlaps(other *StorageRoot) bool {
	for _, pair := range [][2]string{{root.root.Name(), other.root.Name()}, {other.root.Name(), root.root.Name()}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (root *StorageRoot) Close() error { return root.root.Close() }

// ExternalProcessPath resolves a normalized relative path beneath this
// capability for an argument-array subprocess. Callers must keep every
// filesystem operation around the subprocess on the same StorageRoot.
func (root *StorageRoot) ExternalProcessPath(requested string) (string, error) {
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
	return filepath.Join(root.root.Name(), filepath.FromSlash(clean)), nil
}

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
		return nil, storageIOError(err)
	}
	return file, nil
}

// Create opens a root-confined file for streaming output. The caller owns
// Sync and Close; all path resolution stays inside this StorageRoot.
func (root *StorageRoot) Create(requested string, permission os.FileMode) (*os.File, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	clean, err := root.safeRelative(requested)
	if err != nil {
		return nil, err
	}
	file, err := root.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permission)
	if err != nil {
		return nil, storageIOError(err)
	}
	if err := root.syncParent(clean); err != nil {
		_ = file.Close()
		_ = root.root.Remove(clean)
		return nil, err
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

func (root *StorageRoot) ReadDir(requested string) ([]os.DirEntry, error) {
	if root == nil || root.root == nil {
		return nil, ErrUnsafeStoragePath
	}
	clean := "."
	if strings.TrimSpace(requested) != "" && strings.TrimSpace(requested) != "." {
		var err error
		clean, err = root.safeRelative(requested)
		if err != nil {
			return nil, err
		}
	}
	directory, err := root.root.Open(clean)
	if err != nil {
		return nil, storageIOError(err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, storageIOError(err)
	}
	return entries, nil
}

func (root *StorageRoot) WriteFile(requested string, data []byte, permission os.FileMode) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	file, err := root.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permission)
	if err != nil {
		return storageIOError(err)
	}
	written, writeErr := file.Write(data)
	syncErr := error(nil)
	if writeErr == nil && written == len(data) {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || written != len(data) || syncErr != nil || closeErr != nil {
		_ = root.root.Remove(clean)
		cause := errors.Join(writeErr, syncErr, closeErr)
		if written != len(data) {
			cause = errors.Join(cause, io.ErrShortWrite)
		}
		return storageIOError(cause)
	}
	return root.syncParent(clean)
}

func (root *StorageRoot) CopyFileFrom(source *StorageRoot, sourcePath, destinationPath string, permission os.FileMode) (int64, string, error) {
	if source == nil {
		return 0, "", ErrUnsafeStoragePath
	}
	input, err := source.Open(sourcePath)
	if err != nil {
		return 0, "", err
	}
	defer input.Close()
	destination, err := root.safeRelative(destinationPath)
	if err != nil {
		return 0, "", err
	}
	output, err := root.root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permission)
	if err != nil {
		return 0, "", storageIOError(err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	syncErr := error(nil)
	if copyErr == nil {
		syncErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = root.root.Remove(destination)
		return 0, "", storageIOError(errors.Join(copyErr, syncErr, closeErr))
	}
	if err := root.syncParent(destination); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func (root *StorageRoot) FileSHA256(requested string) (int64, string, error) {
	file, err := root.Open(requested)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", storageIOError(err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// Accessible checks whether the configured directory still names the same registered root.
func (root *StorageRoot) Accessible() bool {
	if root == nil || root.root == nil {
		return false
	}
	configured, err := os.OpenRoot(root.root.Name())
	if err != nil {
		return false
	}
	defer configured.Close()
	expected, err := root.root.Stat(".")
	if err != nil {
		return false
	}
	current, err := configured.Stat(".")
	if err != nil || !os.SameFile(expected, current) {
		return false
	}
	dir, err := configured.Open(".")
	if err != nil {
		return false
	}
	defer dir.Close()
	_, err = dir.ReadDir(1)
	return err == nil || errors.Is(err, io.EOF)
}

// Stat observes an entry through the registered root without opening its content.
func (root *StorageRoot) Stat(requested string) (os.FileInfo, error) {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return nil, err
	}
	info, err := root.root.Stat(clean)
	if err != nil {
		return nil, storageIOError(err)
	}
	return info, nil
}

func (root *StorageRoot) Exists(requested string) (bool, error) {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return false, err
	}
	_, err = root.root.Stat(clean)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, ErrUnsafeStoragePath
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
	if err := root.syncRelative(oldClean); err != nil {
		return err
	}
	if err := root.root.Rename(oldClean, newClean); err != nil {
		return storageIOError(err)
	}
	if err := root.syncParent(newClean); err != nil {
		return err
	}
	if filepath.Dir(oldClean) != filepath.Dir(newClean) {
		if err := root.syncParent(oldClean); err != nil {
			return err
		}
	}
	return nil
}

func (root *StorageRoot) syncParent(clean string) error {
	parent := filepath.Dir(clean)
	if parent == "" {
		parent = "."
	}
	return root.syncRelative(parent)
}

func (root *StorageRoot) syncRelative(clean string) error {
	if root == nil || root.root == nil {
		return ErrUnsafeStoragePath
	}
	info, err := root.root.Stat(clean)
	if err != nil {
		return storageIOError(err)
	}
	var file *os.File
	if info.IsDir() {
		file, err = root.root.Open(clean)
	} else {
		// FlushFileBuffers requires a writable Windows handle. Outputs passed to
		// this boundary are temporary files created beneath this StorageRoot.
		file, err = root.root.OpenFile(clean, os.O_RDWR, 0)
	}
	if err != nil {
		return storageIOError(err)
	}
	syncErr := error(nil)
	syncErr = syncStorageFile(file, info.IsDir())
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return storageIOError(errors.Join(syncErr, closeErr))
	}
	return nil
}

func (root *StorageRoot) Remove(requested string) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	if err := root.root.Remove(clean); err != nil {
		return storageIOError(err)
	}
	return nil
}

func (root *StorageRoot) RemoveAll(requested string) error {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return err
	}
	if err := root.root.RemoveAll(clean); err != nil {
		return storageIOError(err)
	}
	return nil
}

func (root *StorageRoot) AcquireExclusiveLock(ctx context.Context, requested string) (*StorageLock, error) {
	clean, err := root.safeRelative(requested)
	if err != nil {
		return nil, err
	}
	key := filepath.Join(root.root.Name(), clean)
	releaseLocal, err := acquireProcessStorageLock(ctx, key)
	if err != nil {
		return nil, err
	}
	file, err := root.root.OpenFile(clean, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseLocal()
		return nil, ErrUnsafeStoragePath
	}
	for {
		locked, lockErr := tryExclusiveFileLock(file)
		if lockErr != nil {
			file.Close()
			releaseLocal()
			return nil, ErrUnsafeStoragePath
		}
		if locked {
			return &StorageLock{file: file, releaseLocal: releaseLocal}, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			releaseLocal()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func acquireProcessStorageLock(ctx context.Context, key string) (func(), error) {
	processStorageLocks.Lock()
	entry := processStorageLocks.entries[key]
	if entry == nil {
		entry = &processStorageLockEntry{semaphore: make(chan struct{}, 1)}
		entry.semaphore <- struct{}{}
		processStorageLocks.entries[key] = entry
	}
	entry.references++
	processStorageLocks.Unlock()
	select {
	case <-ctx.Done():
		releaseProcessStorageLockReference(key, entry)
		return nil, ctx.Err()
	case <-entry.semaphore:
		var once sync.Once
		return func() {
			once.Do(func() {
				entry.semaphore <- struct{}{}
				releaseProcessStorageLockReference(key, entry)
			})
		}, nil
	}
}

func releaseProcessStorageLockReference(key string, entry *processStorageLockEntry) {
	processStorageLocks.Lock()
	defer processStorageLocks.Unlock()
	entry.references--
	if entry.references == 0 && processStorageLocks.entries[key] == entry {
		delete(processStorageLocks.entries, key)
	}
}

func (lock *StorageLock) Close() error {
	if lock == nil || lock.file == nil {
		return ErrUnsafeStoragePath
	}
	unlockErr := unlockFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	if lock.releaseLocal != nil {
		lock.releaseLocal()
		lock.releaseLocal = nil
	}
	if unlockErr != nil || closeErr != nil {
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
		strings.HasPrefix(requested, `/`) || strings.HasPrefix(requested, `\`) ||
		windowsRequestedDrivePattern.MatchString(requested) {
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
