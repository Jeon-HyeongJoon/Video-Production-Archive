// Package job owns the persisted execution instance created from one Template.
package job

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/resource"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

const (
	CreateCommand                  = "job.create"
	CancelCommand                  = "job.cancel"
	RecoverExpiredExecutionCommand = "job.recover_expired_execution"
)

var (
	ErrInvalidJob      = errors.New("job is invalid")
	ErrUnsupportedType = errors.New("job command type is unsupported")
)

type Step struct {
	Workflow                workflow.Step                `json:"workflow"`
	ResourceClass           vocabulary.ResourceClass     `json:"resource_class"`
	ExecutorKind            vocabulary.ExecutorKind      `json:"executor_kind"`
	FallbackExecutorKind    vocabulary.ExecutorKind      `json:"fallback_executor_kind,omitempty"`
	RequiredProcessingKinds []vocabulary.ProcessingKind  `json:"required_processing_kinds,omitempty"`
	Progress                float64                      `json:"progress"`
	ActorID                 string                       `json:"actor_id,omitempty"`
	WorkerID                string                       `json:"worker_id,omitempty"`
	ErrorClass              string                       `json:"error_class,omitempty"`
	WaitTargetStorageID     string                       `json:"wait_target_storage_id,omitempty"`
	LogReference            string                       `json:"log_reference,omitempty"`
	LastEventAt             time.Time                    `json:"last_event_at"`
	ReadyAt                 time.Time                    `json:"ready_at,omitempty"`
	NextEligibleAt          time.Time                    `json:"next_eligible_at,omitempty"`
	ExecutionPriority       resource.ExecutionPriority   `json:"execution_priority"`
	EstimatedMemoryBytes    uint64                       `json:"estimated_memory_bytes"`
	EstimatedScratchBytes   uint64                       `json:"estimated_scratch_bytes"`
	RequiredStorageIDs      []string                     `json:"required_storage_ids,omitempty"`
	PhysicalDeviceIDs       []string                     `json:"physical_device_ids,omitempty"`
	ExecutionSpec           *execution.StepExecutionSpec `json:"execution_spec,omitempty"`
	OutputRecord            *execution.StepOutputRecord  `json:"output_record,omitempty"`
	ProgressEvidence        []StepProgressEvidence       `json:"progress_evidence,omitempty"`
	FailureEvidence         []StepFailureEvidence        `json:"failure_evidence,omitempty"`
	RecoveryEvidence        []StepRecoveryEvidence       `json:"recovery_evidence,omitempty"`
}

// StepProgressEvidence is the immutable, transport-safe execution progress
// retained while a Worker holds the current fencing token.
type StepProgressEvidence struct {
	EvidenceID     string                   `json:"evidence_id"`
	StepID         string                   `json:"step_id"`
	WorkerID       string                   `json:"worker_id"`
	FencingToken   uint64                   `json:"fencing_token"`
	ReportSequence uint64                   `json:"report_sequence"`
	ObservedAt     time.Time                `json:"observed_at"`
	Update         execution.ProgressUpdate `json:"update"`
}

// StepFailureEvidence is the immutable, transport-safe observation retained for
// every unsuccessful Worker settlement. It deliberately excludes raw process
// output, executable arguments, credentials and filesystem paths.
type StepFailureEvidence struct {
	EvidenceID          string                `json:"evidence_id"`
	StepID              string                `json:"step_id"`
	Operation           execution.Operation   `json:"operation"`
	FailureSequence     uint64                `json:"failure_sequence"`
	WorkflowAttempt     int                   `json:"workflow_attempt"`
	WorkerID            string                `json:"worker_id"`
	FencingToken        uint64                `json:"fencing_token"`
	FailureClass        workflow.FailureClass `json:"failure_class"`
	WaitReason          vocabulary.WaitReason `json:"wait_reason,omitempty"`
	WaitTargetStorageID string                `json:"wait_target_storage_id,omitempty"`
	ErrorClass          string                `json:"error_class"`
	CauseCode           string                `json:"cause_code"`
	Summary             string                `json:"summary"`
	ProcessExitCode     *int                  `json:"process_exit_code,omitempty"`
	LogReference        string                `json:"log_reference,omitempty"`
	ObservedAt          time.Time             `json:"observed_at"`
	ResultingState      workflow.StepState    `json:"resulting_state"`
}

