package storageinventory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/adapter/mediafolder"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/device"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

var ErrObservationFailed = errors.New("storage inventory observation failed")

type Probe struct {
	configuration workerconfig.Configuration
	roots         *workerconfig.StorageRoots
}

type AttachedProbe struct {
	mu      sync.Mutex
	states  map[string]*attachedScanState
	refresh time.Duration
	now     func() time.Time
}

type attachedScanState struct {
	observation worker.StorageObservation
	ready       bool
	scanning    bool
	lastAttempt time.Time
}

type attachedScanTask struct {
	storageID string
	volume    device.ConnectedVolume
}

type inventoryRoot interface {
	ReadDir(string) ([]fs.DirEntry, error)
	Open(string) (*os.File, error)
}

func New(configuration workerconfig.Configuration, roots *workerconfig.StorageRoots) (*Probe, error) {
	if roots == nil || len(configuration.StorageRoots) == 0 {
		return nil, ErrObservationFailed
	}
	return &Probe{configuration: configuration, roots: roots}, nil
}

func NewAttachedProbe() *AttachedProbe {
	return &AttachedProbe{states: make(map[string]*attachedScanState), refresh: 30 * time.Second, now: time.Now}
}

func (probe *Probe) Observe(ctx context.Context) ([]worker.StorageObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]worker.StorageObservation, 0, len(probe.configuration.StorageRoots))
	for _, configured := range probe.configuration.StorageRoots {
		accessKind := vocabulary.StorageAccessLocal
		if networkRoot(configured.Root) {
			accessKind = vocabulary.StorageAccessMountedNetwork
		}
		observation := worker.StorageObservation{StorageID: configured.StorageID, AccessKind: accessKind, DiscoveryRevision: 1}
		capacity, free, capacityErr := storageCapacity(configured.Root)
		root, rootFound := probe.roots.Root(configured.StorageID)
		if capacityErr != nil || !rootFound {
			result = append(result, observation)
			continue
		}
		observed, observeErr := observeRoot(ctx, observation, root, capacity, free)
		if observeErr != nil {
			result = append(result, observation)
			continue
		}
		result = append(result, observed)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StorageID < result[j].StorageID })
	return result, nil
}

func (probe *AttachedProbe) Observe(ctx context.Context, selectedStorageIDs []string, volumes []device.ConnectedVolume) ([]worker.StorageObservation, error) {
	if probe == nil || probe.now == nil || probe.refresh <= 0 {
		return nil, ErrObservationFailed
	}
	connected := make(map[string]device.ConnectedVolume, len(volumes))
	for _, volume := range volumes {
		connected[volume.StorageID] = volume
	}
	selected := make(map[string]bool, len(selectedStorageIDs))
	ordered := append([]string(nil), selectedStorageIDs...)
	sort.Strings(ordered)
	for _, storageID := range ordered {
		storageID = strings.TrimSpace(storageID)
		if storageID == "" || selected[storageID] {
			return nil, ErrObservationFailed
		}
		selected[storageID] = true
	}
	now := probe.now().UTC()
	result := make([]worker.StorageObservation, 0, len(ordered))
	tasks := make([]attachedScanTask, 0, len(ordered))
	probe.mu.Lock()
	for storageID := range probe.states {
		if !selected[storageID] {
			delete(probe.states, storageID)
		}
	}
	for _, storageID := range ordered {
		storageID = strings.TrimSpace(storageID)
		volume, found := connected[storageID]
		if !found {
			continue
		}
		state := probe.states[storageID]
		if state == nil {
			state = &attachedScanState{}
			probe.states[storageID] = state
		}
		if state.ready {
			result = append(result, state.observation)
		}
		if !state.scanning && (!state.ready || now.Sub(state.lastAttempt) >= probe.refresh) {
			state.scanning = true
			state.lastAttempt = now
			tasks = append(tasks, attachedScanTask{storageID: storageID, volume: volume})
		}
	}
	probe.mu.Unlock()
	for _, task := range tasks {
		go probe.scan(ctx, task)
	}
	return result, nil
}

func (probe *AttachedProbe) scan(ctx context.Context, task attachedScanTask) {
	root, err := archive.OpenReadOnlyStorageRoot(task.volume.Root)
	if err != nil {
		probe.finish(task.storageID, worker.StorageObservation{}, false)
		return
	}
	observation, observeErr := observeRoot(ctx, worker.StorageObservation{
		StorageID: task.storageID, AccessKind: vocabulary.StorageAccessAttachedVolume, DiscoveryRevision: 1,
	}, root, task.volume.CapacityBytes, task.volume.FreeBytes)
	closeErr := root.Close()
	probe.finish(task.storageID, observation, observeErr == nil && closeErr == nil)
}

func (probe *AttachedProbe) finish(storageID string, observation worker.StorageObservation, succeeded bool) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	state := probe.states[storageID]
	if state == nil {
		return
	}
	state.scanning = false
	if succeeded {
		state.observation = observation
		state.ready = true
	}
}

func observeRoot(ctx context.Context, observation worker.StorageObservation, root inventoryRoot, capacity, free uint64) (worker.StorageObservation, error) {
	if root == nil || capacity == 0 || free > capacity {
		return observation, ErrObservationFailed
	}
	scanner, err := mediafolder.NewScanner(root)
	if err != nil {
		return observation, ErrObservationFailed
	}
	folders, err := scanner.Discover()
	if err != nil {
		return observation, ErrObservationFailed
	}
	observation.Connected = true
	observation.CapacityBytes = capacity
	observation.FreeBytes = free
	digest := sha256.New()
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return observation, err
		}
		var inputBytes uint64
		for _, relativePath := range folder.VideoPaths {
			file, openErr := root.Open(relativePath)
			if openErr != nil {
				return observation, ErrObservationFailed
			}
			info, statErr := file.Stat()
			closeErr := file.Close()
			if statErr != nil || closeErr != nil || info.Size() < 0 {
				return observation, ErrObservationFailed
			}
			inputBytes += uint64(info.Size())
			digest.Write([]byte(relativePath))
			digest.Write([]byte{0})
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(info.Size()))
			digest.Write(size[:])
		}
		observation.Folders = append(observation.Folders, worker.MediaFolderObservation{
			Folder: folder.RelativePath, VideoCount: len(folder.VideoPaths), InputBytes: inputBytes,
		})
	}
	sum := digest.Sum(nil)
	observation.DiscoveryRevision = binary.BigEndian.Uint64(sum[:8]) & ((1 << 53) - 1)
	if observation.DiscoveryRevision == 0 {
		observation.DiscoveryRevision = 1
	}
	return observation, nil
}

func networkRoot(root string) bool {
	trimmed := strings.TrimSpace(root)
	return strings.HasPrefix(trimmed, `\\`) || strings.HasPrefix(trimmed, "//")
}
