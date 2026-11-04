package hostresource

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/core/worker"
)

var ErrObservationFailed = errors.New("host resource observation failed")

type Probe interface {
	Snapshot(context.Context) (worker.Metrics, error)
}

func readyMetrics(now time.Time) worker.Metrics {
	return worker.Metrics{
		ObservedAt: now.UTC(), BusyPhysicalDeviceIDs: map[string]bool{},
		AccessibleStorageIDs: map[string]bool{}, AcceptingWork: true,
	}
}
