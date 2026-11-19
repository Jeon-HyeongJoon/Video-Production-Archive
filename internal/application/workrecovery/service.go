// Package workrecovery applies only recovery actions that the current
// diagnostic projection has declared executable. It is intentionally not a
// generic retry, cancel, shell, or compensation surface.
package workrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/pipeline"
)

const CreateTerminalRecoveryCommand = "job.create_terminal_recovery"

var (
	ErrInvalidConfiguration = errors.New("work recovery configuration is invalid")
	ErrRecoveryNotSafe      = errors.New("work recovery is not supported by current evidence")
)

type DiagnosticReader interface {
	Diagnose(context.Context, archivistruntime.WorkReference) (workdiagnostics.Snapshot, error)
}

type Commander interface {
	Execute(context.Context, command.Envelope) (command.Result, error)
}

type SnapshotLoader interface {
	Load(string) (command.Snapshot, error)
}

type Result struct {
	Command          command.Result           `json:"command"`
	RecoveryEvidence job.StepRecoveryEvidence `json:"recovery_evidence"`
	Diagnostic       workdiagnostics.Snapshot `json:"diagnostic"`
	RecoveryJobID    string                   `json:"recovery_job_id,omitempty"`
}

type Service struct {
	diagnostics DiagnosticReader
	commands    Commander
	states      SnapshotLoader
}

func New(diagnostics DiagnosticReader, commands Commander, states ...SnapshotLoader) (*Service, error) {
	if diagnostics == nil || commands == nil {
		return nil, ErrInvalidConfiguration
	}
	service := &Service{diagnostics: diagnostics, commands: commands}
	if len(states) > 1 || (len(states) == 1 && states[0] == nil) {
		return nil, ErrInvalidConfiguration
	}
	if len(states) == 1 {
		service.states = states[0]
	}
	return service, nil
}

// Apply re-reads the current diagnostic before executing the command. The
// model may choose when to call this method, but it cannot choose a recovery
// that Control did not derive from current persisted evidence.
func (service *Service) Apply(ctx context.Context, reference archivistruntime.WorkReference, envelope command.Envelope) (Result, error) {
	reference, err := archivistruntime.NewWorkReference(reference)
	if err != nil || (envelope.CommandType != job.RecoverExpiredExecutionCommand && envelope.CommandType != CreateTerminalRecoveryCommand) ||
		envelope.Actor.Type != command.ArchivistActor || envelope.RequestedVia != "mcp" {
		return Result{}, ErrRecoveryNotSafe
	}
	diagnostic, err := service.diagnostics.Diagnose(ctx, reference)
	if err != nil {
		return Result{}, err
	}
	parameters, err := recoverParameters(envelope.Parameters)
	if err != nil || parameters.DiagnosticFingerprint != diagnostic.Fingerprint {
		return Result{}, ErrRecoveryNotSafe
	}
	var selected *workdiagnostics.RecoveryAction
	for index := range diagnostic.RecoveryActions {
		candidate := &diagnostic.RecoveryActions[index]
		if candidate.ActionCode == parameters.ActionCode && candidate.JobID == envelope.TargetID &&
			candidate.JobRevision == envelope.ExpectedRevision && candidate.StepID == parameters.StepID &&
			candidate.ReservationID == parameters.ReservationID && candidate.FencingToken == parameters.FencingToken &&
			candidate.DiagnosticFingerprint == parameters.DiagnosticFingerprint && sameEvidence(candidate.Basis, parameters.Basis) {
			selected = candidate
			break
		}
	}
	if selected == nil || !workContainsJob(diagnostic, envelope.TargetID) {
		return Result{}, ErrRecoveryNotSafe
	}
	commandEnvelope := envelope
	if envelope.CommandType == CreateTerminalRecoveryCommand {
		if service.states == nil {
			return Result{}, ErrRecoveryNotSafe
		}
		commandEnvelope, err = service.terminalRecoveryEnvelope(envelope, parameters, *selected)
		if err != nil {
			return Result{}, err
		}
	}
	commandResult, err := service.commands.Execute(ctx, commandEnvelope)
	if err != nil {
		return Result{}, err
	}
	state, err := recoveryState(commandResult.Value)
	if err != nil {
		return Result{}, command.ErrInvalidChange
	}
	var evidence job.StepRecoveryEvidence
	recoveryJobID := ""
	if envelope.CommandType == job.RecoverExpiredExecutionCommand {
		for _, step := range state.Steps {
			if step.Workflow.ID == parameters.StepID && len(step.RecoveryEvidence) > 0 {
				evidence = step.RecoveryEvidence[len(step.RecoveryEvidence)-1]
				break
			}
		}
		if evidence.EvidenceID == "" || evidence.DiagnosticFingerprint != diagnostic.Fingerprint {
			return Result{}, command.ErrInvalidChange
		}
	} else {
		if state.RecoverySource == nil || state.RecoverySource.JobID != envelope.TargetID ||
			state.RecoverySource.FailureEvidenceID != parameters.Basis[0] ||
			state.RecoverySource.DiagnosticFingerprint != diagnostic.Fingerprint {
			return Result{}, command.ErrInvalidChange
		}
		recoveryJobID = state.ID
	}
	after, err := service.diagnostics.Diagnose(ctx, reference)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Command: commandResult, RecoveryEvidence: evidence, RecoveryJobID: recoveryJobID,
		Diagnostic: workdiagnostics.Sanitize(after),
	}, nil
}

