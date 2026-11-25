package workdiagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/execution"
	corejob "videoarchive/internal/core/job"
	"videoarchive/internal/core/qc"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidConfiguration = errors.New("work diagnostics configuration is invalid")
	ErrEvidenceNotFound     = errors.New("failure evidence is not part of the bound work")
)

type WorkReader interface {
	Detail(context.Context, string) (archivework.View, error)
}

type JobReader interface {
	Detail(context.Context, string) (archiveworkspace.JobView, error)
	Inbox(context.Context) ([]archiveworkspace.JobSummary, error)
}

type WorkerReader interface {
	Statuses(context.Context) ([]worker.Status, error)
}

type ExecutionReader interface {
	ActiveExecution(context.Context, string, string) (ExecutionObservation, bool, error)
}

type ExecutionObservation struct {
	ReservationID     string                   `json:"reservation_id"`
	WorkerID          string                   `json:"worker_id"`
	ResourceClass     vocabulary.ResourceClass `json:"resource_class"`
	MemoryBytes       uint64                   `json:"memory_bytes"`
	ScratchBytes      uint64                   `json:"scratch_bytes"`
	PhysicalDeviceIDs []string                 `json:"physical_device_ids,omitempty"`
	ReservedAt        time.Time                `json:"reserved_at"`
	LeaseAcquiredAt   time.Time                `json:"lease_acquired_at,omitempty"`
	LeaseHeartbeatAt  time.Time                `json:"lease_heartbeat_at,omitempty"`
	LeaseExpiresAt    time.Time                `json:"lease_expires_at,omitempty"`
	FencingToken      uint64                   `json:"fencing_token,omitempty"`
	HasLease          bool                     `json:"has_lease"`
}

type Fact struct {
	EvidenceID string    `json:"evidence_id"`
	Code       string    `json:"code"`
	Summary    string    `json:"summary"`
	ObservedAt time.Time `json:"observed_at"`
}

type Hypothesis struct {
	Code      string   `json:"code"`
	Statement string   `json:"statement"`
	Basis     []string `json:"basis"`
	NextCheck string   `json:"next_check"`
}

type SuggestedAction struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

// RecoveryAction is an executable, evidence-bound action. SuggestedAction is
// explanatory; only this shape may cross into the recovery command path.
type RecoveryAction struct {
	ActionCode            string   `json:"action_code"`
	JobID                 string   `json:"job_id"`
	JobRevision           uint64   `json:"job_revision"`
	StepID                string   `json:"step_id"`
	ReservationID         string   `json:"reservation_id"`
	FencingToken          uint64   `json:"fencing_token"`
	DiagnosticFingerprint string   `json:"diagnostic_fingerprint,omitempty"`
	Basis                 []string `json:"basis"`
	RecoveryJobID         string   `json:"recovery_job_id,omitempty"`
}

type WorkerSnapshot struct {
	WorkerID               string    `json:"worker_id"`
	State                  string    `json:"state"`
	LastHeartbeat          time.Time `json:"last_heartbeat,omitempty"`
	AdditionalWorkPossible bool      `json:"additional_work_possible"`
	AvailableMemoryBytes   uint64    `json:"available_memory_bytes"`
	ScratchFreeBytes       uint64    `json:"scratch_free_bytes"`
	ReportedHeavyRunning   int       `json:"reported_heavy_running"`
	ReportedLightRunning   int       `json:"reported_light_running"`
}

type LinkedRecoveryJob struct {
	JobID             string             `json:"job_id"`
	Revision          uint64             `json:"revision"`
	StepID            string             `json:"step_id"`
	State             workflow.StepState `json:"state"`
	FailureEvidenceID string             `json:"failure_evidence_id"`
}

type JobDiagnostic struct {
	RecoveryJob      *LinkedRecoveryJob             `json:"recovery_job,omitempty"`
	EvidenceGaps     []EvidenceGap                  `json:"evidence_gaps,omitempty"`
	JobID            string                         `json:"job_id"`
	JobRevision      uint64                         `json:"job_revision"`
	StepID           string                         `json:"step_id"`
	StepState        workflow.StepState             `json:"step_state"`
	Progress         float64                        `json:"progress"`
	WaitReason       vocabulary.WaitReason          `json:"wait_reason,omitempty"`
	ErrorClass       string                         `json:"error_class,omitempty"`
	LogReference     string                         `json:"log_reference,omitempty"`
	LastProgressAt   time.Time                      `json:"last_progress_at,omitzero"`
	Worker           *WorkerSnapshot                `json:"worker,omitempty"`
	Execution        *ExecutionObservation          `json:"execution,omitempty"`
	ProgressEvidence []corejob.StepProgressEvidence `json:"progress_evidence,omitempty"`
	FailureEvidence  []corejob.StepFailureEvidence  `json:"failure_evidence,omitempty"`
	RecoveryEvidence []corejob.StepRecoveryEvidence `json:"recovery_evidence,omitempty"`
}

