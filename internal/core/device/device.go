package device

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/core/confirmation"
)

type VolumeAttachmentKind string

const (
	VolumeAttached VolumeAttachmentKind = "ATTACHED"
	VolumeDetached VolumeAttachmentKind = "DETACHED"
)

type ConnectedVolume struct {
	PhysicalDeviceID      string
	VolumeID              string
	StorageID             string
	ArchiveStorageID      string
	Serial                string
	Model                 string
	PhysicalCapacityBytes uint64
	CapacityBytes         uint64
	FreeBytes             uint64
	FileSystem            string
	Label                 string
	Root                  string
	WorkerID              string
	ObservedAt            time.Time
}

// AttachedArchiveStorageID returns the stable ARCHIVE_PRIMARY namespace for
// one attached logical Volume. It is deliberately different from the
// SOURCE_MEDIA Storage ID observed for the same Volume: Storage role is part
// of the capability boundary even when both namespaces resolve to one mount.
func AttachedArchiveStorageID(volumeID string) string {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("attached-archive\x00" + volumeID))
	return "archive-" + hex.EncodeToString(digest[:12])
}

type VolumeAttachmentEvent struct {
	Kind VolumeAttachmentKind
	ConnectedVolume
}

type LockMode string

const (
	SharedRead     LockMode = "shared_read"
	ExclusiveWrite LockMode = "exclusive_write"
	Maintenance    LockMode = "maintenance"
	EjectPending   LockMode = "eject_pending"
)

var (
	ErrLockConflict       = errors.New("storage lock conflict")
	ErrFormatPrecondition = errors.New("format precondition failed")
)

type lock struct {
	owner string
	mode  LockMode
}

type LockManager struct {
	mu    sync.Mutex
	locks map[string][]lock
}

func NewLockManager() *LockManager { return &LockManager{locks: make(map[string][]lock)} }

func (m *LockManager) Acquire(volumeID, owner string, mode LockMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.locks[volumeID]
	if len(existing) > 0 {
		if mode != SharedRead {
			return ErrLockConflict
		}
		for _, held := range existing {
			if held.mode != SharedRead {
				return ErrLockConflict
			}
		}
	}
	m.locks[volumeID] = append(existing, lock{owner: owner, mode: mode})
	return nil
}

func (m *LockManager) Release(volumeID, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.locks[volumeID]
	kept := existing[:0]
	for _, held := range existing {
		if held.owner != owner {
			kept = append(kept, held)
		}
	}
	if len(kept) == 0 {
		delete(m.locks, volumeID)
	} else {
		m.locks[volumeID] = kept
	}
}

type FormatRequest struct {
	TargetID              string
	IsSystemDisk          bool
	HasActiveLocks        bool
	ContainsPreservedData bool
	RequiredCopies        int
	VerifiedCopies        int
	ChecksumsVerified     bool
	Confirmation          *confirmation.Record
}

func ValidateFormat(request FormatRequest) error {
	if request.IsSystemDisk || request.HasActiveLocks || request.ContainsPreservedData ||
		request.VerifiedCopies < request.RequiredCopies || !request.ChecksumsVerified ||
		request.Confirmation == nil || !request.Confirmation.ValidFor(request.TargetID) {
		return ErrFormatPrecondition
	}
	return nil
}
