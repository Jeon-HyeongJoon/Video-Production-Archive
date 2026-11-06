package archiveintake

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/adapter/sqlitearchiveintake"
	"videoarchive/internal/core/worker"
)

func TestInventoryReconcilesWorkerObservedCandidatesWithoutChangingRevisionForFreeSpace(t *testing.T) {
	repository, err := sqlitearchiveintake.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service, err := NewInventoryService(repository)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	attached := []worker.AttachedVolumeObservation{{
		PhysicalDeviceID: "physical-usb", VolumeID: "volume-usb", StorageID: "source-usb",
		Model: "USB BRIDGE", FileSystem: "NTFS", CapacityBytes: 2_000, FreeBytes: 500,
	}}
	if err := service.Reconcile(context.Background(), "worker-windows", now, attached); err != nil {
		t.Fatal(err)
	}
	attached[0].FreeBytes = 400
	if err := service.Reconcile(context.Background(), "worker-windows", now.Add(time.Minute), attached); err != nil {
		t.Fatal(err)
	}
	candidate, err := repository.Candidate(context.Background(), "volume-usb")
	if err != nil || candidate.Revision != 1 || !candidate.Connected || candidate.Root != "" {
		t.Fatalf("stable attachment changed selection revision: candidate=%+v err=%v", candidate, err)
	}
	if err := service.Reconcile(context.Background(), "worker-windows", now.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	candidate, err = repository.Candidate(context.Background(), "volume-usb")
	if err != nil || candidate.Revision != 2 || candidate.Connected {
		t.Fatalf("detachment was not reconciled: candidate=%+v err=%v", candidate, err)
	}
	if err := service.Reconcile(context.Background(), "worker-secondary", now.Add(3*time.Minute), attached); err != nil {
		t.Fatal(err)
	}
	candidate, err = repository.Candidate(context.Background(), "volume-usb")
	if err != nil || candidate.Revision != 3 || !candidate.Connected || candidate.WorkerID != "worker-secondary" {
		t.Fatalf("same physical volume was not reassigned to its current Worker: candidate=%+v err=%v", candidate, err)
	}
}
