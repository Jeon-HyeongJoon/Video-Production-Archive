package workerclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"videoarchive/internal/core/execution"
)

type observation struct {
	sequence uint64
	update   execution.ProgressUpdate
}

// Progress delivery owns a small ordered queue per attempt. Lease renewal and
// subprocess pipes can continue even when Control stops accepting telemetry.
type progressDelivery struct {
	mu       sync.Mutex
	produced uint64
	pending  chan observation
	done     chan struct{}
	stop     context.CancelFunc
}

func (client *Client) newProgressDelivery(ctx context.Context, assignment Assignment, reportError func(error)) *progressDelivery {
	ctx, cancel := context.WithCancel(ctx)
	d := &progressDelivery{pending: make(chan observation, 32), done: make(chan struct{}), stop: cancel}
	go func() {
		defer close(d.done)
		for {
			select {
			case <-ctx.Done():
				return
			case sample, ok := <-d.pending:
				if !ok || ctx.Err() != nil {
					return
				}
				if err := client.reportProgress(ctx, assignment, sample.sequence, sample.update); err != nil && ctx.Err() == nil {
					reportError(fmt.Errorf("Job %s Step %s token %d progress %d: %w", assignment.JobID, assignment.StepID, assignment.FencingToken, sample.sequence, err))
				}
			}
		}
	}()
	return d
}

func (d *progressDelivery) observe(_ context.Context, update execution.ProgressUpdate) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.produced++
	update.WorkerObservedAt = time.Now().UTC()
	select {
	case d.pending <- observation{d.produced, update}:
	default: // The sequence gap is detectable at the next sample or settlement.
	}
	return nil
}

func (d *progressDelivery) finish() uint64 {
	close(d.pending) // The executor has returned and all process readers have joined.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
		d.stop()
		<-d.done
	}
	d.stop()
	return d.produced
}