// StepRecoveryEvidence is the immutable reason and execution boundary for one
// evidence-bound recovery. A recovery never erases the failure evidence that
// caused it and never stores raw process output or filesystem paths.
type StepRecoveryEvidence struct {
	EvidenceID            string               `json:"evidence_id"`
	ActionCode            string               `json:"action_code"`
	StepID                string               `json:"step_id"`
	ReservationID         string               `json:"reservation_id"`
	FencingToken          uint64               `json:"fencing_token"`
	DiagnosticFingerprint string               `json:"diagnostic_fingerprint"`
	Basis                 []string             `json:"basis"`
	ActorType             vocabulary.ActorType `json:"actor_type"`
	ActorID               string               `json:"actor_id"`
	AppliedAt             time.Time            `json:"applied_at"`
	ResultingState        workflow.StepState   `json:"resulting_state"`
}

type RecoverExpiredExecutionParameters struct {
	ActionCode            string   `json:"action_code"`
	StepID                string   `json:"step_id"`
	ReservationID         string   `json:"reservation_id"`
	FencingToken          uint64   `json:"fencing_token"`
	DiagnosticFingerprint string   `json:"diagnostic_fingerprint"`
	Basis                 []string `json:"basis"`
}

type State struct {
	ID                   string `json:"id"`
	Revision             uint64 `json:"revision"`
	TemplateName         string `json:"template_name"`
	TargetTitle          string `json:"target_title"`
	CurrentStepID        string `json:"current_step_id"`
	Steps                []Step `json:"steps"`
	LastDispatchSequence uint64 `json:"last_dispatch_sequence"`
}

type CreateParameters struct {
	TemplateName            string                                 `json:"template_name"`
	TargetTitle             string                                 `json:"target_title"`
	ExecutionPriority       resource.ExecutionPriority             `json:"execution_priority,omitempty"`
	RequiredStorageIDs      []string                               `json:"required_storage_ids,omitempty"`
	PhysicalDeviceIDs       []string                               `json:"physical_device_ids,omitempty"`
	StepExecutionSpecs      map[string]execution.StepExecutionSpec `json:"step_execution_specs,omitempty"`
	CompletedControlStepIDs []string                               `json:"completed_control_step_ids,omitempty"`
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
	case RecoverExpiredExecutionCommand:
		return handler.recoverExpiredExecution(envelope, snapshot)
	default:
		return command.Change{}, command.DeterministicFailure("UNSUPPORTED_JOB_COMMAND", ErrUnsupportedType)
	}
}

func (handler *Handler) recoverExpiredExecution(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	if envelope.Actor.Type != command.ArchivistActor || envelope.RequestedVia != "mcp" {
		return command.Change{}, command.DeterministicFailure("RECOVERY_NOT_AUTHORIZED", ErrInvalidJob)
	}
	var parameters RecoverExpiredExecutionParameters
	if decodeParameters(envelope.Parameters, &parameters) != nil || !validRecoveryParameters(parameters) {
		return command.Change{}, command.DeterministicFailure("INVALID_RECOVERY", ErrInvalidJob)
	}
	state, err := decodeState(snapshot)
	if err != nil {
		return command.Change{}, err
	}
	step := state.step(parameters.StepID)
	now := handler.now().UTC()
	if step == nil || step.Workflow.ExecutionLease == nil || step.Workflow.ExecutionLease.FencingToken != parameters.FencingToken ||
		now.Before(step.Workflow.ExecutionLease.ExpiresAt) {
		return command.Change{}, command.DeterministicFailure("RECOVERY_EVIDENCE_STALE", command.ErrRevisionConflict)
	}
	switch step.Workflow.State {
	case workflow.Reserved:
		if step.Workflow.Transition(workflow.Queued) != nil {
			return command.Change{}, command.DeterministicFailure("RECOVERY_NOT_SAFE", workflow.ErrIllegalTransition)
		}
	case workflow.Running:
		if !step.Workflow.RecoverExpiredLease(now) {
			return command.Change{}, command.DeterministicFailure("RECOVERY_NOT_SAFE", workflow.ErrIllegalTransition)
		}
	default:
		return command.Change{}, command.DeterministicFailure("RECOVERY_NOT_SAFE", workflow.ErrIllegalTransition)
	}
	step.Workflow.ExecutionLease = nil
	step.WorkerID = ""
	step.ReadyAt = now
	step.LastEventAt = now
	recovery := StepRecoveryEvidence{
		EvidenceID: recoveryEvidenceID(envelope.CommandID), ActionCode: parameters.ActionCode,
		StepID: step.Workflow.ID, ReservationID: parameters.ReservationID, FencingToken: parameters.FencingToken,
		DiagnosticFingerprint: parameters.DiagnosticFingerprint, Basis: append([]string(nil), parameters.Basis...),
		ActorType: envelope.Actor.Type, ActorID: envelope.Actor.ID, AppliedAt: now, ResultingState: step.Workflow.State,
	}
	step.RecoveryEvidence = append(step.RecoveryEvidence, recovery)
	state.CurrentStepID = step.Workflow.ID
	state.Revision = snapshot.Revision + 1
	return stateChange(envelope, state, "StepRecoveryApplied", map[string]any{
		"step_id": step.Workflow.ID, "recovery_evidence": recovery,
	})
}

