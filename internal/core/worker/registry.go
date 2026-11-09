package worker

import (
	"errors"
	"sort"
	"sync"
	"time"

	"videoarchive/internal/core/vocabulary"
)

var (
	ErrWorkerNotConfigured  = errors.New("worker is not configured or is disabled")
	ErrWorkerAuthentication = errors.New("worker service credential is invalid")
	ErrWorkerNotConnected   = errors.New("worker is not connected")
	ErrNoEligibleWorker     = errors.New("no eligible worker is online")
	ErrInvalidSnapshot      = errors.New("worker resource snapshot is invalid")
	ErrStaleSnapshot        = errors.New("worker resource snapshot is stale")
)

type Definition struct {
	ID                string
	Enabled           bool
	CredentialRef     string
	ProcessingKinds   map[vocabulary.ProcessingKind]bool
	AllowedStorageIDs map[string]bool
}

type CredentialVerifier interface {
	Verify(workerID, credentialRef, presentedCredential string) bool
}

type Metrics struct {
	SnapshotID            uint64
	ObservedAt            time.Time
	AverageCPU5m          float64
	AverageGPU5m          float64
	IdleStableFor         time.Duration
	AvailableMemoryBytes  uint64
	AvailableVRAMBytes    uint64
	ScratchFreeBytes      uint64
	HeavyRunning          int
	LightRunning          int
	BusyPhysicalDeviceIDs map[string]bool
	AccessibleStorageIDs  map[string]bool
	AcceptingWork         bool
	StorageObservations   []StorageObservation
	AttachedVolumes       []AttachedVolumeObservation
}

type AttachedVolumeObservation struct {
	PhysicalDeviceID string `json:"physical_device_id"`
	VolumeID         string `json:"volume_id"`
	StorageID        string `json:"storage_id"`
	ArchiveStorageID string `json:"archive_storage_id"`
	Model            string `json:"model"`
	Label            string `json:"label"`
	FileSystem       string `json:"file_system"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	FreeBytes        uint64 `json:"free_bytes"`
}

type MediaFolderObservation struct {
	Folder     string                 `json:"folder"`
	VideoCount int                    `json:"video_count"`
	InputBytes uint64                 `json:"input_bytes"`
	Videos     []MediaFileObservation `json:"videos,omitempty"`
}

// MediaFileObservation is the bounded, root-relative inventory fact shown for
// one source video. It never contains a mount path or StorageRoot.
type MediaFileObservation struct {
	RelativePath string    `json:"relative_path"`
	FileName     string    `json:"file_name"`
	SizeBytes    uint64    `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at,omitempty"`
}

type StorageObservation struct {
	StorageID         string                          `json:"storage_id"`
	Connected         bool                            `json:"connected"`
	AccessKind        vocabulary.StorageAccessKind    `json:"access_kind"`
	DiscoveryState    vocabulary.FolderDiscoveryState `json:"discovery_state"`
	CapacityBytes     uint64                          `json:"capacity_bytes"`
	FreeBytes         uint64                          `json:"free_bytes"`
	DiscoveryRevision uint64                          `json:"discovery_revision"`
	Folders           []MediaFolderObservation        `json:"folders"`
}

type runtimeState struct {
	lastHeartbeat time.Time
	metrics       Metrics
}

type Status struct {
	WorkerID      string
	State         string
	LastHeartbeat time.Time
	Metrics       Metrics
	Capacity      Capacity
}

type Admission struct {
	Available  bool
	WaitReason vocabulary.WaitReason
	Code       string
}

type Capacity struct {
	ReservedMemoryBytes    uint64
	ReservedScratchBytes   uint64
	AvailableMemoryBytes   uint64
	ScratchFreeBytes       uint64
	HeavySlotsUsed         int
	HeavySlotsMaximum      int
	LightSlotsUsed         int
	LightSlotsMaximum      int
	AdditionalWorkPossible bool
	HeavyAdmission         Admission
	LightAdmission         Admission
}

type DispatchCandidate struct {
	Definition    Definition
	LastHeartbeat time.Time
	Metrics       Metrics
}

type Registry struct {
	mu           sync.RWMutex
	definitions  map[string]Definition
	states       map[string]runtimeState
	verifier     CredentialVerifier
	heartbeatTTL time.Duration
}

