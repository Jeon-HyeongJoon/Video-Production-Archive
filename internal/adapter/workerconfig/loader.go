package workerconfig

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/config"
	"videoarchive/internal/core/device"
	"videoarchive/internal/core/transcription"
)

var ErrInvalidConfiguration = errors.New("worker configuration is invalid")

type Configuration struct {
	SchemaVersion        uint32
	ID                   string
	ControlURL           *url.URL
	CredentialReference  string
	HeartbeatInterval    time.Duration
	ClaimInterval        time.Duration
	StorageRoots         []StorageRootConfiguration
	AccessibleStorageIDs []string
	Tools                MediaTools
	ProcessLogs          *ProcessLogConfiguration
}

type ProcessLogConfiguration struct {
	Root          string
	MaxFileBytes  int64
	TailBytes     int
	Retention     time.Duration
	MaxTotalBytes int64
}

type StorageRootConfiguration struct {
	StorageID string
	Root      string
}

type LocalTool struct {
	Executable string
	Version    string
}

type MediaTools struct {
	FFmpeg     LocalTool
	FFprobe    LocalTool
	WhisperCPP *WhisperTool
}

type WhisperTool struct {
	LocalTool
	ModelStorageID    string
	ModelRelativePath string
	Profile           transcription.Profile
}

// StorageRoots owns the filesystem capabilities opened before the Worker
// execution loop starts. Protocol payloads use only their Storage IDs.
type StorageRoots struct {
	updateMu       sync.Mutex
	mu             sync.RWMutex
	roots          map[string]*archive.StorageRoot
	static         map[string]bool
	dynamic        map[string]attachedStorageRoot
	processLogRoot *archive.StorageRoot
}

type attachedStorageRoot struct {
	volumeID string
	root     string
}

type document struct {
	SchemaVersion uint32 `yaml:"schema_version"`
	Worker        struct {
		ID                  string `yaml:"id"`
		ControlURL          string `yaml:"control_url"`
		CredentialReference string `yaml:"credential_ref"`
		HeartbeatInterval   string `yaml:"heartbeat_interval"`
		ClaimInterval       string `yaml:"claim_interval"`
		StorageRoots        []struct {
			StorageID string `yaml:"storage_id"`
			Root      string `yaml:"root"`
		} `yaml:"storage_roots"`
		Tools struct {
			FFmpeg struct {
				Executable string `yaml:"executable"`
				Version    string `yaml:"version"`
			} `yaml:"ffmpeg"`
			FFprobe struct {
				Executable string `yaml:"executable"`
				Version    string `yaml:"version"`
			} `yaml:"ffprobe"`
			WhisperCPP *struct {
				Executable        string `yaml:"executable"`
				Version           string `yaml:"version"`
				ModelStorageID    string `yaml:"model_storage_id"`
				ModelRelativePath string `yaml:"model_relative_path"`
				Profile           struct {
					ID          string `yaml:"id"`
					Version     uint32 `yaml:"version"`
					ModelID     string `yaml:"model_id"`
					Language    string `yaml:"language"`
					ThreadCount int    `yaml:"thread_count"`
				} `yaml:"profile"`
			} `yaml:"whispercpp"`
		} `yaml:"tools"`
		ProcessLogs *processLogDocument `yaml:"process_logs"`
	} `yaml:"worker"`
}

type processLogDocument struct {
	Root          string `yaml:"root"`
	MaxFileBytes  int64  `yaml:"max_file_bytes"`
	TailBytes     int    `yaml:"tail_bytes"`
	Retention     string `yaml:"retention"`
	MaxTotalBytes int64  `yaml:"max_total_bytes"`
}

func Load(path string) (Configuration, error) {
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var source document
	if err := decoder.Decode(&source); err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Configuration{}, fmt.Errorf("%w: configuration must contain one YAML document", ErrInvalidConfiguration)
	}
	return normalize(source)
}

