package device

import (
	"errors"
	"sync"

	"videoarchive/internal/core/confirmation"
)

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