func NewRegistry(definitions []Definition, verifier CredentialVerifier, heartbeatTTL time.Duration) *Registry {
	configured := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		configured[definition.ID] = definition
	}
	return &Registry{definitions: configured, states: make(map[string]runtimeState), verifier: verifier, heartbeatTTL: heartbeatTTL}
}

func (r *Registry) Connect(workerID, presentedCredential string, now time.Time, metrics Metrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	definition, exists := r.definitions[workerID]
	if !exists || !definition.Enabled {
		return ErrWorkerNotConfigured
	}
	if r.verifier == nil || !r.verifier.Verify(workerID, definition.CredentialRef, presentedCredential) {
		return ErrWorkerAuthentication
	}
	if err := validateMetrics(metrics); err != nil {
		return err
	}
	if current, connected := r.states[workerID]; connected &&
		(metrics.SnapshotID <= current.metrics.SnapshotID || metrics.ObservedAt.Before(current.metrics.ObservedAt)) {
		return ErrStaleSnapshot
	}
	r.states[workerID] = runtimeState{lastHeartbeat: now, metrics: metrics}
	return nil
}

func (r *Registry) Authenticate(workerID, presentedCredential string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, exists := r.definitions[workerID]
	if !exists || !definition.Enabled {
		return ErrWorkerNotConfigured
	}
	if r.verifier == nil || !r.verifier.Verify(workerID, definition.CredentialRef, presentedCredential) {
		return ErrWorkerAuthentication
	}
	return nil
}

func (r *Registry) Heartbeat(workerID string, now time.Time, metrics Metrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, connected := r.states[workerID]
	if !connected {
		return ErrWorkerNotConnected
	}
	if err := validateMetrics(metrics); err != nil {
		return err
	}
	if metrics.SnapshotID <= current.metrics.SnapshotID || metrics.ObservedAt.Before(current.metrics.ObservedAt) {
		return ErrStaleSnapshot
	}
	r.states[workerID] = runtimeState{lastHeartbeat: now, metrics: metrics}
	return nil
}

// UpdateStorageInventory attaches the potentially large folder inventory to
// the exact resource snapshot that announced it. It does not refresh the
// heartbeat TTL and cannot overwrite a newer snapshot.
func (r *Registry) UpdateStorageInventory(workerID string, snapshotID uint64, observations []StorageObservation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, connected := r.states[workerID]
	if !connected {
		return ErrWorkerNotConnected
	}
	if snapshotID != current.metrics.SnapshotID {
		return ErrStaleSnapshot
	}
	next := cloneMetrics(current.metrics)
	next.StorageObservations = cloneStorageObservations(observations)
	if err := validateMetrics(next); err != nil {
		return err
	}
	current.metrics = next
	r.states[workerID] = current
	return nil
}

func (r *Registry) DispatchSnapshot(workerID string, now time.Time) (Metrics, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state, connected := r.states[workerID]
	if !connected || !usableSnapshot(state, now, r.heartbeatTTL) {
		return Metrics{}, false
	}
	return cloneMetrics(state.metrics), true
}

func (r *Registry) DispatchCandidates() []DispatchCandidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	workerIDs := make([]string, 0, len(r.definitions))
	for workerID := range r.definitions {
		workerIDs = append(workerIDs, workerID)
	}
	sort.Strings(workerIDs)
	result := make([]DispatchCandidate, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		state := r.states[workerID]
		result = append(result, DispatchCandidate{
			Definition: cloneDefinition(r.definitions[workerID]), LastHeartbeat: state.lastHeartbeat,
			Metrics: cloneMetrics(state.metrics),
		})
	}
	return result
}

