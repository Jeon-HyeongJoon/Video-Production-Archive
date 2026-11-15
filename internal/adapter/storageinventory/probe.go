package storageinventory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path"
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
	mu          sync.Mutex
	states      map[string]*attachedScanState
	nextAttempt uint64
	refresh     time.Duration
	now         func() time.Time
	scan        attachedScanFunc
}

type attachedScanState struct {
	observation worker.StorageObservation
	ready       bool
	scanning    bool
	lastAttempt time.Time
	attempt     uint64
	volume      device.ConnectedVolume
	cancel      context.CancelFunc
}

type attachedScanTask struct {
	storageID string
	volume    device.ConnectedVolume
	attempt   uint64
	ctx       context.Context
}

type attachedScanFunc func(context.Context, attachedScanTask, func([]worker.MediaFolderObservation)) (worker.StorageObservation, error)

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
	return &AttachedProbe{states: make(map[string]*attachedScanState), refresh: 30 * time.Second, now: time.Now, scan: scanAttachedRoot}
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
		observation := worker.StorageObservation{
			StorageID: configured.StorageID, AccessKind: accessKind, DiscoveryState: vocabulary.FolderDiscoveryFailed,
		}
		capacity, free, capacityErr := storageCapacity(configured.Root)
		root, rootFound := probe.roots.Root(configured.StorageID)
		if capacityErr != nil || !rootFound {
			result = append(result, observation)
			continue
		}
		observed, observeErr := observeRoot(ctx, observation, root, capacity, free, nil)
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
	if probe == nil || probe.now == nil || probe.refresh <= 0 || probe.scan == nil {
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
	for storageID, state := range probe.states {
		_, connectedNow := connected[storageID]
		if !selected[storageID] || !connectedNow {
			if state.cancel != nil {
				state.cancel()
			}
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
		if state != nil && !sameConnectedVolume(state.volume, volume) {
			if state.cancel != nil {
				state.cancel()
			}
			delete(probe.states, storageID)
			state = nil
		}
		if state == nil {
			state = &attachedScanState{observation: scanningObservation(storageID, volume), volume: volume}
			probe.states[storageID] = state
		}
		state.volume = volume
		state.observation.CapacityBytes = volume.CapacityBytes
		state.observation.FreeBytes = volume.FreeBytes
		retryDue := state.observation.DiscoveryState != vocabulary.FolderDiscoveryFailed || now.Sub(state.lastAttempt) >= probe.refresh
		if !state.scanning && ((state.ready && now.Sub(state.lastAttempt) >= probe.refresh) || (!state.ready && retryDue)) {
			scanContext, cancel := context.WithCancel(ctx)
			state.scanning = true
			state.lastAttempt = now
			probe.nextAttempt++
			state.attempt = probe.nextAttempt
			state.cancel = cancel
			if !state.ready {
				state.observation = scanningObservation(storageID, volume)
			}
			tasks = append(tasks, attachedScanTask{storageID: storageID, volume: volume, attempt: state.attempt, ctx: scanContext})
		}
		result = append(result, cloneObservation(state.observation))
	}
	probe.mu.Unlock()
	for _, task := range tasks {
		go probe.runScan(task)
	}
	return result, nil
}

func scanningObservation(storageID string, volume device.ConnectedVolume) worker.StorageObservation {
	return worker.StorageObservation{
		StorageID: storageID, Connected: true, AccessKind: vocabulary.StorageAccessAttachedVolume,
		DiscoveryState: vocabulary.FolderDiscoveryScanning,
		CapacityBytes:  volume.CapacityBytes, FreeBytes: volume.FreeBytes,
	}
}

func cloneObservation(observation worker.StorageObservation) worker.StorageObservation {
	observation.Folders = append([]worker.MediaFolderObservation(nil), observation.Folders...)
	return observation
}

func (probe *AttachedProbe) runScan(task attachedScanTask) {
	observation, err := probe.scan(task.ctx, task, func(folders []worker.MediaFolderObservation) {
		probe.progress(task, folders)
	})
	probe.finish(task, observation, err == nil)
}

func scanAttachedRoot(ctx context.Context, task attachedScanTask, report func([]worker.MediaFolderObservation)) (worker.StorageObservation, error) {
	root, err := archive.OpenReadOnlyStorageRoot(task.volume.Root)
	if err != nil {
		return worker.StorageObservation{}, ErrObservationFailed
	}
	observation, observeErr := observeRoot(ctx, scanningObservation(task.storageID, task.volume), root,
		task.volume.CapacityBytes, task.volume.FreeBytes, report)
	closeErr := root.Close()
	if observeErr != nil || closeErr != nil {
		return worker.StorageObservation{}, ErrObservationFailed
	}
	return observation, nil
}

func (probe *AttachedProbe) progress(task attachedScanTask, folders []worker.MediaFolderObservation) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	state := probe.states[task.storageID]
	if state == nil || state.attempt != task.attempt || state.ready || !state.scanning || task.ctx.Err() != nil {
		return
	}
	state.observation.Folders = append([]worker.MediaFolderObservation(nil), folders...)
}