func validRecoveryParameters(parameters RecoverExpiredExecutionParameters) bool {
	if parameters.ActionCode != "RECONCILE_EXPIRED_EXECUTION" || strings.TrimSpace(parameters.StepID) == "" ||
		strings.TrimSpace(parameters.ReservationID) == "" || parameters.FencingToken == 0 ||
		len(parameters.DiagnosticFingerprint) != 24 || len(parameters.Basis) == 0 || len(parameters.Basis) > 32 {
		return false
	}
	for _, character := range parameters.DiagnosticFingerprint {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	for _, value := range parameters.Basis {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 || !strings.HasPrefix(value, "EVD-") {
			return false
		}
		for _, character := range value {
			if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' && character != '-' {
				return false
			}
		}
	}
	return true
}

func recoveryEvidenceID(commandID string) string {
	digest := sha256.Sum256([]byte(commandID))
	return "EVD-RECOVERY-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
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
	if !ok || strings.TrimSpace(parameters.TargetTitle) == "" {
		return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
	}
	if err := template.Validate(); err != nil {
		return command.Change{}, fmt.Errorf("validate template: %w", err)
	}
	priority := parameters.ExecutionPriority
	if priority == "" {
		priority = resource.Background
	}
	if priority != resource.Background && priority != resource.Immediate {
		return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
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
		step := Step{
			Workflow: workflow.Step{
				ID: definition.ID, Dependencies: append([]string(nil), definition.DependsOn...), State: workflow.Queued,
				MaxAttempts: 3, ApprovalRequired: definition.ApprovalRequired,
			},
			ResourceClass: definition.ResourceClass, ExecutorKind: definition.ExecutorKind,
			FallbackExecutorKind:    definition.FallbackExecutorKind,
			RequiredProcessingKinds: append([]vocabulary.ProcessingKind(nil), definition.RequiredProcessingKinds...),
			LastEventAt:             now,
			ExecutionPriority:       priority,
			RequiredStorageIDs:      append([]string(nil), parameters.RequiredStorageIDs...),
			PhysicalDeviceIDs:       append([]string(nil), parameters.PhysicalDeviceIDs...),
		}
		if spec, exists := parameters.StepExecutionSpecs[id]; exists {
			if spec.Validate() != nil || spec.JobID != envelope.TargetID || spec.StepID != id || spec.TemplateName != template.Name ||
				definition.ExecutorKind != vocabulary.ExecutorWorker {
				return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
			}
			copy := spec
			step.ExecutionSpec = &copy
		}
		if len(definition.DependsOn) == 0 {
			step.ReadyAt = now
		}
		switch definition.ResourceClass {
		case vocabulary.ResourceHeavy:
			step.EstimatedMemoryBytes = resource.DefaultHeavyMemoryBytes
			step.EstimatedScratchBytes = resource.DefaultHeavyScratchBytes
		case vocabulary.ResourceLight:
			step.EstimatedMemoryBytes = resource.DefaultLightMemoryBytes
			step.EstimatedScratchBytes = resource.DefaultLightScratchBytes
		}
		steps = append(steps, step)
	}
	for stepID := range parameters.StepExecutionSpecs {
		if _, exists := template.Steps[stepID]; !exists {
			return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
		}
	}
	completedControls := make(map[string]bool, len(parameters.CompletedControlStepIDs))
	for _, raw := range parameters.CompletedControlStepIDs {
		stepID := strings.TrimSpace(raw)
		definition, exists := template.Steps[stepID]
		if stepID == "" || completedControls[stepID] || !exists || definition.ExecutorKind != vocabulary.ExecutorControl || len(definition.DependsOn) != 0 {
			return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
		}
		completedControls[stepID] = true
	}
	if len(completedControls) > 0 {
		workflowSteps := make(map[string]*workflow.Step, len(steps))
		for index := range steps {
			workflowSteps[steps[index].Workflow.ID] = &steps[index].Workflow
		}
		for index := range steps {
			step := &steps[index]
			if !completedControls[step.Workflow.ID] {
				continue
			}
			for _, target := range []workflow.StepState{workflow.Reserved, workflow.Running, workflow.Verifying, workflow.Completed} {
				if step.Workflow.Transition(target) != nil {
					return command.Change{}, command.DeterministicFailure("INVALID_JOB", ErrInvalidJob)
				}
			}
			step.Progress = 1
			step.ReadyAt = time.Time{}
			step.LastEventAt = now
		}
		currentStepID = ""
		for index := range steps {
			step := &steps[index]
			if step.Workflow.State == workflow.Queued && step.Workflow.Ready(workflowSteps) {
				step.ReadyAt = now
				currentStepID = step.Workflow.ID
				break
			}
		}
	}
	if currentStepID == "" {
		return command.Change{}, fmt.Errorf("template %s has no initial step", template.Name)
	}
	state := State{
		ID: envelope.TargetID, Revision: 1, TemplateName: template.Name,
		TargetTitle:   strings.TrimSpace(parameters.TargetTitle),
		CurrentStepID: currentStepID, Steps: steps,
	}
	return stateChange(envelope, state, "JobCreated", map[string]any{
		"template_name": state.TemplateName, "target_title": state.TargetTitle,
		"completed_control_step_ids": parameters.CompletedControlStepIDs,
	})
}

