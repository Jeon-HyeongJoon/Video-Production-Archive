package jobcontrol

import (
	"context"
	"errors"

	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
)

var ErrProjectionUnavailable = errors.New("job projection is unavailable")

type SnapshotLoader interface {
	Load(targetID string) (command.Snapshot, error)
}

type SnapshotLister interface {
	ListSnapshots() ([]command.Snapshot, error)
}

type ProjectionWriter interface {
	Replace(context.Context, archiveworkspace.JobView) error
}

type Dispatcher interface {
	DispatchAvailable(context.Context) error
}

type Service struct {
	executor   *command.Executor
	states     SnapshotLoader
	projection ProjectionWriter
	dispatcher Dispatcher
}

func (service *Service) SetDispatcher(dispatcher Dispatcher) error {
	if dispatcher == nil {
		return ErrProjectionUnavailable
	}
	service.dispatcher = dispatcher
	return nil
}

func New(executor *command.Executor, states SnapshotLoader, projection ProjectionWriter) (*Service, error) {
	if executor == nil || states == nil || projection == nil {
		return nil, ErrProjectionUnavailable
	}
	return &Service{executor: executor, states: states, projection: projection}, nil
}

// RebuildProjections restores the disposable Job query model from the
// authoritative aggregate ledger. The ledger also contains other aggregate
// types, so snapshots that are not valid Jobs are intentionally ignored.
func RebuildProjections(ctx context.Context, states SnapshotLister, projection ProjectionWriter) error {
	if states == nil || projection == nil {
		return ErrProjectionUnavailable
	}
	snapshots, err := states.ListSnapshots()
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		state, decodeErr := job.Decode(snapshot)
		if errors.Is(decodeErr, job.ErrInvalidJob) {
			continue
		}
		if decodeErr != nil {
			return decodeErr
		}
		view, projectErr := Project(state)
		if projectErr != nil {
			return projectErr
		}
		if replaceErr := projection.Replace(ctx, view); replaceErr != nil {
			return replaceErr
		}
	}
	return nil
}

func (service *Service) Execute(ctx context.Context, envelope command.Envelope) (command.Result, error) {
	result, err := service.executor.Execute(envelope)
	if err != nil {
		return command.Result{}, err
	}
	snapshot, err := service.states.Load(envelope.TargetID)
	if err != nil {
		return command.Result{}, err
	}
	state, err := job.Decode(snapshot)
	if err != nil {
		return command.Result{}, err
	}
	view, err := Project(state)
	if err != nil {
		return command.Result{}, err
	}
	if err := service.projection.Replace(ctx, view); err != nil {
		return command.Result{}, err
	}
	if (envelope.CommandType == job.CreateCommand || envelope.CommandType == job.RecoverExpiredExecutionCommand) && service.dispatcher != nil {
		if err := service.dispatcher.DispatchAvailable(ctx); err != nil {
			return command.Result{}, err
		}
	}
	return result, nil
}

func Project(state job.State) (archiveworkspace.JobView, error) {
	steps := make([]archiveworkspace.StepStatus, 0, len(state.Steps))
	for _, step := range state.Steps {
		steps = append(steps, archiveworkspace.StepStatus{
			ID: step.Workflow.ID, State: step.Workflow.State, Progress: step.Progress,
			ExecutorKind: step.ExecutorKind, ActorID: step.ActorID, WorkerID: step.WorkerID,
			WaitReason: step.Workflow.WaitReason, ErrorClass: step.ErrorClass, LogReference: step.LogReference,
			LastEventAt: step.LastEventAt, DependencyIDs: append([]string(nil), step.Workflow.Dependencies...),
			ProgressEvidence: append([]job.StepProgressEvidence(nil), step.ProgressEvidence...),
			FailureEvidence:  append([]job.StepFailureEvidence(nil), step.FailureEvidence...),
			RecoveryEvidence: append([]job.StepRecoveryEvidence(nil), step.RecoveryEvidence...),
		})
	}
	return archiveworkspace.Project(archiveworkspace.ProjectionInput{
		Job: archiveworkspace.Job{
			ID: state.ID, Revision: state.Revision, TemplateName: state.TemplateName,
			TargetTitle: state.TargetTitle, CurrentStepID: state.CurrentStepID,
		},
		Steps: steps,
	})
}
