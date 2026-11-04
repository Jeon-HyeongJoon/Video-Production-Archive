//go:build !windows

package hostresource

import (
	"context"
	"runtime"
	"syscall"
	"time"

	"videoarchive/internal/core/worker"
)

type portableProbe struct {
	scratchPath string
	now         func() time.Time
}

func New(scratchPath string, now func() time.Time) Probe {
	return &portableProbe{scratchPath: scratchPath, now: now}
}

func (probe *portableProbe) Snapshot(context.Context) (worker.Metrics, error) {
	metrics := readyMetrics(probe.now())
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	metrics.AvailableMemoryBytes = ^uint64(0) - memory.Sys
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(probe.scratchPath, &filesystem); err != nil {
		return worker.Metrics{}, err
	}
	metrics.ScratchFreeBytes = uint64(filesystem.Bavail) * uint64(filesystem.Bsize)
	metrics.IdleStableFor = 10 * time.Minute
	return metrics, nil
}