type Snapshot struct {
	WorkState        archivework.State         `json:"work_state,omitempty"`
	ResultCount      *int                      `json:"result_count,omitempty"`
	WorkKind         archivistruntime.WorkKind `json:"work_kind"`
	WorkID           string                    `json:"work_id"`
	WorkRevision     uint64                    `json:"work_revision"`
	ObservedAt       time.Time                 `json:"observed_at"`
	Jobs             []JobDiagnostic           `json:"jobs"`
	Facts            []Fact                    `json:"facts"`
	Hypotheses       []Hypothesis              `json:"hypotheses"`
	SuggestedActions []SuggestedAction         `json:"suggested_actions"`
	RecoveryActions  []RecoveryAction          `json:"recovery_actions,omitempty"`
	ShouldReport     bool                      `json:"should_report"`
	Fingerprint      string                    `json:"fingerprint"`
}

type FailureDetail struct {
	CauseCode       string                       `json:"cause_code,omitempty"`
	Summary         string                       `json:"summary,omitempty"`
	WorkflowAttempt int                          `json:"workflow_attempt,omitempty"`
	WorkKind        archivistruntime.WorkKind    `json:"work_kind"`
	WorkID          string                       `json:"work_id"`
	JobID           string                       `json:"job_id"`
	StepID          string                       `json:"step_id"`
	EvidenceID      string                       `json:"evidence_id"`
	Operation       execution.Operation          `json:"operation"`
	QCFailure       *qc.Failure                  `json:"qc_failure,omitempty"`
	ProcessExitCode *int                         `json:"process_exit_code,omitempty"`
	ObservedAt      time.Time                    `json:"observed_at"`
	ProcessLog      execution.ProcessLogArtifact `json:"process_log"`
}

func SanitizeFailureDetail(source FailureDetail) FailureDetail {
	result := source
	result.QCFailure = source.QCFailure.Safe()
	result.CauseCode = safeDiagnosticCode(source.CauseCode, "")
	result.Summary = safeDiagnosticSummary(source.Summary)
	result.EvidenceID = safeEvidenceID(source.EvidenceID, source.JobID+"\x00"+source.StepID)
	result.ProcessLog.Reference = safeLogReference(source.ProcessLog.Reference)
	result.ProcessLog.SanitizedTail = safeProcessTail(source.ProcessLog.SanitizedTail)
	if source.Operation == execution.OperationTranscribeAudio {
		result.ProcessLog.SanitizedTail = ""
	}
	if result.ProcessLog.Validate() != nil {
		id := result.ProcessLog.ID
		if id == "" {
			digest := sha256.Sum256([]byte(result.EvidenceID + "\x00invalid-process-log"))
			id = "PLOG-REDACTED-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
		}
		result.ProcessLog = execution.ProcessLogArtifact{ID: id, Reference: "evidence://process-log/" + id, State: execution.ProcessLogCaptureFailed}
	}
	return result
}

type Service struct {
	works      WorkReader
	jobs       JobReader
	workers    WorkerReader
	executions ExecutionReader
	now        func() time.Time
	staleAfter time.Duration
}

func New(works WorkReader, jobs JobReader, workers WorkerReader, executions ExecutionReader, now func() time.Time, staleAfter time.Duration) (*Service, error) {
	if works == nil || jobs == nil || workers == nil || executions == nil || now == nil || staleAfter <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Service{works: works, jobs: jobs, workers: workers, executions: executions, now: now, staleAfter: staleAfter}, nil
}

