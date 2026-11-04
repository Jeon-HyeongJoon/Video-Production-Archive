package archiveintake

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrInvalidCandidate       = errors.New("archive intake candidate is incomplete or invalid")
	ErrStaleCandidate         = errors.New("archive intake candidate revision is stale")
	ErrHumanSelectionRequired = errors.New("archive intake session requires explicit human selection")
	ErrInvalidSession         = errors.New("archive intake session is incomplete or invalid")
)

type Candidate struct {
	VolumeID         string    `json:"volume_id"`
	Revision         uint64    `json:"revision"`
	PhysicalDeviceID string    `json:"physical_device_id"`
	WorkerID         string    `json:"worker_id"`
	StorageID        string    `json:"storage_id"`
	Root             string    `json:"root"`
	Connected        bool      `json:"connected"`
	ObservedAt       time.Time `json:"observed_at"`
}

func (candidate Candidate) Validate() error {
	if strings.TrimSpace(candidate.VolumeID) == "" || candidate.Revision == 0 || strings.TrimSpace(candidate.PhysicalDeviceID) == "" ||
		strings.TrimSpace(candidate.WorkerID) == "" || strings.TrimSpace(candidate.StorageID) == "" ||
		(strings.TrimSpace(candidate.Root) != "" && !filepath.IsAbs(candidate.Root)) || candidate.ObservedAt.IsZero() {
		return ErrInvalidCandidate
	}
	return nil
}

type Session struct {
	ID                string    `json:"id"`
	Revision          uint64    `json:"revision"`
	CandidateRevision uint64    `json:"candidate_revision"`
	VolumeID          string    `json:"volume_id"`
	WorkspaceDriveID  string    `json:"workspace_drive_id"`
	PhysicalDeviceID  string    `json:"physical_device_id"`
	WorkerID          string    `json:"worker_id"`
	StorageID         string    `json:"storage_id"`
	SelectedBy        string    `json:"selected_by"`
	SelectedAt        time.Time `json:"selected_at"`
}

func NewSession(id string, candidate Candidate, expectedRevision uint64, selectedBy string, selectedAt time.Time) (Session, error) {
	if candidate.Validate() != nil || !candidate.Connected {
		return Session{}, ErrInvalidCandidate
	}
	if candidate.Revision != expectedRevision {
		return Session{}, ErrStaleCandidate
	}
	id = strings.TrimSpace(id)
	selectedBy = strings.TrimSpace(selectedBy)
	if id == "" || selectedBy == "" || selectedAt.IsZero() {
		return Session{}, ErrInvalidSession
	}
	return Session{
		ID: id, Revision: 1, CandidateRevision: candidate.Revision, VolumeID: candidate.VolumeID,
		WorkspaceDriveID: candidate.VolumeID, PhysicalDeviceID: candidate.PhysicalDeviceID,
		WorkerID: candidate.WorkerID, StorageID: candidate.StorageID,
		SelectedBy: selectedBy, SelectedAt: selectedAt.UTC(),
	}, nil
}