func normalize(source document) (Configuration, error) {
	workerID := strings.TrimSpace(source.Worker.ID)
	credentialReference := strings.TrimSpace(source.Worker.CredentialReference)
	controlURL, err := url.Parse(strings.TrimSpace(source.Worker.ControlURL))
	if source.SchemaVersion != 1 || workerID == "" || !config.ValidSecretReference(credentialReference) ||
		err != nil || controlURL.User != nil || controlURL.RawQuery != "" || controlURL.Fragment != "" ||
		(controlURL.Scheme != "http" && controlURL.Scheme != "https") || controlURL.Host == "" ||
		(controlURL.Path != "" && controlURL.Path != "/") {
		return Configuration{}, ErrInvalidConfiguration
	}
	host := controlURL.Hostname()
	address := net.ParseIP(host)
	if controlURL.Scheme != "https" && (address == nil || !address.IsLoopback()) && host != "localhost" {
		return Configuration{}, ErrInvalidConfiguration
	}
	controlURL.Path = ""
	interval, err := time.ParseDuration(strings.TrimSpace(source.Worker.HeartbeatInterval))
	if err != nil || interval < time.Second {
		return Configuration{}, ErrInvalidConfiguration
	}
	claimInterval := time.Second
	if raw := strings.TrimSpace(source.Worker.ClaimInterval); raw != "" {
		claimInterval, err = time.ParseDuration(raw)
		if err != nil || claimInterval < 250*time.Millisecond {
			return Configuration{}, ErrInvalidConfiguration
		}
	}
	seen := make(map[string]bool, len(source.Worker.StorageRoots))
	storageIDs := make([]string, 0, len(source.Worker.StorageRoots))
	storageRoots := make([]StorageRootConfiguration, 0, len(source.Worker.StorageRoots))
	for _, raw := range source.Worker.StorageRoots {
		storageID := strings.TrimSpace(raw.StorageID)
		root := strings.TrimSpace(raw.Root)
		if storageID == "" || seen[storageID] || root == "" || !filepath.IsAbs(root) {
			return Configuration{}, ErrInvalidConfiguration
		}
		seen[storageID] = true
		storageIDs = append(storageIDs, storageID)
		storageRoots = append(storageRoots, StorageRootConfiguration{StorageID: storageID, Root: filepath.Clean(root)})
	}
	if len(storageIDs) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}
	ffmpeg, err := normalizeLocalTool(source.Worker.Tools.FFmpeg.Executable, source.Worker.Tools.FFmpeg.Version)
	if err != nil {
		return Configuration{}, err
	}
	ffprobe, err := normalizeLocalTool(source.Worker.Tools.FFprobe.Executable, source.Worker.Tools.FFprobe.Version)
	if err != nil {
		return Configuration{}, err
	}
	var whisper *WhisperTool
	if source.Worker.Tools.WhisperCPP != nil {
		raw := source.Worker.Tools.WhisperCPP
		localTool, toolErr := normalizeLocalTool(raw.Executable, raw.Version)
		modelStorageID := strings.TrimSpace(raw.ModelStorageID)
		modelRelativePath, relativeOK := normalizeRelativePath(raw.ModelRelativePath)
		profile, profileErr := transcription.NewProfile(transcription.Profile{
			ID: raw.Profile.ID, Version: raw.Profile.Version, ModelID: raw.Profile.ModelID,
			Language: raw.Profile.Language, ThreadCount: raw.Profile.ThreadCount,
		})
		if toolErr != nil || !seen[modelStorageID] || !relativeOK || profileErr != nil {
			return Configuration{}, ErrInvalidConfiguration
		}
		whisper = &WhisperTool{
			LocalTool: localTool, ModelStorageID: modelStorageID, ModelRelativePath: modelRelativePath, Profile: profile,
		}
	}
	if source.Worker.ProcessLogs == nil {
		return Configuration{}, ErrInvalidConfiguration
	}
	processLogs, err := normalizeProcessLogs(source.Worker.ProcessLogs)
	if err != nil {
		return Configuration{}, err
	}
	return Configuration{
		SchemaVersion: source.SchemaVersion, ID: workerID, ControlURL: controlURL,
		CredentialReference: credentialReference, HeartbeatInterval: interval,
		ClaimInterval:        claimInterval,
		StorageRoots:         storageRoots,
		AccessibleStorageIDs: storageIDs,
		Tools:                MediaTools{FFmpeg: ffmpeg, FFprobe: ffprobe, WhisperCPP: whisper}, ProcessLogs: processLogs,
	}, nil
}

