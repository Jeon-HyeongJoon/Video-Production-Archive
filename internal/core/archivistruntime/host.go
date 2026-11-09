package archivistruntime

import (
	"errors"
	"sort"
	"sync"
	"time"

	"videoarchive/internal/core/vocabulary"
)

var (
	ErrArchivistHostNotConfigured  = errors.New("archivist host is not configured or is disabled")
	ErrArchivistHostAuthentication = errors.New("archivist host service credential is invalid")
	ErrArchivistHostOffline        = errors.New("archivist host is offline")
)

type HostDefinition struct {
	ID                      string
	Enabled                 bool
	CredentialRef           string
	AllowedBackends         map[vocabulary.RuntimeBackend]bool
	AllowedCLIIDs           map[string]bool
	AllowedWorkspaceRootIDs map[string]bool
}

type HostCredentialVerifier interface {
	Verify(archivistHostID, credentialRef, presentedCredential string) bool
}

type HostStatus struct {
	ArchivistHostID string
	State           vocabulary.ArchivistHostState
	LastHeartbeat   time.Time
}

type HostProfile struct {
	HostStatus
	DetectedCLIIDs []string
}

type HostRegistry struct {
	mu           sync.RWMutex
	definitions  map[string]HostDefinition
	heartbeats   map[string]time.Time
	verifier     HostCredentialVerifier
	heartbeatTTL time.Duration
}

func NewHostRegistry(definitions []HostDefinition, verifier HostCredentialVerifier, heartbeatTTL time.Duration) *HostRegistry {
	configured := make(map[string]HostDefinition, len(definitions))
	for _, definition := range definitions {
		configured[definition.ID] = cloneHostDefinition(definition)
	}
	return &HostRegistry{
		definitions: configured, heartbeats: make(map[string]time.Time), verifier: verifier, heartbeatTTL: heartbeatTTL,
	}
}

func (registry *HostRegistry) Connect(archivistHostID, presentedCredential string, now time.Time) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if err := registry.authenticate(archivistHostID, presentedCredential); err != nil {
		return err
	}
	registry.heartbeats[archivistHostID] = now
	return nil
}

func (registry *HostRegistry) Authenticate(archivistHostID, presentedCredential string) error {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.authenticate(archivistHostID, presentedCredential)
}

func (registry *HostRegistry) authenticate(archivistHostID, presentedCredential string) error {
	definition, exists := registry.definitions[archivistHostID]
	if !exists || !definition.Enabled {
		return ErrArchivistHostNotConfigured
	}
	if registry.verifier == nil || !registry.verifier.Verify(archivistHostID, definition.CredentialRef, presentedCredential) {
		return ErrArchivistHostAuthentication
	}
	return nil
}

func (registry *HostRegistry) Allows(spec SessionSpec) bool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	definition, exists := registry.definitions[spec.ArchivistHostID]
	return exists && definition.Enabled && definition.AllowedBackends[spec.Backend] &&
		definition.AllowedCLIIDs[spec.CLIID] && definition.AllowedWorkspaceRootIDs[spec.WorkspaceRootID]
}

// ConfiguredSessionSpec resolves runtime-owned backend and workspace binding.
// Browser input can choose only the registered Host and detected CLI.
func (registry *HostRegistry) ConfiguredSessionSpec(id, name, archivistHostID, cliID string) (SessionSpec, error) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	definition, exists := registry.definitions[archivistHostID]
	if !exists || !definition.Enabled || !definition.AllowedCLIIDs[cliID] {
		return SessionSpec{}, ErrArchivistHostNotConfigured
	}
	backends := make([]string, 0, len(definition.AllowedBackends))
	for backend, allowed := range definition.AllowedBackends {
		if allowed {
			backends = append(backends, string(backend))
		}
	}
	workspaces := make([]string, 0, len(definition.AllowedWorkspaceRootIDs))
	for workspace, allowed := range definition.AllowedWorkspaceRootIDs {
		if allowed {
			workspaces = append(workspaces, workspace)
		}
	}
	sort.Strings(backends)
	sort.Strings(workspaces)
	if len(backends) == 0 || len(workspaces) == 0 {
		return SessionSpec{}, ErrArchivistHostNotConfigured
	}
	return SessionSpec{
		ID: id, Name: name, ArchivistHostID: archivistHostID, Backend: vocabulary.RuntimeBackend(backends[0]),
		CLIID: cliID, WorkspaceRootID: workspaces[0],
	}, nil
}

func (registry *HostRegistry) Online(archivistHostID string, now time.Time) bool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	definition, exists := registry.definitions[archivistHostID]
	heartbeat := registry.heartbeats[archivistHostID]
	return exists && definition.Enabled && !heartbeat.IsZero() && !now.Before(heartbeat) && now.Sub(heartbeat) <= registry.heartbeatTTL
}

func (registry *HostRegistry) Statuses(now time.Time) []HostStatus {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	ids := make([]string, 0, len(registry.definitions))
	for id := range registry.definitions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	statuses := make([]HostStatus, 0, len(ids))
	for _, id := range ids {
		definition := registry.definitions[id]
		heartbeat := registry.heartbeats[id]
		state := vocabulary.ArchivistHostOffline
		if !definition.Enabled {
			state = vocabulary.ArchivistHostDisabled
		} else if !heartbeat.IsZero() && !now.Before(heartbeat) && now.Sub(heartbeat) <= registry.heartbeatTTL {
			state = vocabulary.ArchivistHostOnline
		}
		statuses = append(statuses, HostStatus{ArchivistHostID: id, State: state, LastHeartbeat: heartbeat})
	}
	return statuses
}

func (registry *HostRegistry) Profiles(now time.Time) []HostProfile {
	statuses := registry.Statuses(now)
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	profiles := make([]HostProfile, 0, len(statuses))
	for _, status := range statuses {
		definition := registry.definitions[status.ArchivistHostID]
		profile := HostProfile{HostStatus: status}
		for cliID, allowed := range definition.AllowedCLIIDs {
			if allowed {
				profile.DetectedCLIIDs = append(profile.DetectedCLIIDs, cliID)
			}
		}
		sort.Strings(profile.DetectedCLIIDs)
		profiles = append(profiles, profile)
	}
	return profiles
}

func cloneHostDefinition(source HostDefinition) HostDefinition {
	backends := make(map[vocabulary.RuntimeBackend]bool, len(source.AllowedBackends))
	for backend, allowed := range source.AllowedBackends {
		backends[backend] = allowed
	}
	cliIDs := make(map[string]bool, len(source.AllowedCLIIDs))
	for id, allowed := range source.AllowedCLIIDs {
		cliIDs[id] = allowed
	}
	workspaceIDs := make(map[string]bool, len(source.AllowedWorkspaceRootIDs))
	for id, allowed := range source.AllowedWorkspaceRootIDs {
		workspaceIDs[id] = allowed
	}
	source.AllowedBackends = backends
	source.AllowedCLIIDs = cliIDs
	source.AllowedWorkspaceRootIDs = workspaceIDs
	return source
}
