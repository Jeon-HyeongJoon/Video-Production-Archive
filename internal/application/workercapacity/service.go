package workercapacity

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/worker"
)

var ErrInvalidConfiguration = errors.New("Worker capacity configuration is invalid")

type Registry interface {
	Statuses(time.Time) []worker.Status
}

type ReservationReader interface {
	ListActiveReservations() ([]dispatch.ResourceReservation, error)
}

type Usage struct {
	MemoryBytes  uint64
	ScratchBytes uint64
	HeavySlots   int
	LightSlots   int
}

type Service struct {
	registry     Registry
	reservations ReservationReader
	now          func() time.Time
	scheduler    resource.Scheduler
}

func New(registry Registry, reservations ReservationReader, now func() time.Time) (*Service, error) {
	if registry == nil || reservations == nil || now == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Service{
		registry: registry, reservations: reservations, now: now, scheduler: resource.DefaultScheduler(),
	}, nil
}

func (service *Service) Statuses(context.Context) ([]worker.Status, error) {
	reservations, err := service.reservations.ListActiveReservations()
	if err != nil {
		return nil, err
	}
	statuses := service.registry.Statuses(service.now().UTC())
	for index := range statuses {
		status := &statuses[index]
		adjusted, usage := Account(status.Metrics, reservations, status.WorkerID)
		status.Capacity = worker.Capacity{
			ReservedMemoryBytes: usage.MemoryBytes, ReservedScratchBytes: usage.ScratchBytes,
			AvailableMemoryBytes: adjusted.AvailableMemoryBytes, ScratchFreeBytes: adjusted.ScratchFreeBytes,
			HeavySlotsUsed: adjusted.HeavyRunning, HeavySlotsMaximum: service.scheduler.MaxHeavy,
			LightSlotsUsed: adjusted.LightRunning, LightSlotsMaximum: service.scheduler.MaxLight,
		}
		status.Capacity.HeavyAdmission = service.admission(*status, adjusted, resource.DefaultRequest(resource.Heavy))
		status.Capacity.LightAdmission = service.admission(*status, adjusted, resource.DefaultRequest(resource.Light))
		status.Capacity.AdditionalWorkPossible = status.Capacity.HeavyAdmission.Available || status.Capacity.LightAdmission.Available
	}
	return statuses, nil
}

func (service *Service) admission(status worker.Status, adjusted worker.Metrics, request resource.Request) worker.Admission {
	if status.State == "DISABLED" || status.State == "OFFLINE" {
		return worker.Admission{Code: status.State}
	}
	if !status.Metrics.AcceptingWork {
		return worker.Admission{Code: "NOT_ACCEPTING_WORK"}
	}
	decision := service.scheduler.Evaluate(resource.Snapshot{
		AverageCPU5m: adjusted.AverageCPU5m, AverageGPU5m: adjusted.AverageGPU5m,
		AvailableMemoryGB: float64(adjusted.AvailableMemoryBytes) / float64(1<<30),
		ScratchFreeGB:     float64(adjusted.ScratchFreeBytes) / float64(1<<30),
		IdleStableFor:     adjusted.IdleStableFor, HeavyRunning: adjusted.HeavyRunning,
		LightRunning: adjusted.LightRunning, BusyPhysicalDeviceIDs: adjusted.BusyPhysicalDeviceIDs,
	}, request)
	if decision.Start {
		return worker.Admission{Available: true}
	}
	return worker.Admission{WaitReason: decision.Reason, Code: "RESOURCE"}
}

func Account(metrics worker.Metrics, reservations []dispatch.ResourceReservation, workerID string) (worker.Metrics, Usage) {
	adjusted := metrics
	adjusted.BusyPhysicalDeviceIDs = make(map[string]bool, len(metrics.BusyPhysicalDeviceIDs))
	for deviceID, busy := range metrics.BusyPhysicalDeviceIDs {
		adjusted.BusyPhysicalDeviceIDs[deviceID] = busy
	}
	usage := Usage{}
	for _, reservation := range reservations {
		if reservation.WorkerID != workerID {
			continue
		}
		usage.MemoryBytes = saturatingAdd(usage.MemoryBytes, reservation.MemoryBytes)
		usage.ScratchBytes = saturatingAdd(usage.ScratchBytes, reservation.ScratchBytes)
		switch reservation.ResourceClass {
		case resource.Heavy:
			usage.HeavySlots++
		case resource.Light:
			usage.LightSlots++
		}
		for _, deviceID := range reservation.PhysicalDeviceIDs {
			adjusted.BusyPhysicalDeviceIDs[deviceID] = true
		}
	}
	adjusted.AvailableMemoryBytes = saturatingSubtract(metrics.AvailableMemoryBytes, usage.MemoryBytes)
	adjusted.ScratchFreeBytes = saturatingSubtract(metrics.ScratchFreeBytes, usage.ScratchBytes)
	adjusted.HeavyRunning += usage.HeavySlots
	adjusted.LightRunning += usage.LightSlots
	return adjusted, usage
}

func saturatingSubtract(value, used uint64) uint64 {
	if used >= value {
		return 0
	}
	return value - used
}

func saturatingAdd(left, right uint64) uint64 {
	result := left + right
	if result < left {
		return ^uint64(0)
	}
	return result
}
