package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/pipeline"
	"videoarchive/internal/core/qc"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

const (
	StartWorkerStepCommand             = "step.start"
	ReportWorkerStepProgressCommand    = "step.report_progress"
	SettleWorkerStepCommand            = "step.settle"
	SettleWorkerCancellationCommand    = "step.settle_cancellation"
	RecoverWorkerReservationCommand    = "step.recover_reservation"
	RecoverWorkerExecutionLeaseCommand = "step.recover_execution_lease"
)

const maxStepProgressEvidence = 1024

var ErrInvalidWorkerLifecycle = errors.New("Worker Step lifecycle command is invalid")

type StartWorkerStepParameters struct {
	StepID       string `json:"step_id"`
	FencingToken uint64 `json:"fencing_token"`
}

type ReportWorkerStepProgressParameters struct {
	StepID         string                   `json:"step_id"`
	FencingToken   uint64                   `json:"fencing_token"`
	ReportSequence uint64                   `json:"report_sequence"`
	Update         execution.ProgressUpdate `json:"update"`
}

type SettleWorkerStepParameters struct {
	ProgressReportCount uint64                        `json:"progress_report_count,omitempty"`
	StepID              string                        `json:"step_id"`
	FencingToken        uint64                        `json:"fencing_token"`
	Success             bool                          `json:"success"`
	FailureClass        workflow.FailureClass         `json:"failure_class,omitempty"`
	WaitReason          vocabulary.WaitReason         `json:"wait_reason,omitempty"`
	WaitTargetStorageID string                        `json:"wait_target_storage_id,omitempty"`
	ErrorClass          string                        `json:"error_class,omitempty"`
	CauseCode           string                        `json:"cause_code,omitempty"`
	ErrorSummary        string                        `json:"error_summary,omitempty"`
	QCFailure           *qc.Failure                   `json:"qc_failure,omitempty"`
	ProcessExitCode     *int                          `json:"process_exit_code,omitempty"`
	LogReference        string                        `json:"log_reference,omitempty"`
	ProcessLog          *execution.ProcessLogArtifact `json:"process_log,omitempty"`
	OutputRecord        *execution.StepOutputRecord   `json:"output_record,omitempty"`
}

type SettleWorkerCancellationParameters struct {
	StepID                   string   `json:"step_id"`
	CancellationID           string   `json:"cancellation_id"`
	FencingToken             uint64   `json:"fencing_token"`
	CleanupConfirmed         bool     `json:"cleanup_confirmed"`
	ExternalEffectReferences []string `json:"external_effect_references,omitempty"`
	LogReference             string   `json:"log_reference,omitempty"`
}

type RecoverWorkerStepParameters struct {
	StepID string `json:"step_id"`
}

// WorkerLifecycleHandler owns deterministic Worker Step transitions. Storage
// adapters validate reservation/lease rows and atomically persist the returned
// staged change together with those records; they never mutate Job state.
type WorkerLifecycleHandler struct {
	now func() time.Time
}

func NewWorkerLifecycleHandler(now func() time.Time) (*WorkerLifecycleHandler, error) {
	if now == nil {
		return nil, ErrInvalidWorkerLifecycle
	}
	return &WorkerLifecycleHandler{now: now}, nil
}