func (service *Service) terminalRecoveryEnvelope(request command.Envelope, parameters job.RecoverExpiredExecutionParameters, action workdiagnostics.RecoveryAction) (command.Envelope, error) {
	if parameters.ActionCode != job.RecoveryActionRetryTerminalArchivePublication || action.RecoveryJobID == "" ||
		action.RecoveryJobID != job.TerminalRecoveryJobID(request.TargetID, parameters.Basis[0]) {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	snapshot, err := service.states.Load(request.TargetID)
	if err != nil || snapshot.Revision != request.ExpectedRevision {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	state, err := job.Decode(snapshot)
	if err != nil || state.CurrentStepID != parameters.StepID {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	var source *job.Step
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == parameters.StepID {
			source = &state.Steps[index]
			break
		}
	}
	if source == nil || source.Workflow.State != "FAILED" || source.ExecutionSpec == nil ||
		len(source.FailureEvidence) == 0 || source.Workflow.ExecutionLease != nil {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	latest := source.FailureEvidence[len(source.FailureEvidence)-1]
	if latest.EvidenceID != parameters.Basis[0] || latest.FencingToken != parameters.FencingToken ||
		latest.FailureClass != "TRANSIENT" || latest.ResultingState != "FAILED" || latest.StepID != parameters.StepID {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	if latest.Operation != execution.OperationReviseArchivePackage && latest.Operation != execution.OperationWriteArchiveCatalog {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	recoverySpec, err := terminalRecoverySpec(action.RecoveryJobID, *source.ExecutionSpec)
	if err != nil {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	recoverySource := &job.RecoverySource{
		ActionCode: parameters.ActionCode, JobID: state.ID, JobRevision: state.Revision, StepID: parameters.StepID,
		FailureEvidenceID: latest.EvidenceID, FencingToken: latest.FencingToken,
		DiagnosticFingerprint: parameters.DiagnosticFingerprint, Basis: append([]string(nil), parameters.Basis...),
	}
	create := job.CreateParameters{
		TemplateName:       pipeline.ArchivePublicationRecovery().Name,
		TargetTitle:        "Recovery for " + state.TargetTitle,
		ExecutionPriority:  source.ExecutionPriority,
		RequiredStorageIDs: append([]string(nil), source.RequiredStorageIDs...),
		PhysicalDeviceIDs:  append([]string(nil), source.PhysicalDeviceIDs...),
		StepExecutionSpecs: map[string]execution.StepExecutionSpec{"reconcile_archive_publication": recoverySpec},
		RecoverySource:     recoverySource,
	}
	payload, err := json.Marshal(create)
	if err != nil {
		return command.Envelope{}, err
	}
	var values map[string]any
	if json.Unmarshal(payload, &values) != nil {
		return command.Envelope{}, ErrRecoveryNotSafe
	}
	return command.Envelope{
		CommandID: request.CommandID + ":recovery-job", CommandType: job.CreateCommand,
		Actor: request.Actor, RequestedVia: request.RequestedVia,
		TargetID: action.RecoveryJobID, ExpectedRevision: 0,
		IdempotencyKey: request.IdempotencyKey + ":recovery-job", Reason: request.Reason,
		Parameters: values,
	}, nil
}

func terminalRecoverySpec(recoveryJobID string, source execution.StepExecutionSpec) (execution.StepExecutionSpec, error) {
	archiveStorageID := strings.TrimSpace(source.Parameters.ArchiveStorageID)
	if archiveStorageID == "" && len(source.Outputs) > 0 {
		archiveStorageID = source.Outputs[0].StorageID
	}
	if archiveStorageID == "" {
		return execution.StepExecutionSpec{}, ErrRecoveryNotSafe
	}
	return execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: recoveryJobID, StepID: "reconcile_archive_publication", TemplateName: pipeline.ArchivePublicationRecovery().Name,
		Operation: execution.OperationReconcileArchivePublication,
		Inputs:    append([]execution.InputReference(nil), source.Inputs...),
		Outputs: []execution.OutputTarget{{
			ID:   "CATALOG-RECOVERY-" + strings.TrimPrefix(recoveryJobID, "JOB-RECOVERY-"),
			Kind: execution.OutputArchiveCatalog, StorageID: archiveStorageID, RelativePath: "archive-catalog.csv",
		}},
		Parameters: source.Parameters,
	})
}

func recoveryState(value any) (job.State, error) {
	if state, ok := value.(job.State); ok {
		return state, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return job.State{}, err
	}
	var state job.State
	if json.Unmarshal(payload, &state) != nil || state.ID == "" || state.Revision == 0 {
		return job.State{}, command.ErrInvalidChange
	}
	return state, nil
}

func recoverParameters(source map[string]any) (job.RecoverExpiredExecutionParameters, error) {
	parameters := job.RecoverExpiredExecutionParameters{}
	action, _ := source["action_code"].(string)
	stepID, _ := source["step_id"].(string)
	reservationID, _ := source["reservation_id"].(string)
	fingerprint, _ := source["diagnostic_fingerprint"].(string)
	parameters.ActionCode = strings.TrimSpace(action)
	parameters.StepID = strings.TrimSpace(stepID)
	parameters.ReservationID = strings.TrimSpace(reservationID)
	parameters.DiagnosticFingerprint = strings.TrimSpace(fingerprint)
	switch value := source["fencing_token"].(type) {
	case uint64:
		parameters.FencingToken = value
	case float64:
		if value > 0 && value == float64(uint64(value)) {
			parameters.FencingToken = uint64(value)
		}
	}
	switch values := source["basis"].(type) {
	case []string:
		parameters.Basis = append([]string(nil), values...)
	case []any:
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return parameters, ErrRecoveryNotSafe
			}
			parameters.Basis = append(parameters.Basis, text)
		}
	}
	if parameters.ActionCode == "" || parameters.StepID == "" ||
		parameters.FencingToken == 0 || parameters.DiagnosticFingerprint == "" || len(parameters.Basis) == 0 {
		return parameters, ErrRecoveryNotSafe
	}
	return parameters, nil
}

func sameEvidence(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func workContainsJob(diagnostic workdiagnostics.Snapshot, jobID string) bool {
	for _, item := range diagnostic.Jobs {
		if item.JobID == jobID {
			return true
		}
	}
	return false
}
