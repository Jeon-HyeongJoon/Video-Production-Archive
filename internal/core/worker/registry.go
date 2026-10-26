package worker

import (
	"errors"
	"sort"
	"sync"
	"time"

	"videoarchive/internal/core/vocabulary"
)

var (
	ErrWorkerNotConfigured  = errors.New("worker is not configured or is disabled")
	ErrWorkerAuthentication = errors.New("worker service credential is invalid")
	ErrWorkerNotConnected   = errors.New("worker is not connected")
	ErrNoEligibleWorker     = errors.New("no eligible worker is online")
)

type Definition struct {
	ID                string
	Enabled           bool
	CredentialRef     string
	ProcessingKinds   map[vocabulary.ProcessingKind]bool
	AllowedStorageIDs map[string]bool
}

type CredentialVerifier interface {
	Verify(workerID, credentialRef, presentedCredential string) bool
}

type Metrics struct {
	AcceptingWork        bool
	HeavyRunning         int
	AccessibleStorageIDs map[string]bool
}

type runtimeState struct {
	lastHeartbeat time.Time
	metrics       Metrics
}

type Status struct {
	WorkerID      string
	State         string
	LastHeartbeat time.Time
	Metrics       Metrics
}

type Registry struct {
	mu           sync.RWMutex
	definitions  map[string]Definition
	states       map[string]runtimeState
	verifier     CredentialVerifier
	heartbeatTTL time.Duration
}

func NewRegistry(definitions []Definition, verifier CredentialVerifier, heartbeatTTL time.Duration) *Registry {
	configured := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		configured[definition.ID] = definition
	}
	return &Registry{definitions: configured, states: make(map[string]runtimeState), verifier: verifier, heartbeatTTL: heartbeatTTL}
}

func (r *Registry) Connect(workerID, presentedCredential string, now time.Time, metrics Metrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	definition, exists := r.definitions[workerID]
	if !exists || !definition.Enabled {
		return ErrWorkerNotConfigured
	}
	if r.verifier == nil || !r.verifier.Verify(workerID, definition.CredentialRef, presentedCredential) {
		return ErrWorkerAuthentication
	}
	r.states[workerID] = runtimeState{lastHeartbeat: now, metrics: metrics}
	return nil
}

func (r *Registry) Heartbeat(workerID string, now time.Time, metrics Metrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, connected := r.states[workerID]; !connected {
		return ErrWorkerNotConnected
	}
	r.states[workerID] = runtimeState{lastHeartbeat: now, metrics: metrics}
	return nil
}

func (r *Registry) Statuses(now time.Time) []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	workerIDs := make([]string, 0, len(r.definitions))
	for workerID := range r.definitions {
		workerIDs = append(workerIDs, workerID)
	}
	sort.Strings(workerIDs)
	statuses := make([]Status, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		definition := r.definitions[workerID]
		status := Status{WorkerID: workerID, State: "OFFLINE"}
		if !definition.Enabled {
			status.State = "DISABLED"
		}
		if state, connected := r.states[workerID]; definition.Enabled && connected {
			status.LastHeartbeat = state.lastHeartbeat
			status.Metrics = cloneMetrics(state.metrics)
			if now.Sub(state.lastHeartbeat) <= r.heartbeatTTL {
				status.State = "ONLINE"
				if state.metrics.HeavyRunning > 0 {
					status.State = "BUSY"
				} else if !state.metrics.AcceptingWork {
					status.State = "DRAINING"
				}
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

type Requirement struct {
	ProcessingKinds []vocabulary.ProcessingKind
	StorageIDs      []string
	PinnedWorkerID  string
}

func (r *Registry) Select(requirement Requirement, now time.Time) (Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	type candidate struct {
		definition  Definition
		activeHeavy int
	}
	var candidates []candidate
	for id, definition := range r.definitions {
		state, connected := r.states[id]
		if !definition.Enabled || !connected || now.Sub(state.lastHeartbeat) > r.heartbeatTTL || !state.metrics.AcceptingWork {
			continue
		}
		if requirement.PinnedWorkerID != "" && id != requirement.PinnedWorkerID {
			continue
		}
		if !supports(definition.ProcessingKinds, requirement.ProcessingKinds) ||
			!supports(definition.AllowedStorageIDs, requirement.StorageIDs) ||
			!supports(state.metrics.AccessibleStorageIDs, requirement.StorageIDs) {
			continue
		}
		candidates = append(candidates, candidate{definition: definition, activeHeavy: state.metrics.HeavyRunning})
	}
	if len(candidates) == 0 {
		return Definition{}, ErrNoEligibleWorker
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activeHeavy == candidates[j].activeHeavy {
			return candidates[i].definition.ID < candidates[j].definition.ID
		}
		return candidates[i].activeHeavy < candidates[j].activeHeavy
	})
	return candidates[0].definition, nil
}

func supports[T ~string](available map[T]bool, required []T) bool {
	for _, item := range required {
		if !available[item] {
			return false
		}
	}
	return true
}

func cloneMetrics(source Metrics) Metrics {
	accessible := make(map[string]bool, len(source.AccessibleStorageIDs))
	for storageID, available := range source.AccessibleStorageIDs {
		accessible[storageID] = available
	}
	return Metrics{
		AcceptingWork: source.AcceptingWork, HeavyRunning: source.HeavyRunning,
		AccessibleStorageIDs: accessible,
	}
}