func (handler *WorkerLifecycleHandler) Handle(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	if handler == nil || handler.now == nil || envelope.TargetID != snapshot.TargetID || envelope.ExpectedRevision != snapshot.Revision {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	switch envelope.CommandType {
	case StartWorkerStepCommand:
		var parameters StartWorkerStepParameters
		if decodeParameters(envelope.Parameters, &parameters) != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		return handler.start(envelope, snapshot, parameters)
	case ReportWorkerStepProgressCommand:
		var parameters ReportWorkerStepProgressParameters
		if decodeParameters(envelope.Parameters, &parameters) != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		return handler.progress(envelope, snapshot, parameters)
	case SettleWorkerStepCommand:
		var parameters SettleWorkerStepParameters
		if decodeParameters(envelope.Parameters, &parameters) != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		return handler.settle(envelope, snapshot, parameters)
	case SettleWorkerCancellationCommand:
		var parameters SettleWorkerCancellationParameters
		if decodeParameters(envelope.Parameters, &parameters) != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		return handler.settleCancellation(envelope, snapshot, parameters)
	case RecoverWorkerReservationCommand, RecoverWorkerExecutionLeaseCommand:
		var parameters RecoverWorkerStepParameters
		if decodeParameters(envelope.Parameters, &parameters) != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		return handler.recover(envelope, snapshot, parameters)
	default:
		return command.Change{}, ErrUnsupportedType
	}
}

func (handler *WorkerLifecycleHandler) progress(
	envelope command.Envelope,
	snapshot command.Snapshot,
	parameters ReportWorkerStepProgressParameters,
) (command.Change, error) {
	state, step, err := workerStepState(envelope, snapshot, parameters.StepID, parameters.FencingToken, true)
	if err != nil || step.Workflow.State != workflow.Running {
		if err != nil {
			return command.Change{}, err
		}
		return command.Change{}, workflow.ErrStaleFencingToken
	}
	if parameters.ReportSequence == 0 || parameters.Update.Validate() != nil || step.ExecutionSpec == nil ||
		parameters.Update.Operation != step.ExecutionSpec.Operation {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	if count := len(step.ProgressEvidence); count > 0 {
		previous := step.ProgressEvidence[count-1]
		if previous.FencingToken == parameters.FencingToken {
			prior, next := previous.Update, parameters.Update
			if parameters.ReportSequence <= previous.ReportSequence || next.Status == execution.ProgressCheckingInputs ||
				prior.Status == execution.ProgressCheckingOutputs || (next.Status == execution.ProgressExecuting && prior.Status != execution.ProgressCheckingInputs) {
				return command.Change{}, ErrInvalidWorkerLifecycle
			}
			if !prior.Status.IsPhase() && !next.Status.IsPhase() && (next.StepFraction() < prior.StepFraction() ||
				next.InputCount != prior.InputCount || next.InputIndex < prior.InputIndex ||
				(next.InputIndex == prior.InputIndex && (next.ChunkCount != prior.ChunkCount || next.ChunkIndex < prior.ChunkIndex || next.OutputMillis < prior.OutputMillis))) {
				return command.Change{}, ErrInvalidWorkerLifecycle
			}
		}
	}
	now := handler.now().UTC()
	evidence := StepProgressEvidence{
		EvidenceID: progressEvidenceID(state.ID, step.Workflow.ID, parameters.FencingToken, parameters.ReportSequence),
		StepID:     step.Workflow.ID, WorkerID: envelope.Actor.ID, FencingToken: parameters.FencingToken,
		ReportSequence: parameters.ReportSequence, ObservedAt: now, Update: parameters.Update,
	}
	if len(step.ProgressEvidence) >= maxStepProgressEvidence {
		step.ProgressHistoryOmitted += uint64(len(step.ProgressEvidence) - maxStepProgressEvidence + 1)
		step.ProgressEvidence = step.ProgressEvidence[len(step.ProgressEvidence)-maxStepProgressEvidence+1:]
	}
	step.ProgressEvidence = append(step.ProgressEvidence, evidence)
	if !parameters.Update.Status.IsPhase() {
		step.Progress = parameters.Update.StepFraction()
	}
	step.LastEventAt = now
	state.CurrentStepID = step.Workflow.ID
	state.Revision = snapshot.Revision + 1
	return stateChange(envelope, state, "StepProgressObserved", map[string]any{
		"step_id": step.Workflow.ID, "fencing_token": parameters.FencingToken, "progress_evidence": evidence,
	})
}

func progressEvidenceID(jobID, stepID string, fencingToken, reportSequence uint64) string {
	digest := sha256.Sum256([]byte(jobID + "\x00" + stepID + "\x00" + strconv.FormatUint(fencingToken, 10) + "\x00" + strconv.FormatUint(reportSequence, 10)))
	return "EVD-PROGRESS-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
}

func (handler *WorkerLifecycleHandler) start(
	envelope command.Envelope,
	snapshot command.Snapshot,
	parameters StartWorkerStepParameters,
) (command.Change, error) {
	state, step, err := workerStepState(envelope, snapshot, parameters.StepID, parameters.FencingToken, true)
	if err != nil {
		return command.Change{}, err
	}
	if step.Workflow.State != workflow.Reserved || step.Workflow.Transition(workflow.Running) != nil {
		return command.Change{}, workflow.ErrIllegalTransition
	}
	now := handler.now().UTC()
	// FailureEvidence is the immutable history. The current-attempt fields must
	// describe the execution that just started, not a preceding retry.
	step.ErrorClass = ""
	step.LogReference = ""
	step.WaitTargetStorageID = ""
	step.Progress = 0
	step.LastEventAt = now
	state.CurrentStepID = step.Workflow.ID
	state.Revision = snapshot.Revision + 1
	return stateChange(envelope, state, "StepStarted", map[string]any{
		"step_id": step.Workflow.ID, "fencing_token": parameters.FencingToken,
	})
}

func (handler *WorkerLifecycleHandler) settle(
	envelope command.Envelope,
	snapshot command.Snapshot,
	parameters SettleWorkerStepParameters,
) (command.Change, error) {
	state, step, err := workerStepState(envelope, snapshot, parameters.StepID, parameters.FencingToken, true)
	if err != nil || step.Workflow.State != workflow.Running {
		if err != nil {
			return command.Change{}, err
		}
		return command.Change{}, workflow.ErrStaleFencingToken
	}
	if step.ExecutionSpec == nil || step.ExecutionSpec.Validate() != nil || step.OutputRecord != nil {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	now := handler.now().UTC()
	eventType := "StepCompleted"
	if parameters.Success {
		if parameters.OutputRecord == nil || parameters.OutputRecord.SpecDigest != step.ExecutionSpec.Digest ||
			parameters.OutputRecord.FencingToken != parameters.FencingToken || parameters.OutputRecord.WorkerID != envelope.Actor.ID {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		outputRecord, outputErr := execution.NewStepOutputRecordWithInspections(
			*step.ExecutionSpec, parameters.FencingToken, envelope.Actor.ID, now,
			parameters.OutputRecord.Outputs, parameters.OutputRecord.VerificationReferences, parameters.OutputRecord.InspectedMedia,
		)
		if outputErr != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		if step.Workflow.Transition(workflow.Verifying) != nil || step.Workflow.Transition(workflow.Completed) != nil {
			return command.Change{}, workflow.ErrIllegalTransition
		}
		step.OutputRecord = &outputRecord
		step.Progress = 1
		step.WaitTargetStorageID = ""
		if err := materializeDependentExecutionSpecs(&state); err != nil {
			return command.Change{}, err
		}
	} else {
		if parameters.OutputRecord != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		deviceWait := parameters.FailureClass == workflow.FailureResource && parameters.WaitReason == vocabulary.WaitDevice
		if deviceWait != (strings.TrimSpace(parameters.WaitTargetStorageID) != "") {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		if err := step.Workflow.RecordFailure(workflow.Failure{Class: parameters.FailureClass, WaitReason: parameters.WaitReason}); err != nil {
			return command.Change{}, err
		}
		step.WaitTargetStorageID = strings.TrimSpace(parameters.WaitTargetStorageID)
		step.ErrorClass = safeFailureCode(parameters.ErrorClass, "WORKER_REPORTED_FAILURE")
		processLog, processLogErr := safeProcessLogArtifact(parameters.ProcessLog, parameters.LogReference)
		if processLogErr != nil {
			return command.Change{}, ErrInvalidWorkerLifecycle
		}
		if processLog != nil && step.ExecutionSpec.Operation == execution.OperationTranscribeAudio {
			processLog.SanitizedTail = ""
		}
		failedQC := parameters.QCFailure.Safe()
		if parameters.QCFailure != nil || parameters.CauseCode == "QC_CHECK_FAILED" {
			if parameters.CauseCode != "QC_CHECK_FAILED" || parameters.FailureClass != workflow.FailureInput || !matchesFailedQC(step.ExecutionSpec, failedQC) {
				return command.Change{}, ErrInvalidWorkerLifecycle
			}
		}
		failureEvidence := StepFailureEvidence{QCFailure: failedQC,
			EvidenceID: failureEvidenceID(state.ID, step.Workflow.ID, parameters.FencingToken),
			StepID:     step.Workflow.ID, Operation: step.ExecutionSpec.Operation,
			FailureSequence: uint64(len(step.FailureEvidence) + 1), WorkflowAttempt: step.Workflow.Attempt,
			WorkerID: envelope.Actor.ID, FencingToken: parameters.FencingToken,
			FailureClass: parameters.FailureClass, WaitReason: parameters.WaitReason,
			WaitTargetStorageID: step.WaitTargetStorageID,
			ErrorClass:          step.ErrorClass, CauseCode: safeFailureCode(parameters.CauseCode, step.ErrorClass),
			Summary: safeFailureSummary(parameters.ErrorSummary), ProcessExitCode: cloneExitCode(parameters.ProcessExitCode),
			LogReference: safeFailureLogReference(parameters.LogReference), ProcessLog: processLog, ObservedAt: now,
			ResultingState: step.Workflow.State,
		}
		step.FailureEvidence = append(step.FailureEvidence, failureEvidence)
		eventType = "StepFailed"
		if step.Workflow.State == workflow.Queued || step.Workflow.State == workflow.Waiting {
			eventType = "StepRetryScheduled"
		}
	}
	if parameters.ProgressReportCount > 0 {
		if step.ProgressReportCounts == nil {
			step.ProgressReportCounts = make(map[uint64]uint64)
		}
		step.ProgressReportCounts[parameters.FencingToken] = parameters.ProgressReportCount
	}
	step.Workflow.ExecutionLease = nil
	if parameters.Success {
		step.ErrorClass = ""
		step.LogReference = strings.TrimSpace(parameters.LogReference)
	} else {
		step.LogReference = step.FailureEvidence[len(step.FailureEvidence)-1].LogReference
	}
	step.LastEventAt = now
	activateNextReadyStep(&state, now)
	state.Revision = snapshot.Revision + 1
	payload := map[string]any{
		"step_id": step.Workflow.ID, "fencing_token": parameters.FencingToken,
		"success": parameters.Success, "output_ids": workerStepOutputIDs(step.OutputRecord),
	}
	if !parameters.Success {
		payload["failure_evidence"] = step.FailureEvidence[len(step.FailureEvidence)-1]
	}
	return stateChange(envelope, state, eventType, payload)
}

func matchesFailedQC(spec *execution.StepExecutionSpec, failure *qc.Failure) bool {
	if failure == nil || spec.Operation != execution.OperationQCRendition {
		return false
	}
	for index, output := range spec.Outputs {
		if output.Kind != execution.OutputQCAssessment || output.ID != failure.AssessmentID || index >= len(spec.Inputs) {
			continue
		}
		in := spec.Inputs[index]
		id := in.ArtifactID
		if id == "" {
			id = in.OriginalAssetID
		}
		if id != failure.TargetArtifactID {
			return false
		}
		if timeline := spec.Parameters.SourceTimeline; timeline != nil {
			return failure.SourceTimelineID == timeline.ID && failure.SourceInspectionID == ""
		}
		for _, source := range spec.Parameters.SourceInputs {
			if source.OriginalAssetID == in.OriginalAssetID && source.InspectionID != "" && source.InspectionID == failure.SourceInspectionID {
				return failure.SourceTimelineID == ""
			}
		}
	}
	return false
}

func failureEvidenceID(jobID, stepID string, fencingToken uint64) string {
	digest := sha256.Sum256([]byte(jobID + "\x00" + stepID + "\x00" + strconv.FormatUint(fencingToken, 10)))
	return "EVD-WORKER-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
}

func safeFailureCode(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 96 {
		return fallback
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return fallback
		}
	}
	return value
}

func safeFailureSummary(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\\/") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") || strings.Contains(value, "@") {
		return "Worker reported a structured execution failure"
	}
	return value
}

func safeFailureLogReference(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if value == "" || len(value) > 256 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") || strings.Contains(value, "@") {
		return ""
	}
	if strings.Contains(value, "://") {
		scheme := strings.SplitN(value, "://", 2)[0]
		if scheme == "worker" || scheme == "media" || scheme == "archive" || scheme == "fixture" || scheme == "evidence" {
			return value
		}
		return ""
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return ""
	}
	return value
}

func safeProcessLogArtifact(source *execution.ProcessLogArtifact, logReference string) (*execution.ProcessLogArtifact, error) {
	if source == nil {
		return nil, nil
	}
	result := *source
	if result.Validate() != nil || result.Reference != safeFailureLogReference(logReference) {
		return nil, ErrInvalidWorkerLifecycle
	}
	lines := strings.Split(strings.ToValidUTF8(result.SanitizedTail, "�"), "\n")
	for index, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(line, "\\") || strings.Contains(line, "@") || strings.Contains(lower, "/users/") ||
			strings.Contains(lower, "/home/") || strings.Contains(lower, "/private/") || strings.Contains(lower, "/tmp/") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "bearer ") ||
			strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") || strings.Contains(lower, "secret=") ||
			strings.Contains(lower, "token=") || strings.Contains(lower, "cookie=") {
			lines[index] = "[REDACTED PROCESS OUTPUT]"
		}
	}
	result.SanitizedTail = strings.TrimSpace(strings.Join(lines, "\n"))
	if len(result.SanitizedTail) > 16<<10 {
		result.SanitizedTail = result.SanitizedTail[len(result.SanitizedTail)-(16<<10):]
		for !utf8.ValidString(result.SanitizedTail) && len(result.SanitizedTail) > 0 {
			result.SanitizedTail = result.SanitizedTail[1:]
		}
	}
	return &result, nil
}

