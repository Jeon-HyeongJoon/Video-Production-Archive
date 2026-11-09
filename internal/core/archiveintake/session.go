package archiveintake

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidCandidate       = errors.New("archive intake candidate is incomplete or invalid")
	ErrStaleCandidate         = errors.New("archive intake candidate revision is stale")
	ErrSelectionActorRequired = errors.New("archive intake session requires an authorized explicit selection")
	ErrHumanSelectionRequired = ErrSelectionActorRequired
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
	ID                string               `json:"id"`
	Revision          uint64               `json:"revision"`
	CandidateRevision uint64               `json:"candidate_revision"`
	VolumeID          string               `json:"volume_id"`
	WorkspaceDriveID  string               `json:"workspace_drive_id"`
	PhysicalDeviceID  string               `json:"physical_device_id"`
	WorkerID          string               `json:"worker_id"`
	StorageID         string               `json:"storage_id"`
	SelectedByType    vocabulary.ActorType `json:"selected_by_type"`
	SelectedBy        string               `json:"selected_by"`
	SelectedAt        time.Time            `json:"selected_at"`
}

func NewSession(id string, candidate Candidate, expectedRevision uint64, selectedBy command.Actor, selectedAt time.Time) (Session, error) {
	if candidate.Validate() != nil || !candidate.Connected {
		return Session{}, ErrInvalidCandidate
	}
	if candidate.Revision != expectedRevision {
		return Session{}, ErrStaleCandidate
	}
	id = strings.TrimSpace(id)
	selectedBy.ID = strings.TrimSpace(selectedBy.ID)
	if id == "" || (selectedBy.Type != vocabulary.ActorHuman && selectedBy.Type != vocabulary.ActorArchivist) ||
		selectedBy.ID == "" || selectedAt.IsZero() {
		return Session{}, ErrInvalidSession
	}
	return Session{
		ID: id, Revision: 1, CandidateRevision: candidate.Revision, VolumeID: candidate.VolumeID,
		WorkspaceDriveID: candidate.VolumeID, PhysicalDeviceID: candidate.PhysicalDeviceID,
		WorkerID: candidate.WorkerID, StorageID: candidate.StorageID,
		SelectedByType: selectedBy.Type, SelectedBy: selectedBy.ID, SelectedAt: selectedAt.UTC(),
	}, nil
}
