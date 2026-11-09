package workercontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

func TestCommittedResultIsAcceptedWhileDurableCompletionRetriesFollowUps(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	state := job.State{
		ID: "JOB-1", Revision: 2, TemplateName: "fixture", TargetTitle: "fixture",
		CurrentStepID: "work",
		Steps: []job.Step{{
			Workflow:     workflow.Step{ID: "work", State: workflow.Completed, MaxAttempts: 1},
			ExecutorKind: vocabulary.ExecutorWorker, Progress: 1, LastEventAt: now,
		}},
	}
	store := &completionStore{state: state, pending: []CompletionWork{{ID: "COMPLETION-1", State: state}}}
	projection := &completionProjection{err: errors.New("projection unavailable")}
	dispatcher := &completionDispatcher{err: errors.New("dispatch unavailable")}
	observer := &completionObserver{}
	service, err := New(store, projection, dispatcher, func() time.Time { return now }, observer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Report(context.Background(), Result{JobID: state.ID}); err != nil {
		t.Fatalf("durably committed Worker result was rejected by a follow-up failure: %v", err)
	}
	if store.settleCalls != 1 || store.delivered {
		t.Fatalf("unexpected immediate settlement state: store=%+v", store)
	}

	projection.err = nil
	dispatcher.err = nil
	if err := service.ReconcileCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.delivered || projection.calls < 2 || observer.calls != 1 || dispatcher.calls < 2 {
		t.Fatalf("durable follow-up did not run projection, observer, and dispatch before delivery: store=%+v projection=%+v observer=%+v dispatcher=%+v",
			store, projection, observer, dispatcher)
	}
}

func TestCommittedCancellationIsAcceptedWhileDurableCompletionRetriesFollowUps(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 30, 0, 0, time.UTC)
	state := job.State{
		ID: "JOB-CANCEL", Revision: 4, TemplateName: "fixture", TargetTitle: "fixture",
		CurrentStepID: "work",
		Steps: []job.Step{{
			Workflow:     workflow.Step{ID: "work", State: workflow.Cancelled, MaxAttempts: 1},
			ExecutorKind: vocabulary.ExecutorWorker, LastEventAt: now,
		}},
	}
	store := &completionStore{state: state, pending: []CompletionWork{{ID: "CANCEL-COMPLETION", State: state}}}
	projection := &completionProjection{err: errors.New("projection unavailable")}
	dispatcher := &completionDispatcher{err: errors.New("dispatch unavailable")}
	service, err := New(store, projection, dispatcher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReportCancellation(context.Background(), CancellationResult{CancellationID: "CANCEL-1"}); err != nil {
		t.Fatalf("durably committed cancellation was rejected by a follow-up failure: %v", err)
	}
	if store.cancellationSettleCalls != 1 || store.delivered {
		t.Fatalf("unexpected cancellation settlement state: store=%+v", store)
	}
	projection.err = nil
	dispatcher.err = nil
	if err := service.ReconcileCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.delivered {
		t.Fatalf("cancellation completion was not delivered: store=%+v", store)
	}
}

type completionStore struct {
	state                   job.State
	pending                 []CompletionWork
	settleCalls             int
	cancellationSettleCalls int
	delivered               bool
}

func (store *completionStore) ClaimWorkerStep(context.Context, string, time.Time) (Assignment, job.State, error) {
	return Assignment{}, job.State{}, ErrNoWork
}
func (store *completionStore) RenewWorkerLease(context.Context, string, string, string, uint64, time.Time) (LeaseRenewal, error) {
	return LeaseRenewal{}, ErrInvalidAssignment
}
func (store *completionStore) SettleWorkerStep(context.Context, Result, time.Time) (job.State, error) {
	store.settleCalls++
	return store.state, nil
}
func (store *completionStore) PendingWorkerCompletions(context.Context, int) ([]CompletionWork, error) {
	return append([]CompletionWork(nil), store.pending...), nil
}
func (store *completionStore) MarkWorkerCompletionDelivered(context.Context, string, time.Time) error {
	store.delivered = true
	store.pending = nil
	return nil
}
func (*completionStore) RecordWorkerCompletionFailure(context.Context, string, string, time.Time) error {
	return nil
}
func (*completionStore) RecoverExpiredReservations(context.Context, time.Time) ([]job.State, error) {
	return nil, nil
}
func (*completionStore) PendingWorkerCancellation(context.Context, string, string, string, uint64, time.Time) (Cancellation, error) {
	return Cancellation{}, ErrNoCancellation
}
func (store *completionStore) SettleWorkerCancellation(context.Context, CancellationResult, time.Time) (job.State, error) {
	store.cancellationSettleCalls++
	return store.state, nil
}

type completionProjection struct {
	calls int
	err   error
}

func (projection *completionProjection) Replace(context.Context, archiveworkspace.JobView) error {
	projection.calls++
	return projection.err
}

type completionDispatcher struct {
	calls int
	err   error
}

func (dispatcher *completionDispatcher) DispatchAvailable(context.Context) error {
	dispatcher.calls++
	return dispatcher.err
}

type completionObserver struct{ calls int }

func (observer *completionObserver) ObserveCompletion(context.Context, job.State) error {
	observer.calls++
	return nil
}
