// Package job owns the persisted execution instance created from one Template.
package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

const (
	CreateCommand = "job.create"
	CancelCommand = "job.cancel"
)

var (
	ErrInvalidJob      = errors.New("job is invalid")
	ErrUnsupportedType = errors.New("job command type is unsupported")
)

type Step struct {
	Workflow                workflow.Step               `json:"workflow"`
	ResourceClass           vocabulary.ResourceClass    `json:"resource_class"`
	ExecutorKind            vocabulary.ExecutorKind     `json:"executor_kind"`
	FallbackExecutorKind    vocabulary.ExecutorKind     `json:"fallback_executor_kind,omitempty"`
	RequiredProcessingKinds []vocabulary.ProcessingKind `json:"required_processing_kinds,omitempty"`
	Progress                float64                     `json:"progress"`
	ActorID                 string                      `json:"actor_id,omitempty"`
	WorkerID                string                      `json:"worker_id,omitempty"`
	ErrorClass              string                      `json:"error_class,omitempty"`
	LogReference            string                      `json:"log_reference,omitempty"`
	LastEventAt             time.Time                   `json:"last_event_at"`
}

type State struct {
	ID            string               `json:"id"`
	Revision      uint64               `json:"revision"`
	TemplateName  string               `json:"template_name"`
	TargetTitle   string               `json:"target_title"`
	ControlMode   workflow.ControlMode `json:"control_mode"`
	CurrentStepID string               `json:"current_step_id"`
	Steps         []Step               `json:"steps"`
}

type CreateParameters struct {
	TemplateName string               `json:"template_name"`
	TargetTitle  string               `json:"target_title"`
	ControlMode  workflow.ControlMode `json:"control_mode"`
}

type Handler struct {
	now func() time.Time
}

func NewHandler(now func() time.Time) (*Handler, error) {
	if now == nil {
		return nil, ErrInvalidJob
	}
	return &Handler{now: now}, nil
}

func (handler *Handler) Handle(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	switch envelope.CommandType {
	case CreateCommand:
		return handler.create(envelope, snapshot)
	case CancelCommand:
		return handler.cancel(envelope, snapshot)
	default:
		return command.Change{}, command.DeterministicFailure("UNSUPPORTED_JOB_COMMAND", ErrUnsupportedType)
	}
}

func (handler *Handler) create(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	if snapshot.Revision != 0 || len(snapshot.State) != 0 {
		return command.Change{}, command.DeterministicFailure("JOB_ALREADY_EXISTS", command.ErrRevisionConflict)
	}
	var parameters CreateParameters
	if err := decodeParameters(envelope.Parameters, &parameters); err != nil {
		return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
	}
	template, ok := templateByName(strings.TrimSpace(parameters.TemplateName))
	if !ok || strings.TrimSpace(parameters.TargetTitle) == "" || !validControlMode(parameters.ControlMode) {
		return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
	}
	if err := template.Validate(); err != nil {
		return command.Change{}, fmt.Errorf("validate template: %w", err)
	}

	now := handler.now().UTC()
	stepIDs := make([]string, 0, len(template.Steps))
	for id := range template.Steps {
		stepIDs = append(stepIDs, id)
	}
	sort.Strings(stepIDs)
	steps := make([]Step, 0, len(stepIDs))
	currentStepID := ""
	for _, id := range stepIDs {
		definition := template.Steps[id]
		if currentStepID == "" && len(definition.DependsOn) == 0 {
			currentStepID = id
		}
		steps = append(steps, Step{
			Workflow: workflow.Step{
				ID: definition.ID, Dependencies: append([]string(nil), definition.DependsOn...), State: workflow.Queued,
				ControlMode: parameters.ControlMode, MaxAttempts: 3, ApprovalRequired: definition.ApprovalRequired,
			},
			ResourceClass: definition.ResourceClass, ExecutorKind: definition.ExecutorKind,
			FallbackExecutorKind:    definition.FallbackExecutorKind,
			RequiredProcessingKinds: append([]vocabulary.ProcessingKind(nil), definition.RequiredProcessingKinds...),
			LastEventAt:             now,
		})
	}
	if currentStepID == "" {
		return command.Change{}, fmt.Errorf("template %s has no initial step", template.Name)
	}
	state := State{
		ID: envelope.TargetID, Revision: 1, TemplateName: template.Name,
		TargetTitle: strings.TrimSpace(parameters.TargetTitle), ControlMode: parameters.ControlMode,
		CurrentStepID: currentStepID, Steps: steps,
	}
	return stateChange(envelope, state, "JobCreated", map[string]any{
		"template_name": state.TemplateName, "target_title": state.TargetTitle,
	})
}

func (handler *Handler) cancel(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	state, err := decodeState(snapshot)
	if err != nil {
		return command.Change{}, err
	}
	step := state.currentStep()
	if step == nil || step.Workflow.Transition(workflow.Cancelled) != nil {
		return command.Change{}, command.DeterministicFailure("JOB_CANNOT_CANCEL", workflow.ErrIllegalTransition)
	}
	step.LastEventAt = handler.now().UTC()
	state.Revision = snapshot.Revision + 1
	return stateChange(envelope, state, "JobCancelled", map[string]any{"step_id": step.Workflow.ID})
}

func Decode(snapshot command.Snapshot) (State, error) {
	return decodeState(snapshot)
}

func decodeState(snapshot command.Snapshot) (State, error) {
	var state State
	if snapshot.TargetID == "" || snapshot.Revision == 0 || len(snapshot.State) == 0 || json.Unmarshal(snapshot.State, &state) != nil ||
		state.ID != snapshot.TargetID || state.Revision != snapshot.Revision || state.TemplateName == "" || state.TargetTitle == "" ||
		!validControlMode(state.ControlMode) || state.currentStep() == nil {
		return State{}, ErrInvalidJob
	}
	return state, nil
}

func (state *State) currentStep() *Step {
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == state.CurrentStepID {
			return &state.Steps[index]
		}
	}
	return nil
}

func stateChange(envelope command.Envelope, state State, eventType string, payload map[string]any) (command.Change, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return command.Change{}, err
	}
	return command.Change{
		Revision: state.Revision, State: encoded, Value: state,
		Events: []command.Event{{EventID: envelope.CommandID + ":event", EventType: eventType, Payload: payload}},
	}, nil
}

func decodeParameters(source map[string]any, target any) error {
	payload, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

func validControlMode(mode workflow.ControlMode) bool {
	return mode == workflow.AgentMode || mode == workflow.AssistedMode || mode == workflow.ManualMode
}

func templateByName(name string) (pipeline.Template, bool) {
	switch name {
	case pipeline.OccasionRecording().Name:
		return pipeline.OccasionRecording(), true
	case pipeline.VJCompilation().Name:
		return pipeline.VJCompilation(), true
	case pipeline.FinalEdit().Name:
		return pipeline.FinalEdit(), true
	case pipeline.LargeFileIntake().Name:
		return pipeline.LargeFileIntake(), true
	default:
		return pipeline.Template{}, false
	}
}
