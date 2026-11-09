package archiveintake

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/archiveintake"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

type Repository interface {
	Candidate(context.Context, string) (coreintake.Candidate, error)
	SaveSession(context.Context, coreintake.Session) error
}

type SelectRequest struct {
	SessionID                 string
	VolumeID                  string
	ExpectedCandidateRevision uint64
	Actor                     command.Actor
	SelectedAt                time.Time
}

type SessionAccess struct {
	Session coreintake.Session
	Root    *archive.ReadOnlyStorageRoot
}

func (access *SessionAccess) Close() error {
	if access == nil || access.Root == nil {
		return archive.ErrUnsafeStoragePath
	}
	return access.Root.Close()
}

func (access SessionAccess) ReservationPhysicalDeviceIDs() []string {
	if access.Session.PhysicalDeviceID == "" {
		return nil
	}
	return []string{access.Session.PhysicalDeviceID}
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, coreintake.ErrInvalidSession
	}
	return &Service{repository: repository}, nil
}

func (service *Service) Select(ctx context.Context, request SelectRequest) (*SessionAccess, error) {
	if (request.Actor.Type != vocabulary.ActorHuman && request.Actor.Type != vocabulary.ActorArchivist) || request.Actor.ID == "" {
		return nil, coreintake.ErrSelectionActorRequired
	}
	candidate, err := service.repository.Candidate(ctx, request.VolumeID)
	if err != nil {
		return nil, err
	}
	session, err := coreintake.NewSession(request.SessionID, candidate, request.ExpectedCandidateRevision, request.Actor, request.SelectedAt)
	if err != nil {
		return nil, err
	}
	var root *archive.ReadOnlyStorageRoot
	if candidate.Root != "" {
		root, err = archive.OpenReadOnlyStorageRoot(candidate.Root)
		if err != nil {
			return nil, err
		}
	}
	if err := service.repository.SaveSession(ctx, session); err != nil {
		if root != nil {
			err = errors.Join(err, root.Close())
		}
		return nil, err
	}
	return &SessionAccess{Session: session, Root: root}, nil
}
