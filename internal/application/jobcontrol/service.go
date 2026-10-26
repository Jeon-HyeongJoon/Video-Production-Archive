package jobcontrol

import (
	"context"
	"errors"

	"videoarchive/internal/application/dashboard"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
)

var ErrProjectionUnavailable = errors.New("job projection is unavailable")

type SnapshotLoader interface {
	Load(targetID string) (command.Snapshot, error)
}

type ProjectionWriter interface {
	Replace(context.Context, dashboard.JobView) error
}

type Service struct {
	executor   *command.Executor
	states     SnapshotLoader
	projection ProjectionWriter
}

func New(executor *command.Executor, states SnapshotLoader, projection ProjectionWriter) (*Service, error) {
	if executor == nil || states == nil || projection == nil {
		return nil, ErrProjectionUnavailable
	}
	return &Service{executor: executor, states: states, projection: projection}, nil
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
	return result, nil
}

func Project(state job.State) (dashboard.JobView, error) {
	steps := make([]dashboard.StepStatus, 0, len(state.Steps))
	for _, step := range state.Steps {
		steps = append(steps, dashboard.StepStatus{
			ID: step.Workflow.ID, State: step.Workflow.State, Progress: step.Progress,
			ExecutorKind: step.ExecutorKind, ActorID: step.ActorID, WorkerID: step.WorkerID,
			WaitReason: step.Workflow.WaitReason, ErrorClass: step.ErrorClass, LogReference: step.LogReference,
			LastEventAt: step.LastEventAt, DependencyIDs: append([]string(nil), step.Workflow.Dependencies...),
		})
	}
	return dashboard.Project(dashboard.ProjectionInput{
		Job: dashboard.Job{
			ID: state.ID, Revision: state.Revision, TemplateName: state.TemplateName,
			TargetTitle: state.TargetTitle, ControlMode: state.ControlMode, CurrentStepID: state.CurrentStepID,
		},
		Steps: steps,
	})
}
