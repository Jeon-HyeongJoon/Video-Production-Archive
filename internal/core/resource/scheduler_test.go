package resource

import (
	"testing"
	"time"
)

func readySnapshot() Snapshot {
	return Snapshot{
		AverageCPU5m: 10, AverageGPU5m: 5, AvailableMemoryGB: 16, ScratchFreeGB: 100,
		IdleStableFor: 10 * time.Minute, BusyPhysicalDeviceIDs: map[string]bool{},
	}
}

func heavyRequest() Request {
	return Request{Class: Heavy, EstimatedMemoryGB: 4, ScratchRequiredGB: 20, ScratchSafetyGB: 5}
}

// Requirement: RES-001
func TestBackgroundHeavyWorkRequiresFiveStableIdleMinutesAndLowUtilization(t *testing.T) {
	scheduler := DefaultScheduler()
	snapshot := readySnapshot()
	snapshot.IdleStableFor = 4*time.Minute + 59*time.Second
	if decision := scheduler.Evaluate(snapshot, heavyRequest()); decision.Start || decision.Reason != WaitIdle {
		t.Fatalf("short idle window accepted: %+v", decision)
	}
	snapshot = readySnapshot()
	snapshot.AverageCPU5m = 35
	if decision := scheduler.Evaluate(snapshot, heavyRequest()); decision.Start || decision.Reason != WaitIdle {
		t.Fatalf("busy CPU accepted: %+v", decision)
	}
	snapshot = readySnapshot()
	snapshot.AverageGPU5m = 20
	if decision := scheduler.Evaluate(snapshot, heavyRequest()); decision.Start || decision.Reason != WaitIdle {
		t.Fatalf("busy GPU accepted: %+v", decision)
	}
}

// Requirement: RES-002
func TestBackgroundRecoveryRequiresTenStableIdleMinutes(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.IdleStableFor = 9 * time.Minute
	request := heavyRequest()
	request.RecoveringFromBusy = true
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitIdle {
		t.Fatalf("recovery started too early: %+v", decision)
	}
	snapshot.IdleStableFor = 10 * time.Minute
	if decision := DefaultScheduler().Evaluate(snapshot, request); !decision.Start {
		t.Fatalf("recovery did not start: %+v", decision)
	}
}

// Requirement: RES-003
func TestImmediateStepBypassesIdleButNotSafetyLimits(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.IdleStableFor = 0
	snapshot.AverageCPU5m = 90
	snapshot.ImmediateStepActive = true
	request := heavyRequest()
	request.Priority = Immediate
	if decision := DefaultScheduler().Evaluate(snapshot, request); !decision.Start {
		t.Fatalf("immediate step was blocked only by idle policy: %+v", decision)
	}
	snapshot.AvailableMemoryGB = 9
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitMemory {
		t.Fatalf("immediate step bypassed memory safety: %+v", decision)
	}
}

// Requirement: RES-004
func TestSchedulerPreservesSixGigabytesAfterEstimatedAllocation(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.AvailableMemoryGB = 9.9
	request := heavyRequest()
	request.EstimatedMemoryGB = 4
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitMemory {
		t.Fatalf("unsafe memory reservation accepted: %+v", decision)
	}
	snapshot.AvailableMemoryGB = 10
	if decision := DefaultScheduler().Evaluate(snapshot, request); !decision.Start {
		t.Fatalf("exact safety reserve rejected: %+v", decision)
	}
}

// Requirement: RES-005
func TestSchedulerAllowsOnlyOneHeavyTask(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.HeavyRunning = 1
	if decision := DefaultScheduler().Evaluate(snapshot, heavyRequest()); decision.Start || decision.Reason != WaitHeavySlot {
		t.Fatalf("second heavy task accepted: %+v", decision)
	}
}

// Requirement: RES-006
func TestSchedulerRejectsTaskUsingBusyPhysicalDisk(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.BusyPhysicalDeviceIDs["disk-7"] = true
	request := heavyRequest()
	request.PhysicalDeviceIDs = []string{"disk-7"}
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitDisk {
		t.Fatalf("busy disk accepted: %+v", decision)
	}
}

// Requirement: RES-007
func TestSchedulerRequiresScratchEstimatePlusSafetyMargin(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.ScratchFreeGB = 24.9
	if decision := DefaultScheduler().Evaluate(snapshot, heavyRequest()); decision.Start || decision.Reason != WaitScratch {
		t.Fatalf("insufficient scratch accepted: %+v", decision)
	}
	snapshot.ScratchFreeGB = 25
	if decision := DefaultScheduler().Evaluate(snapshot, heavyRequest()); !decision.Start {
		t.Fatalf("exact scratch requirement rejected: %+v", decision)
	}
}

// Requirement: RES-008
func TestLightStepBypassesIdleGateButKeepsDeclaredSafetyAndLightSlot(t *testing.T) {
	snapshot := readySnapshot()
	snapshot.AverageCPU5m = 95
	snapshot.AverageGPU5m = 90
	snapshot.IdleStableFor = 0
	snapshot.ImmediateStepActive = true
	request := Request{Class: Light, EstimatedMemoryGB: 1}
	if decision := DefaultScheduler().Evaluate(snapshot, request); !decision.Start {
		t.Fatalf("light step was blocked by idle policy: %+v", decision)
	}
	snapshot.AvailableMemoryGB = 6.5
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitMemory {
		t.Fatalf("light step bypassed memory safety: %+v", decision)
	}
	snapshot = readySnapshot()
	snapshot.LightRunning = 2
	if decision := DefaultScheduler().Evaluate(snapshot, request); decision.Start || decision.Reason != WaitLightSlot {
		t.Fatalf("light step bypassed its slot: %+v", decision)
	}
}