func (r *Registry) Statuses(now time.Time) []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	workerIDs := make([]string, 0, len(r.definitions))
	for workerID := range r.definitions {
		workerIDs = append(workerIDs, workerID)
	}
	sort.Strings(workerIDs)
	statuses := make([]Status, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		definition := r.definitions[workerID]
		status := Status{WorkerID: workerID, State: "OFFLINE"}
		if !definition.Enabled {
			status.State = "DISABLED"
		}
		if state, connected := r.states[workerID]; definition.Enabled && connected {
			status.LastHeartbeat = state.lastHeartbeat
			status.Metrics = cloneMetrics(state.metrics)
			if now.Sub(state.lastHeartbeat) <= r.heartbeatTTL {
				status.State = "ONLINE"
				if state.metrics.HeavyRunning > 0 {
					status.State = "BUSY"
				} else if !state.metrics.AcceptingWork {
					status.State = "DRAINING"
				}
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

type Requirement struct {
	ProcessingKinds []vocabulary.ProcessingKind
	StorageIDs      []string
	PinnedWorkerID  string
}

func (r *Registry) Select(requirement Requirement, now time.Time) (Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	type candidate struct {
		definition  Definition
		activeHeavy int
	}
	var candidates []candidate
	for id, definition := range r.definitions {
		state, connected := r.states[id]
		if !definition.Enabled || !connected || !usableSnapshot(state, now, r.heartbeatTTL) || !state.metrics.AcceptingWork {
			continue
		}
		if requirement.PinnedWorkerID != "" && id != requirement.PinnedWorkerID {
			continue
		}
		if !supports(definition.ProcessingKinds, requirement.ProcessingKinds) ||
			!SupportsConfiguredOrAttachedStorage(definition.AllowedStorageIDs, state.metrics, requirement.StorageIDs) ||
			!supports(state.metrics.AccessibleStorageIDs, requirement.StorageIDs) {
			continue
		}
		candidates = append(candidates, candidate{definition: definition, activeHeavy: state.metrics.HeavyRunning})
	}
	if len(candidates) == 0 {
		return Definition{}, ErrNoEligibleWorker
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activeHeavy == candidates[j].activeHeavy {
			return candidates[i].definition.ID < candidates[j].definition.ID
		}
		return candidates[i].activeHeavy < candidates[j].activeHeavy
	})
	return candidates[0].definition, nil
}

// SupportsConfiguredOrAttachedStorage accepts only boot-authorized Storage or
// role-separated Storage IDs from the Worker's current attached-volume
// observation. Heartbeat accessibility remains an independent requirement.
func SupportsConfiguredOrAttachedStorage(configured map[string]bool, metrics Metrics, required []string) bool {
	attached := make(map[string]bool, len(metrics.AttachedVolumes)*2)
	for _, volume := range metrics.AttachedVolumes {
		attached[volume.StorageID] = true
		attached[volume.ArchiveStorageID] = true
	}
	for _, storageID := range required {
		if !configured[storageID] && !attached[storageID] {
			return false
		}
	}
	return true
}

func supports[T ~string](available map[T]bool, required []T) bool {
	for _, item := range required {
		if !available[item] {
			return false
		}
	}
	return true
}

func cloneMetrics(source Metrics) Metrics {
	accessible := make(map[string]bool, len(source.AccessibleStorageIDs))
	for storageID, available := range source.AccessibleStorageIDs {
		accessible[storageID] = available
	}
	devices := make(map[string]bool, len(source.BusyPhysicalDeviceIDs))
	for deviceID, busy := range source.BusyPhysicalDeviceIDs {
		devices[deviceID] = busy
	}
	observations := cloneStorageObservations(source.StorageObservations)
	attachedVolumes := append([]AttachedVolumeObservation(nil), source.AttachedVolumes...)
	return Metrics{
		SnapshotID: source.SnapshotID, ObservedAt: source.ObservedAt,
		AverageCPU5m: source.AverageCPU5m, AverageGPU5m: source.AverageGPU5m,
		IdleStableFor: source.IdleStableFor, AvailableMemoryBytes: source.AvailableMemoryBytes,
		AvailableVRAMBytes: source.AvailableVRAMBytes, ScratchFreeBytes: source.ScratchFreeBytes,
		HeavyRunning: source.HeavyRunning, LightRunning: source.LightRunning,
		BusyPhysicalDeviceIDs: devices, AccessibleStorageIDs: accessible, AcceptingWork: source.AcceptingWork,
		StorageObservations: observations, AttachedVolumes: attachedVolumes,
	}
}

func cloneStorageObservations(source []StorageObservation) []StorageObservation {
	observations := make([]StorageObservation, 0, len(source))
	for _, observation := range source {
		copy := observation
		copy.Folders = append([]MediaFolderObservation(nil), observation.Folders...)
		for index := range copy.Folders {
			copy.Folders[index].Videos = append([]MediaFileObservation(nil), observation.Folders[index].Videos...)
		}
		observations = append(observations, copy)
	}
	return observations
}

func cloneDefinition(source Definition) Definition {
	processingKinds := make(map[vocabulary.ProcessingKind]bool, len(source.ProcessingKinds))
	for kind, supported := range source.ProcessingKinds {
		processingKinds[kind] = supported
	}
	storageIDs := make(map[string]bool, len(source.AllowedStorageIDs))
	for storageID, allowed := range source.AllowedStorageIDs {
		storageIDs[storageID] = allowed
	}
	source.ProcessingKinds = processingKinds
	source.AllowedStorageIDs = storageIDs
	return source
}

func validateMetrics(metrics Metrics) error {
	if metrics.SnapshotID == 0 || metrics.ObservedAt.IsZero() || metrics.AverageCPU5m < 0 || metrics.AverageCPU5m > 100 ||
		metrics.AverageGPU5m < 0 || metrics.AverageGPU5m > 100 || metrics.IdleStableFor < 0 ||
		metrics.HeavyRunning < 0 || metrics.LightRunning < 0 || metrics.AccessibleStorageIDs == nil ||
		metrics.BusyPhysicalDeviceIDs == nil {
		return ErrInvalidSnapshot
	}
	for storageID, available := range metrics.AccessibleStorageIDs {
		if storageID == "" || !available {
			return ErrInvalidSnapshot
		}
	}
	for deviceID, busy := range metrics.BusyPhysicalDeviceIDs {
		if deviceID == "" || !busy {
			return ErrInvalidSnapshot
		}
	}
	seenStorages := make(map[string]bool, len(metrics.StorageObservations))
	for _, observation := range metrics.StorageObservations {
		if observation.StorageID == "" || seenStorages[observation.StorageID] || !observation.DiscoveryState.Valid() ||
			observation.FreeBytes > observation.CapacityBytes || !metrics.AccessibleStorageIDs[observation.StorageID] {
			return ErrInvalidSnapshot
		}
		if (observation.DiscoveryState == vocabulary.FolderDiscoveryReady) != (observation.DiscoveryRevision != 0) ||
			(observation.DiscoveryState == vocabulary.FolderDiscoveryFailed && len(observation.Folders) != 0) {
			return ErrInvalidSnapshot
		}
		seenStorages[observation.StorageID] = true
		seenFolders := make(map[string]bool, len(observation.Folders))
		for _, folder := range observation.Folders {
			if folder.Folder == "" || folder.VideoCount <= 0 || seenFolders[folder.Folder] {
				return ErrInvalidSnapshot
			}
			seenFolders[folder.Folder] = true
		}
	}
	seenVolumes := make(map[string]bool, len(metrics.AttachedVolumes))
	for _, observation := range metrics.AttachedVolumes {
		if observation.PhysicalDeviceID == "" || observation.VolumeID == "" || observation.StorageID == "" ||
			observation.ArchiveStorageID == "" || observation.ArchiveStorageID == observation.StorageID ||
			observation.Model == "" || observation.FileSystem == "" || observation.CapacityBytes == 0 ||
			observation.FreeBytes > observation.CapacityBytes || seenVolumes[observation.VolumeID] ||
			!metrics.AccessibleStorageIDs[observation.StorageID] || !metrics.AccessibleStorageIDs[observation.ArchiveStorageID] {
			return ErrInvalidSnapshot
		}
		seenVolumes[observation.VolumeID] = true
	}
	return nil
}

func usableSnapshot(state runtimeState, now time.Time, ttl time.Duration) bool {
	if state.lastHeartbeat.IsZero() || state.metrics.ObservedAt.IsZero() || now.Before(state.lastHeartbeat) ||
		now.Before(state.metrics.ObservedAt) {
		return false
	}
	return now.Sub(state.lastHeartbeat) <= ttl && now.Sub(state.metrics.ObservedAt) <= ttl
}
