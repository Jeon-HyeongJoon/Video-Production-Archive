package workercontrol

import (
	"context"
	"errors"
	"time"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/jobcontrol"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrNoWork            = errors.New("no Worker Step is available")
	ErrInvalidAssignment = errors.New("Worker assignment is invalid")
	ErrNoCancellation    = errors.New("no Worker cancellation is pending")
)

type Assignment struct {
	JobID                   string
	StepID                  string
	FencingToken            uint64
	LeaseExpiresAt          time.Time
	LeaseRenewAfter         time.Duration
	ResourceClass           vocabulary.ResourceClass
	RequiredProcessingKinds []vocabulary.ProcessingKind
	ExecutionSpec           execution.StepExecutionSpec
}

type LeaseRenewal struct {
	ExpiresAt  time.Time
	RenewAfter time.Duration
}

type CompletionWork struct {
	ID    string
	State job.State
}

type Result struct {
	WorkerID            string
	JobID               string
	StepID              string
	FencingToken        uint64
	Success             bool
	FailureClass        workflow.FailureClass
	WaitReason          vocabulary.WaitReason
	WaitTargetStorageID string
	ErrorClass          string
	LogReference        string
	OutputRecord        *execution.StepOutputRecord
}

type Cancellation struct {
	ID           string
	WorkerID     string
	JobID        string
	StepID       string
	FencingToken uint64
	RequestedAt  time.Time
}

type CancellationResult struct {
	CancellationID           string
	WorkerID                 string
	JobID                    string
	StepID                   string
	FencingToken             uint64
	ProcessStopped           bool
	CleanupConfirmed         bool
	ExternalEffectReferences []string
	LogReference             string
}

type Store interface {
	ClaimWorkerStep(context.Context, string, time.Time) (Assignment, job.State, error)
	RenewWorkerLease(context.Context, string, string, string, uint64, time.Time) (LeaseRenewal, error)
	SettleWorkerStep(context.Context, Result, time.Time) (job.State, error)
	PendingWorkerCompletions(context.Context, int) ([]CompletionWork, error)
	MarkWorkerCompletionDelivered(context.Context, string, time.Time) error
	RecordWorkerCompletionFailure(context.Context, string, string, time.Time) error
	RecoverExpiredReservations(context.Context, time.Time) ([]job.State, error)
	PendingWorkerCancellation(context.Context, string, string, string, uint64, time.Time) (Cancellation, error)
	SettleWorkerCancellation(context.Context, CancellationResult, time.Time) (job.State, error)
}

func (service *Service) RenewLease(ctx context.Context, workerID, jobID, stepID string, fencingToken uint64) (LeaseRenewal, error) {
	if workerID == "" || jobID == "" || stepID == "" || fencingToken == 0 {
		return LeaseRenewal{}, ErrInvalidAssignment
	}
	return service.store.RenewWorkerLease(ctx, workerID, jobID, stepID, fencingToken, service.now().UTC())
}

type ProjectionWriter interface {
	Replace(context.Context, archiveworkspace.JobView) error
}

type Dispatcher interface {
	DispatchAvailable(context.Context) error
}

type CompletionObserver interface {
	ObserveCompletion(context.Context, job.State) error
}

type Service struct {
	store      Store
	projection ProjectionWriter
	dispatcher Dispatcher
	observers  []CompletionObserver
	now        func() time.Time
}

func New(store Store, projection ProjectionWriter, dispatcher Dispatcher, now func() time.Time, observers ...CompletionObserver) (*Service, error) {
	if store == nil || projection == nil || dispatcher == nil || now == nil {
		return nil, ErrInvalidAssignment
	}
	for _, observer := range observers {
		if observer == nil {
			return nil, ErrInvalidAssignment
		}
	}
	return &Service{store: store, projection: projection, dispatcher: dispatcher, now: now, observers: append([]CompletionObserver(nil), observers...)}, nil
}

func (service *Service) Claim(ctx context.Context, workerID string) (Assignment, error) {
	assignment, state, err := service.store.ClaimWorkerStep(ctx, workerID, service.now().UTC())
	if errors.Is(err, ErrNoWork) {
		states, recoveryErr := service.store.RecoverExpiredReservations(ctx, service.now().UTC())
		if recoveryErr != nil {
			return Assignment{}, recoveryErr
		}
		for _, recovered := range states {
			if projectionErr := service.project(ctx, recovered); projectionErr != nil {
				return Assignment{}, projectionErr
			}
		}
		if len(states) > 0 {
			if dispatchErr := service.dispatcher.DispatchAvailable(ctx); dispatchErr != nil {
				return Assignment{}, dispatchErr
			}
			assignment, state, err = service.store.ClaimWorkerStep(ctx, workerID, service.now().UTC())
		}
	}
	if err != nil {
		return Assignment{}, err
	}
	if err := service.project(ctx, state); err != nil {
		return Assignment{}, err
	}
	return assignment, nil
}

func (service *Service) Report(ctx context.Context, result Result) error {
	state, err := service.store.SettleWorkerStep(ctx, result, service.now().UTC())
	if err != nil {
		return err
	}
	// Settlement and its completion outbox are the only required synchronous boundary.
	// The small read-model refresh and dispatch keep claim/UI latency low, but
	// they are best-effort and cannot reject the committed report. Potentially
	// large per-file observers run only in ReconcileCompletions.
	_ = service.project(ctx, state)
	_ = service.dispatcher.DispatchAvailable(ctx)
	return nil
}

func (service *Service) ReconcileCompletions(ctx context.Context) error {
	work, err := service.store.PendingWorkerCompletions(ctx, 32)
	if err != nil {
		return err
	}
	var combined error
	for _, item := range work {
		reconcileErr := service.project(ctx, item.State)
		if reconcileErr == nil {
			for _, observer := range service.observers {
				if err := observer.ObserveCompletion(ctx, item.State); err != nil {
					reconcileErr = err
					break
				}
			}
		}
		if reconcileErr == nil {
			reconcileErr = service.dispatcher.DispatchAvailable(ctx)
		}
		if reconcileErr != nil {
			if recordErr := service.store.RecordWorkerCompletionFailure(ctx, item.ID, reconcileErr.Error(), service.now().UTC()); recordErr != nil {
				combined = errors.Join(combined, recordErr)
			}
			combined = errors.Join(combined, reconcileErr)
			continue
		}
		if err := service.store.MarkWorkerCompletionDelivered(ctx, item.ID, service.now().UTC()); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (service *Service) PendingCancellation(ctx context.Context, workerID, jobID, stepID string, fencingToken uint64) (Cancellation, error) {
	return service.store.PendingWorkerCancellation(ctx, workerID, jobID, stepID, fencingToken, service.now().UTC())
}

func (service *Service) ReportCancellation(ctx context.Context, result CancellationResult) error {
	state, err := service.store.SettleWorkerCancellation(ctx, result, service.now().UTC())
	if err != nil {
		return err
	}
	// Cancellation settlement uses the same durable completion outbox as a
	// normal Worker result. Once committed, projection or dispatch failure must
	// not make the Worker retry a state change that already succeeded.
	_ = service.project(ctx, state)
	_ = service.dispatcher.DispatchAvailable(ctx)
	return nil
}

func (service *Service) project(ctx context.Context, state job.State) error {
	view, err := jobcontrol.Project(state)
	if err != nil {
		return err
	}
	return service.projection.Replace(ctx, view)
}
