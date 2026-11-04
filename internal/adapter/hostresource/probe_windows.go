//go:build windows

package hostresource

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"videoarchive/internal/core/worker"
)

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	getSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

type memoryStatusEx struct {
	Length                   uint32
	MemoryLoad               uint32
	TotalPhysical            uint64
	AvailablePhysical        uint64
	TotalPageFile            uint64
	AvailablePageFile        uint64
	TotalVirtual             uint64
	AvailableVirtual         uint64
	AvailableExtendedVirtual uint64
}

type sample struct {
	at         time.Time
	cpuPercent float64
	gpuPercent float64
}

type WindowsProbe struct {
	mu             sync.Mutex
	scratchPath    string
	now            func() time.Time
	previousIdle   uint64
	previousKernel uint64
	previousUser   uint64
	hasPrevious    bool
	samples        []sample
	idleSince      time.Time
}

func New(scratchPath string, now func() time.Time) Probe {
	return &WindowsProbe{scratchPath: scratchPath, now: now}
}

func (probe *WindowsProbe) Snapshot(ctx context.Context) (worker.Metrics, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	now := probe.now().UTC()
	metrics := readyMetrics(now)

	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	result, _, callErr := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if result == 0 {
		return worker.Metrics{}, fmt.Errorf("%w: memory: %v", ErrObservationFailed, callErr)
	}
	metrics.AvailableMemoryBytes = status.AvailablePhysical

	directory, err := windows.UTF16PtrFromString(probe.scratchPath)
	if err != nil {
		return worker.Metrics{}, fmt.Errorf("%w: scratch path", ErrObservationFailed)
	}
	if err := windows.GetDiskFreeSpaceEx(directory, &metrics.ScratchFreeBytes, nil, nil); err != nil {
		return worker.Metrics{}, fmt.Errorf("%w: scratch: %v", ErrObservationFailed, err)
	}

	metrics.AverageCPU5m = probe.observeCPU()
	metrics.AverageGPU5m, metrics.AvailableVRAMBytes = observeNVIDIAGPU(ctx)
	probe.samples = append(probe.samples, sample{at: now, cpuPercent: metrics.AverageCPU5m, gpuPercent: metrics.AverageGPU5m})
	cutoff := now.Add(-5 * time.Minute)
	first := 0
	for first < len(probe.samples) && probe.samples[first].at.Before(cutoff) {
		first++
	}
	probe.samples = append([]sample(nil), probe.samples[first:]...)
	var cpuTotal, gpuTotal float64
	for _, value := range probe.samples {
		cpuTotal += value.cpuPercent
		gpuTotal += value.gpuPercent
	}
	metrics.AverageCPU5m = cpuTotal / float64(len(probe.samples))
	metrics.AverageGPU5m = gpuTotal / float64(len(probe.samples))
	if metrics.AverageCPU5m < 35 && metrics.AverageGPU5m < 20 {
		if probe.idleSince.IsZero() {
			probe.idleSince = now
		}
		metrics.IdleStableFor = now.Sub(probe.idleSince)
	} else {
		probe.idleSince = time.Time{}
	}
	return metrics, nil
}

func (probe *WindowsProbe) observeCPU() float64 {
	var idle, kernel, user windows.Filetime
	result, _, _ := getSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)),
	)
	if result == 0 {
		return 100
	}
	idleValue := filetimeValue(idle)
	kernelValue := filetimeValue(kernel)
	userValue := filetimeValue(user)
	if !probe.hasPrevious {
		probe.previousIdle, probe.previousKernel, probe.previousUser = idleValue, kernelValue, userValue
		probe.hasPrevious = true
		return 0
	}
	idleDelta := idleValue - probe.previousIdle
	totalDelta := kernelValue - probe.previousKernel + userValue - probe.previousUser
	probe.previousIdle, probe.previousKernel, probe.previousUser = idleValue, kernelValue, userValue
	if totalDelta == 0 || idleDelta >= totalDelta {
		return 0
	}
	return 100 * float64(totalDelta-idleDelta) / float64(totalDelta)
}

func filetimeValue(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}

func observeNVIDIAGPU(ctx context.Context) (float64, uint64) {
	command := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=utilization.gpu,memory.free", "--format=csv,noheader,nounits")
	output, err := command.Output()
	if err != nil {
		return 0, 0
	}
	var maxUtilization float64
	var totalFreeMiB uint64
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ",")
		if len(parts) != 2 {
			continue
		}
		utilization, utilizationErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		freeMiB, memoryErr := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if utilizationErr != nil || memoryErr != nil {
			continue
		}
		if utilization > maxUtilization {
			maxUtilization = utilization
		}
		totalFreeMiB += freeMiB
	}
	return maxUtilization, totalFreeMiB << 20
}
