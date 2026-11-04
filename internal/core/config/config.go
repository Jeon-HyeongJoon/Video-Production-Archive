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

type ArchivistHostDefinition struct {
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
	ArchivistHosts      []ArchivistHostDefinition
}

func (Settings) ManagementRoutes() []string {
	return nil
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
	ChangeArchiveWorkspace ChangeSource = "archive_workspace"
	ChangeAPI              ChangeSource = "api"
	ChangeLocalFile        ChangeSource = "local_file"
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
		if provider.ID == "" || !ValidSecretReference(provider.SecretRef) {
			return fmt.Errorf("provider %q: %w", provider.ID, ErrInvalidSecretReference)
		}
		if providerIDs[provider.ID] {
			return fmt.Errorf("provider %q: %w", provider.ID, ErrDuplicateDefinition)
		}
		providerIDs[provider.ID] = true
	}
	workerIDs := make(map[string]bool, len(s.Workers))
	for _, worker := range s.Workers {
		if worker.ID == "" || !ValidSecretReference(worker.CredentialRef) {
			return fmt.Errorf("worker %q: %w", worker.ID, ErrInvalidSecretReference)
		}
		if workerIDs[worker.ID] {
			return fmt.Errorf("worker %q: %w", worker.ID, ErrDuplicateDefinition)
		}
		workerIDs[worker.ID] = true
	}
	archivistHostIDs := make(map[string]bool, len(s.ArchivistHosts))
	for _, host := range s.ArchivistHosts {
		if host.ID == "" || !ValidSecretReference(host.CredentialRef) {
			return fmt.Errorf("archivist host %q: %w", host.ID, ErrInvalidSecretReference)
		}
		if archivistHostIDs[host.ID] {
			return fmt.Errorf("archivist host %q: %w", host.ID, ErrDuplicateDefinition)
		}
		archivistHostIDs[host.ID] = true
	}
	return nil
}

func ValidSecretReference(reference string) bool {
	reference = strings.TrimSpace(reference)
	if strings.HasPrefix(reference, "wincred://") {
		target := strings.TrimPrefix(reference, "wincred://")
		return target != "" && !strings.ContainsAny(target, "\x00\r\n")
	}
	if !strings.HasPrefix(reference, "dpapi://") {
		return false
	}
	path := strings.TrimPrefix(reference, "dpapi://")
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '/' || path[2] == '\\') && !strings.ContainsAny(path[3:], "\x00\r\n:")
}
