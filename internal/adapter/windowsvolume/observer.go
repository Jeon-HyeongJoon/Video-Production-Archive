package windowsvolume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/core/device"
)

var ErrObservationFailed = errors.New("windows volume observation failed")

// Probe reports mounted data volumes from the local operating system. Root is
// kept inside the Worker process and is never used as an identity component.
type Probe interface {
	Volumes(context.Context) ([]ProbeVolume, error)
}

type ProbeVolume struct {
	Serial                string
	Model                 string
	PhysicalCapacityBytes uint64
	CapacityBytes         uint64
	FreeBytes             uint64
	VolumeUniqueID        string
	FileSystem            string
	Label                 string
	Root                  string
	BusType               uint32
	RemovableMedia        bool
	SystemVolume          bool
}

type Observer struct {
	mu        sync.Mutex
	probe     Probe
	connected map[string]device.ConnectedVolume
}

func NewObserver(probe Probe) (*Observer, error) {
	if probe == nil {
		return nil, ErrObservationFailed
	}
	return &Observer{probe: probe, connected: make(map[string]device.ConnectedVolume)}, nil
}

func (observer *Observer) Poll(ctx context.Context, workerID string, observedAt time.Time) ([]device.VolumeAttachmentEvent, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || observedAt.IsZero() {
		return nil, ErrObservationFailed
	}
	probed, err := observer.probe.Volumes(ctx)
	if err != nil {
		return nil, errors.Join(ErrObservationFailed, err)
	}
	next := make(map[string]device.ConnectedVolume)
	for _, value := range probed {
		if value.SystemVolume || value.CapacityBytes < 1<<30 || !attachedDataBus(value.BusType, value.RemovableMedia) {
			continue
		}
		serial := strings.TrimSpace(value.Serial)
		model := strings.TrimSpace(value.Model)
		volumeUniqueID := strings.TrimSpace(value.VolumeUniqueID)
		fileSystem := strings.ToUpper(strings.TrimSpace(value.FileSystem))
		root := strings.TrimSpace(value.Root)
		if serial == "" || model == "" || value.PhysicalCapacityBytes == 0 || value.CapacityBytes == 0 || value.FreeBytes > value.CapacityBytes ||
			volumeUniqueID == "" || fileSystem == "" || !validDriveRoot(root) {
			return nil, ErrObservationFailed
		}
		physicalID := stableID("physical", strings.ToUpper(serial), strings.ToUpper(model), strconv.FormatUint(value.PhysicalCapacityBytes, 10))
		volumeID := stableID("volume", physicalID, strings.ToUpper(volumeUniqueID), fileSystem)
		if _, exists := next[volumeID]; exists {
			return nil, ErrObservationFailed
		}
		next[volumeID] = device.ConnectedVolume{
			PhysicalDeviceID:      physicalID,
			VolumeID:              volumeID,
			StorageID:             stableID("source", volumeID),
			ArchiveStorageID:      device.AttachedArchiveStorageID(volumeID),
			Serial:                serial,
			Model:                 model,
			PhysicalCapacityBytes: value.PhysicalCapacityBytes,
			CapacityBytes:         value.CapacityBytes,
			FreeBytes:             value.FreeBytes,
			FileSystem:            fileSystem,
			Label:                 strings.TrimSpace(value.Label),
			Root:                  strings.ToUpper(root[:1]) + `:\`,
			WorkerID:              workerID,
			ObservedAt:            observedAt.UTC(),
		}
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	events := make([]device.VolumeAttachmentEvent, 0)
	for volumeID, current := range next {
		if _, exists := observer.connected[volumeID]; !exists {
			events = append(events, device.VolumeAttachmentEvent{Kind: device.VolumeAttached, ConnectedVolume: current})
		}
	}
	for volumeID, previous := range observer.connected {
		if _, exists := next[volumeID]; !exists {
			previous.ObservedAt = observedAt.UTC()
			events = append(events, device.VolumeAttachmentEvent{Kind: device.VolumeDetached, ConnectedVolume: previous})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Serial != events[j].Serial {
			return events[i].Serial < events[j].Serial
		}
		return events[i].Kind < events[j].Kind
	})
	observer.connected = next
	return events, nil
}

func (observer *Observer) Connected() []device.ConnectedVolume {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	values := make([]device.ConnectedVolume, 0, len(observer.connected))
	for _, value := range observer.connected {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Serial < values[j].Serial })
	return values
}

func attachedDataBus(busType uint32, removable bool) bool {
	if removable {
		return true
	}
	// STORAGE_BUS_TYPE values from ntddstor.h. USB bridges normally expose
	// HDDs as DRIVE_FIXED, so the physical bus is the reliable boundary.
	switch busType {
	case 4, 7, 12, 13: // 1394, USB, SD, MMC
		return true
	default:
		return false
	}
}

func validDriveRoot(root string) bool {
	return len(root) == 3 && root[1] == ':' && (root[2] == '\\' || root[2] == '/') &&
		((root[0] >= 'A' && root[0] <= 'Z') || (root[0] >= 'a' && root[0] <= 'z'))
}

func stableID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}
