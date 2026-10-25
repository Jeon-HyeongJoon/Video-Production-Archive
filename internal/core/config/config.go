package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"videoarchive/internal/core/security"
)

var (
	ErrRestartRequired        = errors.New("management configuration is applied only at process startup")
	ErrInvalidSecretReference = errors.New("credential must be an OS secret store reference")
	ErrDuplicateDefinition    = errors.New("configuration contains a duplicate definition")
	ErrUnsafeListenAddress    = errors.New("service listen address is unsafe")
)

type ProviderDefinition struct {
	ID        string
	SecretRef string
}

type WorkerDefinition struct {
	ID            string
	CredentialRef string
}

type Settings struct {
	AuthMode            security.AuthMode
	ListenAddress       string
	MCPListenAddress    string
	AllowDeviceFormat   bool
	AllowOriginalDelete bool
	AllowArchiveReplace bool
	Providers           []ProviderDefinition
	Workers             []WorkerDefinition
}

func (s Settings) RemoteManagementEnabled() bool {
	return s.AuthMode == security.AuthLocal
}

func (s Settings) ManagementRoutes() []string {
	if !s.RemoteManagementEnabled() {
		return nil
	}
	return []string{"/api/v1/admin/providers", "/api/v1/admin/workers", "/api/v1/admin/security"}
}

type DestructiveFeature string

const (
	DeviceFormat   DestructiveFeature = "device_format"
	OriginalDelete DestructiveFeature = "original_delete"
	ArchiveReplace DestructiveFeature = "archive_replace"
)

func (s Settings) AllowsDestructive(feature DestructiveFeature) bool {
	switch feature {
	case DeviceFormat:
		return s.AllowDeviceFormat
	case OriginalDelete:
		return s.AllowOriginalDelete
	case ArchiveReplace:
		return s.AllowArchiveReplace
	default:
		return false
	}
}

type ChangeSource string

const (
	ChangeDashboard ChangeSource = "dashboard"
	ChangeAPI       ChangeSource = "api"
	ChangeLocalFile ChangeSource = "local_file"
)

func ValidateBootApplication(source ChangeSource, processStarting bool) error {
	if source != ChangeLocalFile || !processStarting {
		return ErrRestartRequired
	}
	return nil
}

func (s Settings) Validate() error {
	listen, err := netip.ParseAddrPort(s.ListenAddress)
	if err != nil || listen.Addr().IsUnspecified() {
		return ErrUnsafeListenAddress
	}
	mcpListen, err := netip.ParseAddrPort(s.MCPListenAddress)
	if err != nil || mcpListen.Addr().IsUnspecified() || !security.MCPListenAllowed(s.AuthMode, mcpListen.Addr()) {
		return ErrUnsafeListenAddress
	}
	providerIDs := make(map[string]bool, len(s.Providers))
	for _, provider := range s.Providers {
		if provider.ID == "" || !validSecretRef(provider.SecretRef) {
			return fmt.Errorf("provider %q: %w", provider.ID, ErrInvalidSecretReference)
		}
		if providerIDs[provider.ID] {
			return fmt.Errorf("provider %q: %w", provider.ID, ErrDuplicateDefinition)
		}
		providerIDs[provider.ID] = true
	}
	workerIDs := make(map[string]bool, len(s.Workers))
	for _, worker := range s.Workers {
		if worker.ID == "" || !validSecretRef(worker.CredentialRef) {
			return fmt.Errorf("worker %q: %w", worker.ID, ErrInvalidSecretReference)
		}
		if workerIDs[worker.ID] {
			return fmt.Errorf("worker %q: %w", worker.ID, ErrDuplicateDefinition)
		}
		workerIDs[worker.ID] = true
	}
	return nil
}

func validSecretRef(reference string) bool {
	return strings.HasPrefix(reference, "wincred://") || strings.HasPrefix(reference, "dpapi://")
}