func (service *Service) Diagnose(ctx context.Context, reference archivistruntime.WorkReference) (Snapshot, error) {
	reference, err := archivistruntime.NewWorkReference(reference)
	if err != nil {
		return Snapshot{}, err
	}
	now := service.now().UTC()
	statuses, err := service.workers.Statuses(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	byWorker := make(map[string]worker.Status, len(statuses))
	for _, status := range statuses {
		byWorker[status.WorkerID] = status
	}
	jobIDs := make([]string, 0)
	recoverySummaries, err := service.jobs.Inbox(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	recoveriesByFailure := make(map[string]archiveworkspace.JobSummary)
	for _, summary := range recoverySummaries {
		if summary.RecoverySource != nil && summary.RecoverySource.FailureEvidenceID != "" {
			recoveriesByFailure[summary.RecoverySource.FailureEvidenceID] = summary
		}
	}
	result := Snapshot{WorkKind: reference.Kind, WorkID: reference.WorkID, WorkRevision: reference.Revision, ObservedAt: now}
	switch reference.Kind {
	case archivistruntime.WorkJob:
		jobIDs = append(jobIDs, reference.WorkID)
	case archivistruntime.WorkArchiveRun:
		view, loadErr := service.works.Detail(ctx, reference.WorkID)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		summary := view.Summary
		result.WorkState, result.ResultCount = summary.State, &summary.ResultCount
		result.Facts = append(result.Facts, diagnosticFact("ARCHIVE_RUN_OUTCOME",
			fmt.Sprintf("ArchiveRun is %s with %d archived packages", summary.State, summary.ResultCount),
			summary.LastEventAt, reference.WorkID, strconv.FormatUint(summary.Revision, 10), string(summary.State), strconv.Itoa(summary.ResultCount)))
		for _, child := range view.Children {
			if child.Job.StepState == workflow.Completed || (child.Job.StepState == workflow.Cancelled && view.Summary.State != archivework.StateCancelled) {
				continue
			}
			jobIDs = append(jobIDs, child.Job.JobID)
		}
	}
	sort.Strings(jobIDs)
	for _, jobID := range jobIDs {
		view, loadErr := service.jobs.Detail(ctx, jobID)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		jobDiagnostic, facts, hypotheses, actions, recoveries, report, diagnosticErr := service.diagnoseJob(ctx, view, byWorker, recoveriesByFailure, now)
		if diagnosticErr != nil {
			return Snapshot{}, diagnosticErr
		}
		result.Jobs = append(result.Jobs, jobDiagnostic)
		result.Facts = append(result.Facts, facts...)
		result.Hypotheses = append(result.Hypotheses, hypotheses...)
		result.SuggestedActions = append(result.SuggestedActions, actions...)
		result.RecoveryActions = append(result.RecoveryActions, recoveries...)
		result.ShouldReport = result.ShouldReport || report
	}
	result.Fingerprint = fingerprint(result)
	for index := range result.RecoveryActions {
		result.RecoveryActions[index].DiagnosticFingerprint = result.Fingerprint
	}
	return result, nil
}

func (service *Service) FailureEvidence(ctx context.Context, reference archivistruntime.WorkReference, evidenceID string) (FailureDetail, error) {
	reference, err := archivistruntime.NewWorkReference(reference)
	if err != nil || strings.TrimSpace(evidenceID) == "" {
		return FailureDetail{}, ErrEvidenceNotFound
	}
	jobIDs := []string{reference.WorkID}
	if reference.Kind == archivistruntime.WorkArchiveRun {
		view, loadErr := service.works.Detail(ctx, reference.WorkID)
		if loadErr != nil {
			return FailureDetail{}, loadErr
		}
		jobIDs = jobIDs[:0]
		for _, child := range view.Children {
			jobIDs = append(jobIDs, child.Job.JobID)
		}
	}
	for _, jobID := range jobIDs {
		view, loadErr := service.jobs.Detail(ctx, jobID)
		if loadErr != nil {
			return FailureDetail{}, loadErr
		}
		for _, step := range view.Steps {
			for _, source := range step.FailureEvidence {
				if source.EvidenceID != evidenceID {
					continue
				}
				evidence := sanitizeFailureEvidenceWithTail(source)
				return FailureDetail{
					CauseCode: evidence.CauseCode, Summary: evidence.Summary, WorkflowAttempt: evidence.WorkflowAttempt,
					WorkKind: reference.Kind, WorkID: reference.WorkID, JobID: jobID, StepID: evidence.StepID,
					EvidenceID: evidence.EvidenceID, Operation: evidence.Operation, QCFailure: evidence.QCFailure, ProcessExitCode: evidence.ProcessExitCode,
					ObservedAt: evidence.ObservedAt, ProcessLog: *evidence.ProcessLog,
				}, nil
			}
		}
	}
	return FailureDetail{}, ErrEvidenceNotFound
}

func (service *Service) diagnoseJob(ctx context.Context, view archiveworkspace.JobView, workers map[string]worker.Status, recoveryJobs map[string]archiveworkspace.JobSummary, now time.Time) (JobDiagnostic, []Fact, []Hypothesis, []SuggestedAction, []RecoveryAction, bool, error) {
	summary := view.Summary
	item := JobDiagnostic{
		JobID: summary.JobID, JobRevision: summary.Revision, StepID: summary.CurrentStepID, StepState: summary.StepState,
		Progress: summary.Progress, WaitReason: summary.WaitReason, ErrorClass: strings.TrimSpace(summary.ErrorClass),
		LogReference: safeLogReference(summary.LogReference),
	}
	for _, step := range view.Steps {
		item.ProgressEvidence = append(item.ProgressEvidence, step.ProgressEvidence...)
		item.EvidenceGaps = append(item.EvidenceGaps, evidenceGaps(step)...)
		for _, evidence := range step.FailureEvidence {
			item.FailureEvidence = append(item.FailureEvidence, sanitizeFailureEvidence(evidence))
		}
		for _, evidence := range step.RecoveryEvidence {
			item.RecoveryEvidence = append(item.RecoveryEvidence, sanitizeRecoveryEvidence(evidence))
		}
	}
	sort.SliceStable(item.ProgressEvidence, func(i, j int) bool {
		if item.ProgressEvidence[i].ObservedAt.Equal(item.ProgressEvidence[j].ObservedAt) {
			return item.ProgressEvidence[i].ReportSequence < item.ProgressEvidence[j].ReportSequence
		}
		return item.ProgressEvidence[i].ObservedAt.Before(item.ProgressEvidence[j].ObservedAt)
	})
	sort.SliceStable(item.FailureEvidence, func(i, j int) bool {
		if item.FailureEvidence[i].ObservedAt.Equal(item.FailureEvidence[j].ObservedAt) {
			return item.FailureEvidence[i].FailureSequence < item.FailureEvidence[j].FailureSequence
		}
		return item.FailureEvidence[i].ObservedAt.Before(item.FailureEvidence[j].ObservedAt)
	})
	stateFact := diagnosticFact("STEP_STATE_OBSERVED", "Step "+summary.CurrentStepID+" is "+string(summary.StepState), summary.LastEventAt,
		summary.JobID, strconv.FormatUint(summary.Revision, 10), summary.CurrentStepID, string(summary.StepState))
	facts := []Fact{stateFact}
	for _, step := range view.Steps {
		for _, id := range step.ReusedOutputIDs {
			fact := diagnosticFact("VERIFIED_PUBLICATION_REUSED", "Reused verified published output "+id, step.LastEventAt, summary.JobID, step.ID, id, step.LastEventAt.UTC().Format(time.RFC3339Nano))
			facts = append(facts, fact)
		}
	}
	for _, evidence := range item.ProgressEvidence {
		facts = append(facts, progressFact(evidence))
	}
	for _, evidence := range item.FailureEvidence {
		facts = append(facts, Fact{
			EvidenceID: evidence.EvidenceID, Code: "WORKER_FAILURE_RECORDED",
			Summary: evidence.Summary + " (" + evidence.CauseCode + ")", ObservedAt: evidence.ObservedAt,
		})
	}
	for _, evidence := range item.RecoveryEvidence {
		facts = append(facts, Fact{
			EvidenceID: evidence.EvidenceID, Code: "WORK_RECOVERY_RECORDED",
			Summary:    "Recovery " + evidence.ActionCode + " changed Step " + evidence.StepID + " to " + string(evidence.ResultingState),
			ObservedAt: evidence.AppliedAt,
		})
	}
	if status, ok := workers[summary.WorkerID]; ok && summary.WorkerID != "" {
		item.Worker = &WorkerSnapshot{
			WorkerID: status.WorkerID, State: status.State, LastHeartbeat: status.LastHeartbeat,
			AdditionalWorkPossible: status.Capacity.AdditionalWorkPossible,
			AvailableMemoryBytes:   status.Capacity.AvailableMemoryBytes, ScratchFreeBytes: status.Capacity.ScratchFreeBytes,
			ReportedHeavyRunning: status.Metrics.HeavyRunning, ReportedLightRunning: status.Metrics.LightRunning,
		}
		facts = append(facts, diagnosticFact("WORKER_STATE_OBSERVED", "Worker "+status.WorkerID+" is "+status.State, now,
			summary.JobID, status.WorkerID, status.State, now.Format(time.RFC3339Nano)))
	}
	executionObservation, found, err := service.executions.ActiveExecution(ctx, summary.JobID, summary.CurrentStepID)
	if err != nil {
		return JobDiagnostic{}, nil, nil, nil, nil, false, err
	}
	if found {
		item.Execution = &executionObservation
		facts = append(facts, diagnosticFact("RESOURCE_RESERVED", "The current Step has an active resource reservation", executionObservation.ReservedAt,
			summary.JobID, summary.CurrentStepID, executionObservation.ReservationID))
		if executionObservation.HasLease {
			facts = append(facts, diagnosticFact("LEASE_HEARTBEAT_OBSERVED", "The Worker execution loop renewed its lease", executionObservation.LeaseHeartbeatAt,
				summary.JobID, summary.CurrentStepID, strconv.FormatUint(executionObservation.FencingToken, 10), executionObservation.LeaseHeartbeatAt.Format(time.RFC3339Nano)))
		}
	}
	// A state transition or a lease heartbeat is not a processing sample.
	for _, e := range item.ProgressEvidence {
		if e.StepID != summary.CurrentStepID || e.Update.Status.IsPhase() || (found && executionObservation.HasLease && e.FencingToken != executionObservation.FencingToken) {
			continue
		}
		item.LastProgressAt = progressFact(e).ObservedAt
	}
	var hypotheses []Hypothesis
	var actions []SuggestedAction
	var recoveries []RecoveryAction
	report := false
	if source, ok := service.executions.(interface {
		DispatchObservations(context.Context, string, uint64) ([]dispatch.Observation, error)
	}); ok {
		observations, err := source.DispatchObservations(ctx, summary.JobID, summary.Revision)
		if err != nil {
			return JobDiagnostic{}, nil, nil, nil, nil, false, err
		}
		for _, item := range observations {
			for _, b := range item.Blockers {
				text := fmt.Sprintf("Step %s admission %s, Worker %s snapshot %d", item.StepID, b.Code, b.WorkerID, b.SnapshotID)
				facts = append(facts, diagnosticFact("DISPATCH_BLOCKED", text, item.ObservedAt, summary.JobID, item.StepID, b.WorkerID, b.Code, string(b.WaitReason), item.ObservedAt.UTC().Format(time.RFC3339Nano)))
				if b.Code != "RESOURCE" {
					report = true
				}
			}
		}
	}
	active := summary.StepState == workflow.Reserved || summary.StepState == workflow.Running || summary.StepState == workflow.Verifying
	if summary.StepState == workflow.Failed || item.ErrorClass != "" {
		report = true
		facts = append(facts, diagnosticFact("REPORTED_FAILURE", "The Step reported "+item.ErrorClass, summary.LastEventAt,
			summary.JobID, summary.CurrentStepID, item.ErrorClass, summary.LastEventAt.Format(time.RFC3339Nano)))
		latest, actionCode := terminalRecovery(item, summary.TemplateName)
		if latest.QCFailure != nil {
			actions = append(actions, SuggestedAction{Code: "INSPECT_FAILURE_DETAIL", Summary: "Read the failed QC checks and compare the expected and measured values for the affected file"})
		} else if latestProcessLog(item.FailureEvidence) == execution.ProcessLogAvailable {
			actions = append(actions, SuggestedAction{Code: "INSPECT_FAILURE_DETAIL", Summary: "Read the available persisted failure evidence detail before retrying"})
		} else {
			actions = append(actions, SuggestedAction{Code: "FAILURE_DETAIL_UNAVAILABLE", Summary: "Use the structured failure fields because process detail was not recorded"})
		}
		switch latest.CauseCode {
		case "SOURCE_DECODE_FAILED":
			actions = append(actions, SuggestedAction{Code: "CHECK_SOURCE_COPY", Summary: "Inspect the affected original at the reported output position and compare an alternate source copy before retrying"})
		case "QC_CHECK_FAILED":
			actions = append(actions, SuggestedAction{Code: "REVIEW_QC_MEASUREMENTS", Summary: "Compare the failed checks with their expected values before changing the source or rendering profile"})
		case "INPUT_FILE_MISSING", "INPUT_FILE_ACCESS_DENIED":
			actions = append(actions, SuggestedAction{Code: "CHECK_REGISTERED_INPUT", Summary: "Check the referenced input on the accessible Storage before scheduling a new execution"})
		case "WHISPER_MODEL_INVALID":
			actions = append(actions, SuggestedAction{Code: "VERIFY_TRANSCRIPTION_MODEL", Summary: "Verify the configured model against its trusted checksum before scheduling transcription"})
		case "WHISPER_OUTPUT_OPEN_FAILED":
			actions = append(actions, SuggestedAction{Code: "CHECK_TRANSCRIPTION_OUTPUT_DIRECTORY", Summary: "Check the target directory for write access, conflicting files and available space"})
		}
		if actionCode == corejob.RecoveryActionRetryTranscriptionShortPath {
			if slices.ContainsFunc(view.Steps, func(s archiveworkspace.StepStatus) bool {
				return s.ID == "audio" && s.State == workflow.Completed && s.HasVerificationReferences
			}) {
				facts = append(facts, diagnosticFact("RECOVERY_AUDIO_VERIFIED", "Audio extraction has a verified output record; the recovery Worker checks its checksum before reuse", latest.ObservedAt, summary.JobID, latest.EvidenceID))
			} else {
				actionCode = ""
			}
		}
		if actionCode != "" && !found {
			if recovery, exists := recoveryJobs[latest.EvidenceID]; exists {
				item.RecoveryJob = &LinkedRecoveryJob{JobID: recovery.JobID, Revision: recovery.Revision, StepID: recovery.CurrentStepID, State: recovery.StepState, FailureEvidenceID: latest.EvidenceID}
				facts = append(facts, diagnosticFact("TERMINAL_RECOVERY_JOB_CREATED", "A replacement recovery Job preserves and follows the terminal failure", recovery.LastEventAt,
					summary.JobID, latest.EvidenceID, recovery.JobID, strconv.FormatUint(recovery.Revision, 10)))
			} else {
				recoveries = append(recoveries, RecoveryAction{
					ActionCode: actionCode,
					JobID:      summary.JobID, JobRevision: summary.Revision, StepID: summary.CurrentStepID,
					FencingToken: latest.FencingToken, Basis: []string{latest.EvidenceID},
					RecoveryJobID: corejob.TerminalRecoveryJobID(summary.JobID, latest.EvidenceID),
				})
			}
		}
	}
	if summary.StepState == workflow.Waiting {
		report = report || summary.WaitReason == vocabulary.WaitInput || summary.WaitReason == vocabulary.WaitApproval || summary.WaitReason == vocabulary.WaitDevice
		facts = append(facts, diagnosticFact("REPORTED_WAIT", "The Step is waiting for "+string(summary.WaitReason), summary.LastEventAt,
			summary.JobID, summary.CurrentStepID, string(summary.WaitReason), summary.LastEventAt.Format(time.RFC3339Nano)))
		if report {
			actions = append(actions, SuggestedAction{Code: "RESOLVE_WAIT", Summary: "Show the persisted wait target and ask for the required human action"})
		}
	}
	if active && (!found || !executionObservation.HasLease) {
		report = true
		facts = append(facts, diagnosticFact("EXECUTION_LEASE_MISSING", "The active Step has no current execution lease", now,
			summary.JobID, summary.CurrentStepID, now.Format(time.RFC3339Nano)))
		actions = append(actions, SuggestedAction{Code: "RECONCILE_EXECUTION", Summary: "Reconcile the Step lifecycle before assigning duplicate work"})
	} else if active && !now.Before(executionObservation.LeaseExpiresAt) {
		report = true
		facts = append(facts, diagnosticFact("EXECUTION_LEASE_EXPIRED", "The active Step execution lease expired", executionObservation.LeaseExpiresAt,
			summary.JobID, summary.CurrentStepID, executionObservation.LeaseExpiresAt.Format(time.RFC3339Nano)))
		actions = append(actions, SuggestedAction{Code: "RECONCILE_EXECUTION", Summary: "Confirm the Worker process state and recover the expired lease"})
		basis := facts[len(facts)-1].EvidenceID
		recoveries = append(recoveries, RecoveryAction{
			ActionCode: "RECONCILE_EXPIRED_EXECUTION", JobID: summary.JobID, JobRevision: summary.Revision,
			StepID: summary.CurrentStepID, ReservationID: executionObservation.ReservationID, FencingToken: executionObservation.FencingToken,
			Basis: []string{basis},
		})
	}
	if found && executionObservation.HasLease && item.Worker != nil && (summary.StepState == workflow.Running || summary.StepState == workflow.Verifying) &&
		((executionObservation.ResourceClass == vocabulary.ResourceHeavy && item.Worker.ReportedHeavyRunning == 0) ||
			(executionObservation.ResourceClass == vocabulary.ResourceLight && item.Worker.ReportedLightRunning == 0)) {
		report = true
		mismatchFact := diagnosticFact("RESERVATION_LOAD_MISMATCH", "Control has an active execution reservation that the latest Worker load snapshot does not report", now,
			summary.JobID, summary.CurrentStepID, summary.WorkerID, now.Format(time.RFC3339Nano))
		facts = append(facts, mismatchFact)
		hypotheses = append(hypotheses, Hypothesis{
			Code: "WORKER_LOAD_NOT_REPORTED", Statement: "The Worker snapshot may lag or the subprocess may no longer be registered as running",
			Basis:     []string{mismatchFact.EvidenceID},
			NextCheck: "Read the next Worker heartbeat and structured operation progress event",
		})
	}
	if active && item.Worker != nil && (item.Worker.State == "OFFLINE" || item.Worker.State == "DISABLED") {
		report = true
		facts = append(facts, diagnosticFact("WORKER_UNAVAILABLE", "The assigned Worker is "+item.Worker.State, now,
			summary.JobID, summary.WorkerID, item.Worker.State, now.Format(time.RFC3339Nano)))
		actions = append(actions, SuggestedAction{Code: "RESTORE_WORKER", Summary: "Restore the assigned Worker connection before changing the work"})
	}
	if active && !summary.LastEventAt.IsZero() && now.Sub(summary.LastEventAt) >= service.staleAfter {
		report = true
		hypotheses = append(hypotheses, Hypothesis{
			Code: "PROGRESS_UNOBSERVED", Statement: "The subprocess may still be running or may be stalled; current evidence cannot distinguish them",
			Basis:     []string{stateFact.EvidenceID},
			NextCheck: "Read the Worker's next structured progress sample or sanitized operation log event",
		})
		actions = append(actions, SuggestedAction{Code: "OBSERVE_PROGRESS", Summary: "Request a new structured Worker progress sample before retrying or cancelling"})
	}
	return item, facts, hypotheses, dedupeActions(actions), recoveries, report, nil
}

func terminalRecovery(item JobDiagnostic, template string) (corejob.StepFailureEvidence, string) {
	var latest corejob.StepFailureEvidence
	if item.StepState == workflow.Failed {
		for _, failure := range item.FailureEvidence {
			if failure.StepID == item.StepID && failure.FailureSequence > latest.FailureSequence {
				latest = failure
			}
		}
	}
	return latest, corejob.TerminalRecoveryAction(template, latest)
}

func sanitizeRecoveryEvidence(source corejob.StepRecoveryEvidence) corejob.StepRecoveryEvidence {
	result := source
	result.EvidenceID = safeEvidenceID(source.EvidenceID, source.StepID+"\x00"+strconv.FormatUint(source.FencingToken, 10))
	result.ActionCode = safeDiagnosticCode(source.ActionCode, "WORK_RECOVERY")
	result.DiagnosticFingerprint = strings.TrimSpace(source.DiagnosticFingerprint)
	if len(result.DiagnosticFingerprint) != 24 {
		result.DiagnosticFingerprint = ""
	}
	result.Basis = append([]string(nil), source.Basis...)
	for index := range result.Basis {
		result.Basis[index] = safeEvidenceID(result.Basis[index], result.EvidenceID+"\x00"+strconv.Itoa(index))
	}
	return result
}

func diagnosticFact(code, summary string, observedAt time.Time, identity ...string) Fact {
	source := strings.Join(append([]string{code}, identity...), "\x00")
	digest := sha256.Sum256([]byte(source))
	return Fact{EvidenceID: "EVD-DIAGNOSTIC-" + strings.ToUpper(hex.EncodeToString(digest[:8])), Code: code, Summary: summary, ObservedAt: observedAt}
}

func sanitizeFailureEvidence(source corejob.StepFailureEvidence) corejob.StepFailureEvidence {
	result := sanitizeFailureEvidenceWithTail(source)
	if result.ProcessLog != nil {
		copy := *result.ProcessLog
		copy.SanitizedTail = ""
		result.ProcessLog = &copy
	}
	return result
}

func sanitizeFailureEvidenceWithTail(source corejob.StepFailureEvidence) corejob.StepFailureEvidence {
	result := source
	result.EvidenceID = safeEvidenceID(source.EvidenceID, source.StepID+"\x00"+strconv.FormatUint(source.FencingToken, 10))
	result.QCFailure = source.QCFailure.Safe()
	result.ErrorClass = safeDiagnosticCode(source.ErrorClass, "WORKER_REPORTED_FAILURE")
	result.CauseCode = safeDiagnosticCode(source.CauseCode, result.ErrorClass)
	result.LogReference = safeLogReference(source.LogReference)
	result.Summary = safeDiagnosticSummary(source.Summary)
	if source.ProcessLog != nil && source.ProcessLog.Validate() == nil {
		copy := *source.ProcessLog
		copy.SanitizedTail = safeProcessTail(copy.SanitizedTail)
		if source.Operation == execution.OperationTranscribeAudio {
			copy.SanitizedTail = ""
		}
		result.ProcessLog = &copy
	} else {
		digest := sha256.Sum256([]byte(result.EvidenceID + "\x00legacy-process-log"))
		id := "PLOG-LEGACY-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
		result.ProcessLog = &execution.ProcessLogArtifact{
			ID: id, Reference: "evidence://process-log/" + id, State: execution.ProcessLogLegacyUnresolvable,
		}
	}
	if result.ProcessExitCode != nil {
		value := *result.ProcessExitCode
		if value < 0 {
			result.ProcessExitCode = nil
		} else {
			result.ProcessExitCode = &value
		}
	}
	return result
}

// SanitizeFailureEvidenceForDisclosure returns the bounded detail intended for
// an explicit UI disclosure. General diagnostics use the tail-free projection.
func SanitizeFailureEvidenceForDisclosure(source corejob.StepFailureEvidence) corejob.StepFailureEvidence {
	return sanitizeFailureEvidenceWithTail(source)
}

func latestProcessLog(evidence []corejob.StepFailureEvidence) execution.ProcessLogState {
	if len(evidence) == 0 || evidence[len(evidence)-1].ProcessLog == nil {
		return execution.ProcessLogLegacyUnresolvable
	}
	return evidence[len(evidence)-1].ProcessLog.State
}

func safeProcessTail(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	if len(value) > 16<<10 {
		value = value[len(value)-(16<<10):]
		for !utf8.ValidString(value) && len(value) > 0 {
			value = value[1:]
		}
	}
	lines := strings.Split(value, "\n")
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
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func safeEvidenceID(value, fallbackSeed string) string {
	value = strings.TrimSpace(value)
	valid := value != "" && len(value) <= 128
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			valid = false
			break
		}
	}
	if valid {
		return value
	}
	digest := sha256.Sum256([]byte(fallbackSeed))
	return "EVD-REDACTED-" + strings.ToUpper(hex.EncodeToString(digest[:8]))
}

func safeDiagnosticCode(value, fallback string) string {
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

func safeDiagnosticSummary(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\\/") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") || strings.Contains(value, "@") {
		return "Worker failure detail was withheld by the evidence boundary"
	}
	return value
}

func safeLogReference(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if value == "" || len(value) > 256 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(value, "@") {
		return ""
	}
	if strings.Contains(value, "://") {
		scheme := strings.SplitN(value, "://", 2)[0]
		if scheme != "worker" && scheme != "media" && scheme != "archive" && scheme != "fixture" && scheme != "evidence" {
			return ""
		}
		return value
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return ""
	}
	return value
}

// Sanitize returns the transport-safe diagnostic shape. It is intentionally
// idempotent so the application service and an HTTP boundary may both apply it.
func Sanitize(snapshot Snapshot) Snapshot {
	result := snapshot
	result.Jobs = append([]JobDiagnostic(nil), snapshot.Jobs...)
	for index := range result.Jobs {
		result.Jobs[index].LogReference = safeLogReference(result.Jobs[index].LogReference)
		if result.Jobs[index].Worker != nil {
			copy := *result.Jobs[index].Worker
			result.Jobs[index].Worker = &copy
		}
		if result.Jobs[index].Execution != nil {
			copy := *result.Jobs[index].Execution
			copy.PhysicalDeviceIDs = append([]string(nil), copy.PhysicalDeviceIDs...)
			result.Jobs[index].Execution = &copy
		}
		result.Jobs[index].EvidenceGaps = append([]EvidenceGap(nil), snapshot.Jobs[index].EvidenceGaps...)
		for i := range result.Jobs[index].EvidenceGaps {
			gap := &result.Jobs[index].EvidenceGaps[i]
			if gap.LastKnown != nil {
				fact := *gap.LastKnown
				fact.EvidenceID = safeEvidenceID(fact.EvidenceID, gap.StepID)
				fact.Code = safeDiagnosticCode(fact.Code, "WORKER_PROGRESS_RECORDED")
				fact.Summary = safeDiagnosticSummary(fact.Summary)
				gap.LastKnown = &fact
			}
		}
		result.Jobs[index].ProgressEvidence = append([]corejob.StepProgressEvidence(nil), snapshot.Jobs[index].ProgressEvidence...)
		result.Jobs[index].FailureEvidence = append([]corejob.StepFailureEvidence(nil), snapshot.Jobs[index].FailureEvidence...)
		for evidenceIndex := range result.Jobs[index].FailureEvidence {
			result.Jobs[index].FailureEvidence[evidenceIndex] = sanitizeFailureEvidence(result.Jobs[index].FailureEvidence[evidenceIndex])
		}
		result.Jobs[index].RecoveryEvidence = append([]corejob.StepRecoveryEvidence(nil), snapshot.Jobs[index].RecoveryEvidence...)
		for evidenceIndex := range result.Jobs[index].RecoveryEvidence {
			result.Jobs[index].RecoveryEvidence[evidenceIndex] = sanitizeRecoveryEvidence(result.Jobs[index].RecoveryEvidence[evidenceIndex])
		}
	}
	result.Facts = append([]Fact(nil), snapshot.Facts...)
	for index := range result.Facts {
		result.Facts[index].EvidenceID = safeEvidenceID(result.Facts[index].EvidenceID,
			result.Facts[index].Code+"\x00"+result.Facts[index].ObservedAt.UTC().Format(time.RFC3339Nano))
		result.Facts[index].Code = safeDiagnosticCode(result.Facts[index].Code, "DIAGNOSTIC_FACT")
		result.Facts[index].Summary = safeDiagnosticSummary(result.Facts[index].Summary)
	}
	result.Hypotheses = append([]Hypothesis(nil), snapshot.Hypotheses...)
	for index := range result.Hypotheses {
		result.Hypotheses[index].Code = safeDiagnosticCode(result.Hypotheses[index].Code, "DIAGNOSTIC_HYPOTHESIS")
		result.Hypotheses[index].Statement = safeDiagnosticSummary(result.Hypotheses[index].Statement)
		result.Hypotheses[index].NextCheck = safeDiagnosticSummary(result.Hypotheses[index].NextCheck)
		result.Hypotheses[index].Basis = append([]string(nil), result.Hypotheses[index].Basis...)
		for basisIndex := range result.Hypotheses[index].Basis {
			result.Hypotheses[index].Basis[basisIndex] = safeEvidenceID(result.Hypotheses[index].Basis[basisIndex],
				result.Hypotheses[index].Code+"\x00"+strconv.Itoa(basisIndex))
		}
	}
	result.SuggestedActions = append([]SuggestedAction(nil), snapshot.SuggestedActions...)
	for index := range result.SuggestedActions {
		result.SuggestedActions[index].Code = safeDiagnosticCode(result.SuggestedActions[index].Code, "DIAGNOSTIC_ACTION")
		result.SuggestedActions[index].Summary = safeDiagnosticSummary(result.SuggestedActions[index].Summary)
	}
	result.RecoveryActions = append([]RecoveryAction(nil), snapshot.RecoveryActions...)
	for index := range result.RecoveryActions {
		result.RecoveryActions[index].ActionCode = safeDiagnosticCode(result.RecoveryActions[index].ActionCode, "WORK_RECOVERY")
		result.RecoveryActions[index].Basis = append([]string(nil), result.RecoveryActions[index].Basis...)
		for basisIndex := range result.RecoveryActions[index].Basis {
			result.RecoveryActions[index].Basis[basisIndex] = safeEvidenceID(result.RecoveryActions[index].Basis[basisIndex],
				result.RecoveryActions[index].JobID+"\x00"+result.RecoveryActions[index].StepID+"\x00"+strconv.Itoa(basisIndex))
		}
	}
	return result
}

func dedupeActions(source []SuggestedAction) []SuggestedAction {
	seen := make(map[string]bool, len(source))
	result := make([]SuggestedAction, 0, len(source))
	for _, value := range source {
		if !seen[value.Code] {
			seen[value.Code] = true
			result = append(result, value)
		}
	}
	return result
}

func fingerprint(snapshot Snapshot) string {
	var source strings.Builder
	source.WriteString(string(snapshot.WorkKind))
	source.WriteByte(0)
	source.WriteString(snapshot.WorkID)
	for _, item := range snapshot.Jobs {
		source.WriteByte(0)
		source.WriteString(item.JobID + "/revision=" + strconv.FormatUint(item.JobRevision, 10) + "/" + item.StepID + "/" + string(item.StepState) + "/" + string(item.WaitReason) + "/" + item.ErrorClass)
		source.WriteString("/progress=" + strconv.FormatFloat(item.Progress, 'f', 6, 64) + "/at=" + item.LastProgressAt.UTC().Format(time.RFC3339Nano))
		if item.Worker != nil {
			source.WriteString("/" + item.Worker.State)
		}
		if item.RecoveryJob != nil {
			source.WriteString("/replacement=" + item.RecoveryJob.JobID + ":" + strconv.FormatUint(item.RecoveryJob.Revision, 10))
		}
		if item.Execution != nil {
			source.WriteString("/lease=" + boolString(item.Execution.HasLease))
		}
		for _, evidence := range item.ProgressEvidence {
			source.WriteString("/operation-progress=" + evidence.EvidenceID + ":" + string(evidence.Update.Status))
		}
		for _, evidence := range item.FailureEvidence {
			source.WriteString("/failure=" + evidence.EvidenceID + ":" + evidence.CauseCode)
		}
		for _, evidence := range item.RecoveryEvidence {
			source.WriteString("/recovery=" + evidence.EvidenceID + ":" + evidence.ActionCode)
		}
	}
	for _, value := range snapshot.Facts {
		source.WriteByte(0)
		source.WriteString(value.Code)
		if value.Code == "DISPATCH_BLOCKED" || value.Code == "ARCHIVE_RUN_OUTCOME" {
			source.WriteString("/" + value.EvidenceID)
		}
	}
	for _, value := range snapshot.Hypotheses {
		source.WriteByte(0)
		source.WriteString(value.Code)
	}
	digest := sha256.Sum256([]byte(source.String()))
	return hex.EncodeToString(digest[:12])
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