func (handler *Handler) cancel(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	state, err := decodeState(snapshot)
	if err != nil {
		return command.Change{}, err
	}
	now := handler.now().UTC()
	pendingStops := make([]string, 0)
	cancelledSteps := make([]string, 0)
	for index := range state.Steps {
		step := &state.Steps[index]
		switch step.Workflow.State {
		case workflow.Completed, workflow.Failed, workflow.Cancelled:
			continue
		case workflow.Running, workflow.Verifying:
			pendingStops = append(pendingStops, step.Workflow.ID)
			step.Workflow.ExecutionLease = nil
			step.LastEventAt = now
			state.CurrentStepID = step.Workflow.ID
		case workflow.Paused:
			if step.Workflow.ExecutionLease != nil {
				pendingStops = append(pendingStops, step.Workflow.ID)
				step.Workflow.ExecutionLease = nil
				step.LastEventAt = now
				state.CurrentStepID = step.Workflow.ID
				continue
			}
			if step.Workflow.Transition(workflow.Cancelled) != nil {
				return command.Change{}, command.DeterministicFailure("JOB_CANNOT_CANCEL", workflow.ErrIllegalTransition)
			}
			step.LastEventAt = now
			cancelledSteps = append(cancelledSteps, step.Workflow.ID)
		case workflow.Queued, workflow.Waiting, workflow.Reserved:
			if step.Workflow.Transition(workflow.Cancelled) != nil {
				return command.Change{}, command.DeterministicFailure("JOB_CANNOT_CANCEL", workflow.ErrIllegalTransition)
			}
			step.Workflow.ExecutionLease = nil
			step.LastEventAt = now
			cancelledSteps = append(cancelledSteps, step.Workflow.ID)
		default:
			return command.Change{}, command.DeterministicFailure("JOB_CANNOT_CANCEL", workflow.ErrIllegalTransition)
		}
	}
	if len(pendingStops) == 0 && len(cancelledSteps) == 0 {
		return command.Change{}, command.DeterministicFailure("JOB_CANNOT_CANCEL", workflow.ErrIllegalTransition)
	}
	state.Revision = snapshot.Revision + 1
	eventType := "JobCancelled"
	if len(pendingStops) > 0 {
		eventType = "JobCancellationRequested"
	}
	return stateChange(envelope, state, eventType, map[string]any{
		"pending_stop_step_ids": pendingStops,
		"cancelled_step_ids":    cancelledSteps,
	})
}

func Decode(snapshot command.Snapshot) (State, error) {
	return decodeState(snapshot)
}

func decodeState(snapshot command.Snapshot) (State, error) {
	var state State
	if snapshot.TargetID == "" || snapshot.Revision == 0 || len(snapshot.State) == 0 || json.Unmarshal(snapshot.State, &state) != nil ||
		state.ID != snapshot.TargetID || state.Revision != snapshot.Revision || state.TemplateName == "" || state.TargetTitle == "" ||
		state.currentStep() == nil {
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
		Events: []command.Event{{
			EventID: envelope.CommandID + ":event", EventType: eventType, CommandID: envelope.CommandID,
			Actor: envelope.Actor, RequestedVia: envelope.RequestedVia, TargetID: envelope.TargetID,
			BeforeRevision: envelope.ExpectedRevision, AfterRevision: state.Revision, Reason: envelope.Reason, Payload: payload,
		}},
	}, nil
}

func decodeParameters(source map[string]any, target any) error {
	payload, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
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
	case pipeline.ArchiveIntakeInspection().Name:
		return pipeline.ArchiveIntakeInspection(), true
	case pipeline.ArchiveTranscription().Name:
		return pipeline.ArchiveTranscription(), true
	default:
		for _, disposition := range []vocabulary.OrganizationDisposition{
			vocabulary.OrganizationAsIs, vocabulary.OrganizationRenderRecordings, vocabulary.OrganizationCompileSourceSequence,
		} {
			template, err := pipeline.ArchiveOrganization(disposition)
			if err == nil && template.Name == name {
				return template, true
			}
		}
		return pipeline.Template{}, false
	}
}