func normalizeProcessLogs(source *processLogDocument) (*ProcessLogConfiguration, error) {
	if source == nil {
		return nil, nil
	}
	root := strings.TrimSpace(source.Root)
	retention, err := time.ParseDuration(strings.TrimSpace(source.Retention))
	if err != nil || !filepath.IsAbs(root) || source.MaxFileBytes < 1024 || source.MaxFileBytes > 64<<20 ||
		source.TailBytes < 512 || source.TailBytes > 16<<10 || retention < time.Hour ||
		source.MaxTotalBytes < source.MaxFileBytes || source.MaxTotalBytes > 16<<30 {
		return nil, ErrInvalidConfiguration
	}
	return &ProcessLogConfiguration{Root: filepath.Clean(root), MaxFileBytes: source.MaxFileBytes, TailBytes: source.TailBytes,
		Retention: retention, MaxTotalBytes: source.MaxTotalBytes}, nil
}

func normalizeLocalTool(executable, version string) (LocalTool, error) {
	executable = strings.TrimSpace(executable)
	version = strings.TrimSpace(version)
	if executable == "" || version == "" || !filepath.IsAbs(executable) {
		return LocalTool{}, ErrInvalidConfiguration
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return LocalTool{}, ErrInvalidConfiguration
	}
	return LocalTool{Executable: filepath.Clean(executable), Version: version}, nil
}

func normalizeRelativePath(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		strings.HasPrefix(clean, "//") || (len(clean) >= 3 && clean[1] == ':' && clean[2] == '/') {
		return "", false
	}
	return clean, clean == normalized
}

func OpenStorageRoots(configuration Configuration) (*StorageRoots, error) {
	if len(configuration.StorageRoots) == 0 || len(configuration.StorageRoots) != len(configuration.AccessibleStorageIDs) {
		return nil, ErrInvalidConfiguration
	}
	accessible := make(map[string]bool, len(configuration.AccessibleStorageIDs))
	for _, raw := range configuration.AccessibleStorageIDs {
		storageID := strings.TrimSpace(raw)
		if storageID == "" || accessible[storageID] {
			return nil, ErrInvalidConfiguration
		}
		accessible[storageID] = true
	}
	opened := &StorageRoots{
		roots:  make(map[string]*archive.StorageRoot, len(configuration.StorageRoots)),
		static: make(map[string]bool, len(configuration.StorageRoots)), dynamic: make(map[string]attachedStorageRoot),
	}
	for _, configured := range configuration.StorageRoots {
		if !accessible[configured.StorageID] || opened.roots[configured.StorageID] != nil || !filepath.IsAbs(configured.Root) {
			opened.Close()
			return nil, ErrInvalidConfiguration
		}
		root, err := archive.OpenStorageRoot(configured.Root)
		if err != nil {
			opened.Close()
			return nil, fmt.Errorf("%w: storage %s: %v", ErrInvalidConfiguration, configured.StorageID, err)
		}
		opened.roots[configured.StorageID] = root
		opened.static[configured.StorageID] = true
	}
	if configuration.ProcessLogs != nil {
		root, err := archive.OpenStorageRoot(configuration.ProcessLogs.Root)
		if err != nil {
			opened.Close()
			return nil, fmt.Errorf("%w: process logs: %v", ErrInvalidConfiguration, err)
		}
		opened.processLogRoot = root
	}
	if tool := configuration.Tools.WhisperCPP; tool != nil {
		modelRoot, ok := opened.Root(tool.ModelStorageID)
		if !ok {
			opened.Close()
			return nil, ErrInvalidConfiguration
		}
		size, _, err := modelRoot.FileSHA256(tool.ModelRelativePath)
		if err != nil || size <= 0 {
			opened.Close()
			return nil, ErrInvalidConfiguration
		}
	}
	return opened, nil
}

func (roots *StorageRoots) Root(storageID string) (*archive.StorageRoot, bool) {
	if roots == nil {
		return nil, false
	}
	roots.mu.RLock()
	defer roots.mu.RUnlock()
	root, ok := roots.roots[strings.TrimSpace(storageID)]
	return root, ok
}

func (roots *StorageRoots) ProcessLogRoot() *archive.StorageRoot {
	if roots == nil {
		return nil
	}
	roots.mu.RLock()
	defer roots.mu.RUnlock()
	return roots.processLogRoot
}

