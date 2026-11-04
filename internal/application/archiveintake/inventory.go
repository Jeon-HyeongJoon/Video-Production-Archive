package archiveintake

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/worker"
)

var ErrInvalidVolumeInventory = errors.New("attached volume inventory is invalid")

type InventoryRepository interface {
	Candidate(context.Context, string) (coreintake.Candidate, error)
	ListCandidates(context.Context) ([]coreintake.Candidate, error)
	ListSessions(context.Context) ([]coreintake.Session, error)
	SaveCandidate(context.Context, coreintake.Candidate) error
}

type InventoryService struct {
	repository InventoryRepository
}

// SelectedStorageIDs returns only current, connected human selections for a
// Worker. The Worker receives stable IDs regardless of whether a co-located
// Control also has a boot-local root; no mount root crosses the protocol.
func (service *InventoryService) SelectedStorageIDs(ctx context.Context, workerID string) ([]string, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, ErrInvalidVolumeInventory
	}
	sessions, err := service.repository.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool)
	for _, session := range sessions {
		if session.WorkerID != workerID {
			continue
		}
		candidate, candidateErr := service.repository.Candidate(ctx, session.VolumeID)
		if candidateErr != nil {
			return nil, candidateErr
		}
		if candidate.Connected && candidate.WorkerID == workerID && candidate.StorageID == session.StorageID &&
			candidate.Revision == session.CandidateRevision {
			selected[session.StorageID] = true
		}
	}
	result := make([]string, 0, len(selected))
	for storageID := range selected {
		result = append(result, storageID)
	}
	sort.Strings(result)
	return result, nil
}

func NewInventoryService(repository InventoryRepository) (*InventoryService, error) {
	if repository == nil {
		return nil, ErrInvalidVolumeInventory
	}
	return &InventoryService{repository: repository}, nil
}

// Reconcile stores only stable identities and connection state. Capacity and
// labels remain in the live Worker snapshot, so changing free space does not
// invalidate a human selection revision.
func (service *InventoryService) Reconcile(ctx context.Context, workerID string, observedAt time.Time, observations []worker.AttachedVolumeObservation) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || observedAt.IsZero() {
		return ErrInvalidVolumeInventory
	}
	existing, err := service.repository.ListCandidates(ctx)
	if err != nil {
		return err
	}
	byVolume := make(map[string]coreintake.Candidate, len(existing))
	for _, candidate := range existing {
		byVolume[candidate.VolumeID] = candidate
	}
	current := make(map[string]bool, len(observations))
	for _, observation := range observations {
		if strings.TrimSpace(observation.PhysicalDeviceID) == "" || strings.TrimSpace(observation.VolumeID) == "" ||
			strings.TrimSpace(observation.StorageID) == "" || observation.CapacityBytes == 0 ||
			observation.FreeBytes > observation.CapacityBytes || current[observation.VolumeID] {
			return ErrInvalidVolumeInventory
		}
		current[observation.VolumeID] = true
		candidate, found := byVolume[observation.VolumeID]
		if found && (candidate.WorkerID != workerID || candidate.PhysicalDeviceID != observation.PhysicalDeviceID ||
			candidate.StorageID != observation.StorageID) {
			return ErrInvalidVolumeInventory
		}
		if found && candidate.Connected {
			continue
		}
		revision := uint64(1)
		if found {
			revision = candidate.Revision + 1
		}
		if err := service.repository.SaveCandidate(ctx, coreintake.Candidate{
			VolumeID: observation.VolumeID, Revision: revision, PhysicalDeviceID: observation.PhysicalDeviceID,
			WorkerID: workerID, StorageID: observation.StorageID, Connected: true, ObservedAt: observedAt.UTC(),
		}); err != nil {
			return err
		}
	}
	for _, candidate := range existing {
		// Boot-configured candidates keep a local root and are reconciled by the
		// static topology. Only Worker-observed candidates have an empty root.
		if candidate.WorkerID != workerID || candidate.Root != "" || !candidate.Connected || current[candidate.VolumeID] {
			continue
		}
		candidate.Revision++
		candidate.Connected = false
		candidate.ObservedAt = observedAt.UTC()
		if err := service.repository.SaveCandidate(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}
