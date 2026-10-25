package worker

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

type fixedVerifier map[string]string

func (v fixedVerifier) Verify(workerID, _ string, presented string) bool {
	return v[workerID] == presented
}

func workerDefinition(id string) Definition {
	return Definition{
		ID: id, Enabled: true, CredentialRef: "wincred://workers/" + id,
		ProcessingKinds:   map[vocabulary.ProcessingKind]bool{vocabulary.ProcessingRender: true, vocabulary.ProcessingInspect: true},
		AllowedStorageIDs: map[string]bool{"archive": true, "source-card": true},
	}
}

func availableMetrics(storage ...string) Metrics {
	storages := make(map[string]bool, len(storage))
	for _, id := range storage {
		storages[id] = true
	}
	return Metrics{AcceptingWork: true, AccessibleStorageIDs: storages}
}

// Requirement: WRK-001
func TestWorkerConnectRequiresPreconfigurationAndIndividualServiceCredential(t *testing.T) {
	now := time.Now()
	registry := NewRegistry([]Definition{workerDefinition("worker-a")}, fixedVerifier{"worker-a": "token-a"}, 30*time.Second)
	if err := registry.Connect("unknown", "token-a", now, availableMetrics()); !errors.Is(err, ErrWorkerNotConfigured) {
		t.Fatalf("unknown worker connected: %v", err)
	}
	if err := registry.Connect("worker-a", "wrong", now, availableMetrics()); !errors.Is(err, ErrWorkerAuthentication) {
		t.Fatalf("wrong credential accepted: %v", err)
	}
	if err := registry.Connect("worker-a", "token-a", now, availableMetrics()); err != nil {
		t.Fatalf("configured worker rejected: %v", err)
	}
}

// Requirement: WRK-002
func TestWorkerWithExpiredHeartbeatCannotReceiveLease(t *testing.T) {
	now := time.Now()
	registry := NewRegistry([]Definition{workerDefinition("worker-a")}, fixedVerifier{"worker-a": "token-a"}, 30*time.Second)
	if err := registry.Connect("worker-a", "token-a", now.Add(-31*time.Second), availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"archive"}}, now); !errors.Is(err, ErrNoEligibleWorker) {
		t.Fatalf("offline worker selected: %v", err)
	}
	if err := registry.Heartbeat("worker-a", now, availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"archive"}}, now); err != nil {
		t.Fatalf("online worker not selected: %v", err)
	}
}

// Requirement: WRK-003
func TestWorkerSelectionRequiresProcessingKindAndStorageAccess(t *testing.T) {
	now := time.Now()
	a := workerDefinition("worker-a")
	b := workerDefinition("worker-b")
	delete(a.ProcessingKinds, vocabulary.ProcessingRender)
	b.AllowedStorageIDs = map[string]bool{"archive": true}
	registry := NewRegistry([]Definition{a, b}, fixedVerifier{"worker-a": "a", "worker-b": "b"}, time.Minute)
	if err := registry.Connect("worker-a", "a", now, availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Connect("worker-b", "b", now, availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	selected, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"archive"}}, now)
	if err != nil || selected.ID != "worker-b" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	b.AllowedStorageIDs = map[string]bool{}
	registry = NewRegistry([]Definition{b}, fixedVerifier{"worker-b": "b"}, time.Minute)
	if err := registry.Connect("worker-b", "b", now, availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"archive"}}, now); !errors.Is(err, ErrNoEligibleWorker) {
		t.Fatalf("heartbeat storage bypassed configured allowlist: %v", err)
	}
}

// Requirement: WRK-004
func TestSourceLocalStepIsPinnedToWorkerHoldingPhysicalVolume(t *testing.T) {
	now := time.Now()
	registry := NewRegistry([]Definition{workerDefinition("worker-a"), workerDefinition("worker-b")}, fixedVerifier{"worker-a": "a", "worker-b": "b"}, time.Minute)
	if err := registry.Connect("worker-a", "a", now, availableMetrics("source-card")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Connect("worker-b", "b", now, availableMetrics("archive")); err != nil {
		t.Fatal(err)
	}
	selected, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"source-card"}, PinnedWorkerID: "worker-a"}, now)
	if err != nil || selected.ID != "worker-a" {
		t.Fatalf("source-local work was not pinned: selected=%+v err=%v", selected, err)
	}
	if _, err := registry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"source-card"}, PinnedWorkerID: "worker-b"}, now); !errors.Is(err, ErrNoEligibleWorker) {
		t.Fatalf("worker without source volume was selected: %v", err)
	}
	offlineRegistry := NewRegistry([]Definition{workerDefinition("worker-a"), workerDefinition("worker-b")}, fixedVerifier{"worker-a": "a", "worker-b": "b"}, time.Minute)
	if err := offlineRegistry.Connect("worker-a", "a", now.Add(-2*time.Minute), availableMetrics("source-card")); err != nil {
		t.Fatal(err)
	}
	if err := offlineRegistry.Connect("worker-b", "b", now, availableMetrics("source-card")); err != nil {
		t.Fatal(err)
	}
	if _, err := offlineRegistry.Select(Requirement{ProcessingKinds: []vocabulary.ProcessingKind{vocabulary.ProcessingRender}, StorageIDs: []string{"source-card"}, PinnedWorkerID: "worker-a"}, now); !errors.Is(err, ErrNoEligibleWorker) {
		t.Fatalf("offline pinned step was rerouted: %v", err)
	}
}