// SyncAttachedVolumes reconciles runtime filesystem capabilities while no
// Step is executing. Calling code owns that lifecycle exclusion. Merely
// opening an os.Root does not inspect or modify files; execution still needs a
// fenced StepExecutionSpec whose Storage IDs match these capabilities.
func (roots *StorageRoots) SyncAttachedVolumes(volumes []device.ConnectedVolume) error {
	if roots == nil {
		return ErrInvalidConfiguration
	}
	type desiredRoot struct {
		storageID string
		attachedStorageRoot
	}
	desired := make(map[string]desiredRoot, len(volumes)*2)
	for _, volume := range volumes {
		volumeID := strings.TrimSpace(volume.VolumeID)
		sourceStorageID := strings.TrimSpace(volume.StorageID)
		archiveStorageID := strings.TrimSpace(volume.ArchiveStorageID)
		localRoot := strings.TrimSpace(volume.Root)
		if volumeID == "" || strings.TrimSpace(volume.PhysicalDeviceID) == "" || sourceStorageID == "" ||
			archiveStorageID == "" || sourceStorageID == archiveStorageID ||
			archiveStorageID != device.AttachedArchiveStorageID(volumeID) || !filepath.IsAbs(localRoot) {
			return ErrInvalidConfiguration
		}
		for _, storageID := range []string{sourceStorageID, archiveStorageID} {
			if _, duplicate := desired[storageID]; duplicate {
				return ErrInvalidConfiguration
			}
			desired[storageID] = desiredRoot{
				storageID:           storageID,
				attachedStorageRoot: attachedStorageRoot{volumeID: volumeID, root: filepath.Clean(localRoot)},
			}
		}
	}

	roots.updateMu.Lock()
	defer roots.updateMu.Unlock()
	roots.mu.RLock()
	current := make(map[string]attachedStorageRoot, len(roots.dynamic))
	for storageID, value := range roots.dynamic {
		current[storageID] = value
	}
	for storageID := range desired {
		if roots.static[storageID] {
			roots.mu.RUnlock()
			return ErrInvalidConfiguration
		}
	}
	roots.mu.RUnlock()

	opened := make(map[string]*archive.StorageRoot)
	for storageID, value := range desired {
		if existing, found := current[storageID]; found && existing == value.attachedStorageRoot {
			continue
		}
		root, err := archive.OpenStorageRoot(value.root)
		if err != nil {
			for _, openedRoot := range opened {
				_ = openedRoot.Close()
			}
			return errors.Join(ErrInvalidConfiguration, err)
		}
		opened[storageID] = root
	}

	var closing []*archive.StorageRoot
	roots.mu.Lock()
	for storageID, value := range current {
		next, found := desired[storageID]
		if found && next.attachedStorageRoot == value {
			continue
		}
		if root := roots.roots[storageID]; root != nil {
			closing = append(closing, root)
		}
		delete(roots.roots, storageID)
		delete(roots.dynamic, storageID)
	}
	for storageID, root := range opened {
		roots.roots[storageID] = root
		roots.dynamic[storageID] = desired[storageID].attachedStorageRoot
	}
	roots.mu.Unlock()
	var closeErrors []error
	for _, root := range closing {
		closeErrors = append(closeErrors, root.Close())
	}
	return errors.Join(closeErrors...)
}

// MustRoot is for operation handlers after Registry validation has proved the
// Storage ID exists. It returns nil for an invalid call instead of widening the
// filesystem boundary.
func (roots *StorageRoots) MustRoot(storageID string) *archive.StorageRoot {
	root, _ := roots.Root(storageID)
	return root
}

func (roots *StorageRoots) ExternalProcessPath(storageID, relativePath string) (string, error) {
	root, ok := roots.Root(storageID)
	if !ok {
		return "", ErrInvalidConfiguration
	}
	return root.ExternalProcessPath(relativePath)
}

func (roots *StorageRoots) Close() error {
	if roots == nil {
		return nil
	}
	roots.updateMu.Lock()
	defer roots.updateMu.Unlock()
	roots.mu.Lock()
	ids := make([]string, 0, len(roots.roots))
	for storageID := range roots.roots {
		ids = append(ids, storageID)
	}
	sort.Strings(ids)
	opened := roots.roots
	processLogRoot := roots.processLogRoot
	roots.processLogRoot = nil
	roots.roots = make(map[string]*archive.StorageRoot)
	roots.dynamic = make(map[string]attachedStorageRoot)
	roots.static = make(map[string]bool)
	roots.mu.Unlock()
	var closeErrors []error
	for _, storageID := range ids {
		if err := opened[storageID].Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if processLogRoot != nil {
		closeErrors = append(closeErrors, processLogRoot.Close())
	}
	return errors.Join(closeErrors...)
}
