package resource

import (
	"time"

	"videoarchive/internal/core/vocabulary"
)

type Class = vocabulary.ResourceClass

const (
	Heavy = vocabulary.ResourceHeavy
	Light = vocabulary.ResourceLight
)

type WaitReason = vocabulary.WaitReason

const (
	WaitIdle      = vocabulary.WaitIdle
	WaitMemory    = vocabulary.WaitMemory
	WaitScratch   = vocabulary.WaitScratch
	WaitHeavySlot = vocabulary.WaitSlot
	WaitLightSlot = vocabulary.WaitSlot
	WaitDisk      = vocabulary.WaitDisk
	WaitPriority  = vocabulary.WaitPriority
)

type ExecutionPriority string

const (
	Background ExecutionPriority = "BACKGROUND"
	Immediate  ExecutionPriority = "IMMEDIATE"
)

type Request struct {
	Class              Class
	Priority           ExecutionPriority
	RecoveringFromBusy bool
	EstimatedMemoryGB  float64
	ScratchRequiredGB  float64
	ScratchSafetyGB    float64
	PhysicalDeviceIDs  []string
}

type Snapshot struct {
	AverageCPU5m          float64
	AverageGPU5m          float64
	AvailableMemoryGB     float64
	ScratchFreeGB         float64
	IdleStableFor         time.Duration
	ImmediateStepActive   bool
	HeavyRunning          int
	LightRunning          int
	BusyPhysicalDeviceIDs map[string]bool
}

type Decision struct {
	Start  bool
	Reason WaitReason
}

type Scheduler struct {
	MemorySafetyGB float64
	InitialIdle    time.Duration
	RecoveryIdle   time.Duration
	MaxHeavy       int
	MaxLight       int
}

func DefaultScheduler() Scheduler {
	return Scheduler{
		MemorySafetyGB: 6,
		InitialIdle:    5 * time.Minute,
		RecoveryIdle:   10 * time.Minute,
		MaxHeavy:       1,
		MaxLight:       2,
	}
}

func (s Scheduler) Evaluate(snapshot Snapshot, request Request) Decision {
	if snapshot.AvailableMemoryGB-request.EstimatedMemoryGB < s.MemorySafetyGB {
		return Decision{Reason: WaitMemory}
	}
	if snapshot.ScratchFreeGB < request.ScratchRequiredGB+request.ScratchSafetyGB {
		return Decision{Reason: WaitScratch}
	}
	for _, deviceID := range request.PhysicalDeviceIDs {
		if snapshot.BusyPhysicalDeviceIDs[deviceID] {
			return Decision{Reason: WaitDisk}
		}
	}
	if request.Class == Heavy && snapshot.HeavyRunning >= s.MaxHeavy {
		return Decision{Reason: WaitHeavySlot}
	}
	if request.Class == Light && snapshot.LightRunning >= s.MaxLight {
		return Decision{Reason: WaitLightSlot}
	}
	if request.Class == Heavy && snapshot.ImmediateStepActive && request.Priority != Immediate {
		return Decision{Reason: WaitPriority}
	}
	if request.Class == Heavy && request.Priority != Immediate {
		requiredIdle := s.InitialIdle
		if request.RecoveringFromBusy {
			requiredIdle = s.RecoveryIdle
		}
		if snapshot.AverageCPU5m >= 35 || snapshot.AverageGPU5m >= 20 || snapshot.IdleStableFor < requiredIdle {
			return Decision{Reason: WaitIdle}
		}
	}
	return Decision{Start: true}
}