func cloneExitCode(value *int) *int {
	if value == nil || *value < 0 {
		return nil
	}
	copy := *value
	return &copy
}

func (handler *WorkerLifecycleHandler) settleCancellation(
	envelope command.Envelope,
	snapshot command.Snapshot,
	parameters SettleWorkerCancellationParameters,
) (command.Change, error) {
	if envelope.Actor.Type != command.WorkerActor || strings.TrimSpace(envelope.Actor.ID) == "" ||
		strings.TrimSpace(parameters.StepID) == "" || strings.TrimSpace(parameters.CancellationID) == "" || parameters.FencingToken == 0 {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	state, err := decodeState(snapshot)
	if err != nil {
		return command.Change{}, err
	}
	step := state.step(parameters.StepID)
	if step == nil || (step.Workflow.State != workflow.Running && step.Workflow.State != workflow.Verifying && step.Workflow.State != workflow.Paused) ||
		step.Workflow.ExecutionLease != nil {
		return command.Change{}, workflow.ErrStaleFencingToken
	}
	if err := step.Workflow.Transition(workflow.Cancelled); err != nil {
		return command.Change{}, err
	}
	now := handler.now().UTC()
	step.LastEventAt = now
	step.LogReference = strings.TrimSpace(parameters.LogReference)
	partial := !parameters.CleanupConfirmed || len(parameters.ExternalEffectReferences) != 0
	if partial {
		step.ErrorClass = "CANCELLATION_PARTIAL"
	}
	for index := range state.Steps {
		candidate := &state.Steps[index]
		switch candidate.Workflow.State {
		case workflow.Queued, workflow.Waiting, workflow.Reserved:
			if err := candidate.Workflow.Transition(workflow.Cancelled); err != nil {
				return command.Change{}, err
			}
			candidate.Workflow.ExecutionLease = nil
			candidate.LastEventAt = now
		}
	}
	state.CurrentStepID = step.Workflow.ID
	state.Revision = snapshot.Revision + 1
	reasonType := "StepCancellationSettled"
	if partial {
		reasonType = "StepCancellationPartiallySettled"
	}
	return stateChange(envelope, state, reasonType, map[string]any{
		"step_id": step.Workflow.ID, "cancellation_id": parameters.CancellationID,
		"fencing_token": parameters.FencingToken, "cleanup_confirmed": parameters.CleanupConfirmed,
		"external_effect_references": append([]string(nil), parameters.ExternalEffectReferences...),
	})
}

func (handler *WorkerLifecycleHandler) recover(
	envelope command.Envelope,
	snapshot command.Snapshot,
	parameters RecoverWorkerStepParameters,
) (command.Change, error) {
	if envelope.Actor.Type != command.SystemActor || strings.TrimSpace(parameters.StepID) == "" {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	state, err := decodeState(snapshot)
	if err != nil {
		return command.Change{}, err
	}
	step := state.step(parameters.StepID)
	if step == nil {
		return command.Change{}, ErrInvalidWorkerLifecycle
	}
	now := handler.now().UTC()
	eventType := "StepReservationExpired"
	switch envelope.CommandType {
	case RecoverWorkerReservationCommand:
		if step.Workflow.State != workflow.Reserved || step.Workflow.Transition(workflow.Queued) != nil {
			return command.Change{}, workflow.ErrIllegalTransition
		}
	case RecoverWorkerExecutionLeaseCommand:
		if step.Workflow.State != workflow.Running || !step.Workflow.RecoverExpiredLease(now) {
			return command.Change{}, workflow.ErrIllegalTransition
		}
		eventType = "StepExecutionLeaseExpired"
	default:
		return command.Change{}, ErrUnsupportedType
	}
	step.WorkerID = ""
	step.LastEventAt = now
	state.Revision = snapshot.Revision + 1
	return stateChange(envelope, state, eventType, map[string]any{"step_id": step.Workflow.ID})
}

func workerStepState(
	envelope command.Envelope,
	snapshot command.Snapshot,
	stepID string,
	fencingToken uint64,
	requireLease bool,
) (State, *Step, error) {
	if envelope.Actor.Type != command.WorkerActor || strings.TrimSpace(envelope.Actor.ID) == "" || strings.TrimSpace(stepID) == "" || fencingToken == 0 {
		return State{}, nil, ErrInvalidWorkerLifecycle
	}
	state, err := decodeState(snapshot)
	if err != nil {
		return State{}, nil, err
	}
	step := state.step(stepID)
	if step == nil {
		return State{}, nil, workflow.ErrStaleFencingToken
	}
	if requireLease && step.Workflow.AcceptWorkerResult(envelope.Actor.ID, fencingToken) != nil {
		return State{}, nil, workflow.ErrStaleFencingToken
	}
	return state, step, nil
}

func (state *State) step(stepID string) *Step {
	for index := range state.Steps {
		if state.Steps[index].Workflow.ID == stepID {
			return &state.Steps[index]
		}
	}
	return nil
}

func workerStepOutputIDs(record *execution.StepOutputRecord) []string {
	if record == nil {
		return nil
	}
	ids := make([]string, 0, len(record.Outputs))
	for _, output := range record.Outputs {
		ids = append(ids, output.ID)
	}
	return ids
}

func materializeDependentExecutionSpecs(state *State) error {
	if state == nil || !(strings.HasPrefix(state.TemplateName, "archive-intake-") || state.TemplateName == "archive-transcription" || state.TemplateName == pipeline.ArchiveTranscriptionRecoveryName) {
		return nil
	}
	workflowSteps := make(map[string]*workflow.Step, len(state.Steps))
	completed := make([]execution.CompletedStep, 0, len(state.Steps))
	for index := range state.Steps {
		step := &state.Steps[index]
		workflowSteps[step.Workflow.ID] = &step.Workflow
		if step.Workflow.State == workflow.Completed && step.ExecutionSpec != nil && step.OutputRecord != nil {
			completed = append(completed, execution.CompletedStep{
				StepID: step.Workflow.ID, Spec: *step.ExecutionSpec, OutputRecord: *step.OutputRecord,
			})
		}
	}
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.ExecutorKind != vocabulary.ExecutorWorker || step.Workflow.State != workflow.Queued || step.ExecutionSpec != nil ||
			!step.Workflow.Ready(workflowSteps) {
			continue
		}
		spec, err := execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{
			JobID: state.ID, TemplateName: state.TemplateName, StepID: step.Workflow.ID, Completed: completed,
		})
		if err != nil {
			return err
		}
		step.ExecutionSpec = &spec
	}
	return nil
}

func activateNextReadyStep(state *State, now time.Time) {
	steps := make(map[string]*workflow.Step, len(state.Steps))
	for index := range state.Steps {
		steps[state.Steps[index].Workflow.ID] = &state.Steps[index].Workflow
	}
	for index := range state.Steps {
		step := &state.Steps[index]
		if step.Workflow.State == workflow.Queued && step.Workflow.Ready(steps) {
			if step.ReadyAt.IsZero() {
				step.ReadyAt = now
			}
			state.CurrentStepID = step.Workflow.ID
			return
		}
	}
}
