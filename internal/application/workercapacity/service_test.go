package workercapacity_test

import (
	"context"
	"testing"
	"time"

	"videoarchive/internal/application/workercapacity"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

func TestCapacitySubtractsActiveReservationsFromWorkerSnapshot(t *testing.T) {
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	registry := fixedRegistry{statuses: []worker.Status{{
		WorkerID: "WORKER-1", State: "ONLINE", LastHeartbeat: now,
		Metrics: worker.Metrics{
			SnapshotID: 7, ObservedAt: now, AverageCPU5m: 10, AverageGPU5m: 5, IdleStableFor: 10 * time.Minute,
			AvailableMemoryBytes: 18 << 30, ScratchFreeBytes: 100 << 30, AcceptingWork: true,
			BusyPhysicalDeviceIDs: map[string]bool{}, AccessibleStorageIDs: map[string]bool{"archive-main": true},
		},
	}}}
	reservations := fixedReservations{values: []dispatch.ResourceReservation{{
		ID: "RES-1", JobID: "JOB-1", StepID: "JOB-1/render", WorkerID: "WORKER-1", SnapshotID: 7,
		ResourceClass: vocabulary.ResourceHeavy, MemoryBytes: 4 << 30, ScratchBytes: 25 << 30, CreatedAt: now,
	}}}
	service, err := workercapacity.New(registry, reservations, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := service.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
	capacity := statuses[0].Capacity
	if capacity.ReservedMemoryBytes != 4<<30 || capacity.AvailableMemoryBytes != 14<<30 ||
		capacity.ReservedScratchBytes != 25<<30 || capacity.ScratchFreeBytes != 75<<30 ||
		capacity.HeavySlotsUsed != 1 || capacity.HeavySlotsMaximum != 1 ||
		capacity.HeavyAdmission.WaitReason != vocabulary.WaitSlot || !capacity.LightAdmission.Available ||
		!capacity.AdditionalWorkPossible {
		t.Fatalf("reservation-adjusted capacity is wrong: %+v", capacity)
	}
}

type fixedRegistry struct {
	statuses []worker.Status
}

func (registry fixedRegistry) Statuses(time.Time) []worker.Status {
	return append([]worker.Status(nil), registry.statuses...)
}

type fixedReservations struct {
	values []dispatch.ResourceReservation
}

func (reservations fixedReservations) ListActiveReservations() ([]dispatch.ResourceReservation, error) {
	return append([]dispatch.ResourceReservation(nil), reservations.values...), nil
}