func (probe *AttachedProbe) finish(task attachedScanTask, observation worker.StorageObservation, succeeded bool) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	state := probe.states[task.storageID]
	if state == nil || state.attempt != task.attempt {
		return
	}
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	state.scanning = false
	if succeeded {
		state.observation = cloneObservation(observation)
		state.ready = true
	} else if !state.ready {
		state.observation = scanningObservation(task.storageID, task.volume)
		state.observation.DiscoveryState = vocabulary.FolderDiscoveryFailed
		state.observation.Folders = nil
	}
}

func observeRoot(ctx context.Context, observation worker.StorageObservation, root inventoryRoot, capacity, free uint64, report func([]worker.MediaFolderObservation)) (worker.StorageObservation, error) {
	if root == nil || capacity == 0 || free > capacity {
		return observation, ErrObservationFailed
	}
	scanner, err := mediafolder.NewScanner(root)
	if err != nil {
		return observation, ErrObservationFailed
	}
	var progress func([]mediafolder.Folder)
	if report != nil {
		progress = func(folders []mediafolder.Folder) { report(summarizeFolders(folders)) }
	}
	folders, err := scanner.DiscoverWithProgressContext(ctx, progress)
	if err != nil {
		return observation, ErrObservationFailed
	}
	observation.Connected = true
	observation.DiscoveryState = vocabulary.FolderDiscoveryReady
	observation.CapacityBytes = capacity
	observation.FreeBytes = free
	digest := sha256.New()
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return observation, err
		}
		var inputBytes uint64
		videos := make([]worker.MediaFileObservation, 0, len(folder.VideoPaths))
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
			videos = append(videos, worker.MediaFileObservation{
				RelativePath: relativePath, FileName: path.Base(relativePath),
				SizeBytes: uint64(info.Size()), ModifiedAt: info.ModTime().UTC(),
			})
			digest.Write([]byte(relativePath))
			digest.Write([]byte{0})
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(info.Size()))
			digest.Write(size[:])
		}
		observation.Folders = append(observation.Folders, worker.MediaFolderObservation{
			Folder: folder.RelativePath, VideoCount: len(folder.VideoPaths), InputBytes: inputBytes, Videos: videos,
		})
	}
	sum := digest.Sum(nil)
	observation.DiscoveryRevision = binary.BigEndian.Uint64(sum[:8]) & ((1 << 53) - 1)
	if observation.DiscoveryRevision == 0 {
		observation.DiscoveryRevision = 1
	}
	return observation, nil
}

func sameConnectedVolume(left, right device.ConnectedVolume) bool {
	return left.StorageID == right.StorageID && left.VolumeID == right.VolumeID &&
		left.PhysicalDeviceID == right.PhysicalDeviceID && strings.EqualFold(left.Root, right.Root)
}

func summarizeFolders(folders []mediafolder.Folder) []worker.MediaFolderObservation {
	result := make([]worker.MediaFolderObservation, 0, len(folders))
	for _, folder := range folders {
		videos := make([]worker.MediaFileObservation, 0, len(folder.VideoPaths))
		for _, relativePath := range folder.VideoPaths {
			videos = append(videos, worker.MediaFileObservation{RelativePath: relativePath, FileName: path.Base(relativePath)})
		}
		result = append(result, worker.MediaFolderObservation{Folder: folder.RelativePath, VideoCount: len(folder.VideoPaths), Videos: videos})
	}
	return result
}

func networkRoot(root string) bool {
	trimmed := strings.TrimSpace(root)
	return strings.HasPrefix(trimmed, `\\`) || strings.HasPrefix(trimmed, "//")
}
