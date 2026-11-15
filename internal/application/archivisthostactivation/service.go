package archivisthostactivation

import (
	"context"
	"errors"
	"strings"

	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrHostNotFound       = errors.New("archivist host was not found")
	ErrHostDisabled       = errors.New("archivist host is disabled")
	ErrHostStartUnmanaged = errors.New("archivist host start is not configured")
)

type HostProfiles interface {
	HostProfiles() []archivistruntime.HostProfile
}

type Starter interface {
	Start(context.Context, string) error
}

type Result struct {
	ArchivistHostID string
	State           vocabulary.ArchivistHostState
}

type Service struct {
	hosts   HostProfiles
	starter Starter
}

func New(hosts HostProfiles, starter Starter) (*Service, error) {
	if hosts == nil || starter == nil {
		return nil, errors.New("archivist host activation dependencies are incomplete")
	}
	return &Service{hosts: hosts, starter: starter}, nil
}

func (service *Service) Start(ctx context.Context, archivistHostID string) (Result, error) {
	archivistHostID = strings.TrimSpace(archivistHostID)
	for _, profile := range service.hosts.HostProfiles() {
		if profile.ArchivistHostID != archivistHostID {
			continue
		}
		if profile.State == vocabulary.ArchivistHostDisabled {
			return Result{}, ErrHostDisabled
		}
		if profile.State == vocabulary.ArchivistHostOnline || profile.State == vocabulary.ArchivistHostBusy {
			return Result{ArchivistHostID: archivistHostID, State: profile.State}, nil
		}
		if err := service.starter.Start(ctx, archivistHostID); err != nil {
			return Result{}, err
		}
		return Result{ArchivistHostID: archivistHostID, State: vocabulary.ArchivistHostConnecting}, nil
	}
	return Result{}, ErrHostNotFound
}
