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
	"videoarchive/internal/core/job"
)

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

type Result struct {
	Command          command.Result           `json:"command"`
	RecoveryEvidence job.StepRecoveryEvidence `json:"recovery_evidence"`
	Diagnostic       workdiagnostics.Snapshot `json:"diagnostic"`
}

type Service struct {
	diagnostics DiagnosticReader
	commands    Commander
}

func New(diagnostics DiagnosticReader, commands Commander) (*Service, error) {
	if diagnostics == nil || commands == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Service{diagnostics: diagnostics, commands: commands}, nil
}

// Apply re-reads the current diagnostic before executing the command. The
// model may choose when to call this method, but it cannot choose a recovery
// that Control did not derive from current persisted evidence.
func (service *Service) Apply(ctx context.Context, reference archivistruntime.WorkReference, envelope command.Envelope) (Result, error) {
	reference, err := archivistruntime.NewWorkReference(reference)
	if err != nil || envelope.CommandType != job.RecoverExpiredExecutionCommand ||
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
	commandResult, err := service.commands.Execute(ctx, envelope)
	if err != nil {
		return Result{}, err
	}
	state, err := recoveryState(commandResult.Value)
	if err != nil {
		return Result{}, command.ErrInvalidChange
	}
	var evidence job.StepRecoveryEvidence
	for _, step := range state.Steps {
		if step.Workflow.ID == parameters.StepID && len(step.RecoveryEvidence) > 0 {
			evidence = step.RecoveryEvidence[len(step.RecoveryEvidence)-1]
			break
		}
	}
	if evidence.EvidenceID == "" || evidence.DiagnosticFingerprint != diagnostic.Fingerprint {
		return Result{}, command.ErrInvalidChange
	}
	after, err := service.diagnostics.Diagnose(ctx, reference)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Command: commandResult, RecoveryEvidence: evidence,
		Diagnostic: workdiagnostics.Sanitize(after),
	}, nil
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
	if parameters.ActionCode == "" || parameters.StepID == "" || parameters.ReservationID == "" ||
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
