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
	ResourceClass           vocabulary.ResourceClass
	RequiredProcessingKinds []vocabulary.ProcessingKind
	ExecutionSpec           execution.StepExecutionSpec
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
	SettleWorkerStep(context.Context, Result, time.Time) (job.State, error)
	RecoverExpiredReservations(context.Context, time.Time) ([]job.State, error)
	PendingWorkerCancellation(context.Context, string, string, string, uint64, time.Time) (Cancellation, error)
	SettleWorkerCancellation(context.Context, CancellationResult, time.Time) (job.State, error)
}

type ProjectionWriter interface {
	Replace(context.Context, archiveworkspace.JobView) error
}

type Dispatcher interface {
	DispatchAvailable(context.Context) error
}

type Service struct {
	store      Store
	projection ProjectionWriter
	dispatcher Dispatcher
	now        func() time.Time
}

func New(store Store, projection ProjectionWriter, dispatcher Dispatcher, now func() time.Time) (*Service, error) {
	if store == nil || projection == nil || dispatcher == nil || now == nil {
		return nil, ErrInvalidAssignment
	}
	return &Service{store: store, projection: projection, dispatcher: dispatcher, now: now}, nil
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
	if err := service.project(ctx, state); err != nil {
		return err
	}
	return service.dispatcher.DispatchAvailable(ctx)
}

func (service *Service) PendingCancellation(ctx context.Context, workerID, jobID, stepID string, fencingToken uint64) (Cancellation, error) {
	return service.store.PendingWorkerCancellation(ctx, workerID, jobID, stepID, fencingToken, service.now().UTC())
}

func (service *Service) ReportCancellation(ctx context.Context, result CancellationResult) error {
	state, err := service.store.SettleWorkerCancellation(ctx, result, service.now().UTC())
	if err != nil {
		return err
	}
	if err := service.project(ctx, state); err != nil {
		return err
	}
	return service.dispatcher.DispatchAvailable(ctx)
}

func (service *Service) project(ctx context.Context, state job.State) error {
	view, err := jobcontrol.Project(state)
	if err != nil {
		return err
	}
	return service.projection.Replace(ctx, view)
}
