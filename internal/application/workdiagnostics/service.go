package workdiagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/application/archivework"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidConfiguration = errors.New("work diagnostics configuration is invalid")
)

type WorkReader interface {
	Detail(context.Context, string) (archivework.View, error)
}

type JobReader interface {
	Detail(context.Context, string) (archiveworkspace.JobView, error)
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

type JobDiagnostic struct {
	JobID          string                `json:"job_id"`
	JobRevision    uint64                `json:"job_revision"`
	StepID         string                `json:"step_id"`
	StepState      workflow.StepState    `json:"step_state"`
	Progress       float64               `json:"progress"`
	WaitReason     vocabulary.WaitReason `json:"wait_reason,omitempty"`
	ErrorClass     string                `json:"error_class,omitempty"`
	LogReference   string                `json:"log_reference,omitempty"`
	LastProgressAt time.Time             `json:"last_progress_at"`
	Worker         *WorkerSnapshot       `json:"worker,omitempty"`
	Execution      *ExecutionObservation `json:"execution,omitempty"`
}

type Snapshot struct {
	WorkKind         archivistruntime.WorkKind `json:"work_kind"`
	WorkID           string                    `json:"work_id"`
	WorkRevision     uint64                    `json:"work_revision"`
	ObservedAt       time.Time                 `json:"observed_at"`
	Jobs             []JobDiagnostic           `json:"jobs"`
	Facts            []Fact                    `json:"facts"`
	Hypotheses       []Hypothesis              `json:"hypotheses"`
	SuggestedActions []SuggestedAction         `json:"suggested_actions"`
	ShouldReport     bool                      `json:"should_report"`
	Fingerprint      string                    `json:"fingerprint"`
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
	switch reference.Kind {
	case archivistruntime.WorkJob:
		jobIDs = append(jobIDs, reference.WorkID)
	case archivistruntime.WorkArchiveRun:
		view, loadErr := service.works.Detail(ctx, reference.WorkID)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		for _, child := range view.Children {
			if child.Job.StepState == workflow.Completed || child.Job.StepState == workflow.Cancelled {
				continue
			}
			jobIDs = append(jobIDs, child.Job.JobID)
		}
	}
	sort.Strings(jobIDs)
	result := Snapshot{WorkKind: reference.Kind, WorkID: reference.WorkID, WorkRevision: reference.Revision, ObservedAt: now}
	for _, jobID := range jobIDs {
		view, loadErr := service.jobs.Detail(ctx, jobID)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		jobDiagnostic, facts, hypotheses, actions, report, diagnosticErr := service.diagnoseJob(ctx, view, byWorker, now)
		if diagnosticErr != nil {
			return Snapshot{}, diagnosticErr
		}
		result.Jobs = append(result.Jobs, jobDiagnostic)
		result.Facts = append(result.Facts, facts...)
		result.Hypotheses = append(result.Hypotheses, hypotheses...)
		result.SuggestedActions = append(result.SuggestedActions, actions...)
		result.ShouldReport = result.ShouldReport || report
	}
	result.Fingerprint = fingerprint(result)
	return result, nil
}

func (service *Service) diagnoseJob(ctx context.Context, view archiveworkspace.JobView, workers map[string]worker.Status, now time.Time) (JobDiagnostic, []Fact, []Hypothesis, []SuggestedAction, bool, error) {
	summary := view.Summary
	item := JobDiagnostic{
		JobID: summary.JobID, JobRevision: summary.Revision, StepID: summary.CurrentStepID, StepState: summary.StepState,
		Progress: summary.Progress, WaitReason: summary.WaitReason, ErrorClass: strings.TrimSpace(summary.ErrorClass),
		LogReference: safeLogReference(summary.LogReference), LastProgressAt: summary.LastEventAt,
	}
	facts := []Fact{{Code: "STEP_STATE_OBSERVED", Summary: "Step " + summary.CurrentStepID + " is " + string(summary.StepState), ObservedAt: summary.LastEventAt}}
	if status, ok := workers[summary.WorkerID]; ok && summary.WorkerID != "" {
		item.Worker = &WorkerSnapshot{
			WorkerID: status.WorkerID, State: status.State, LastHeartbeat: status.LastHeartbeat,
			AdditionalWorkPossible: status.Capacity.AdditionalWorkPossible,
			AvailableMemoryBytes:   status.Capacity.AvailableMemoryBytes, ScratchFreeBytes: status.Capacity.ScratchFreeBytes,
			ReportedHeavyRunning: status.Metrics.HeavyRunning, ReportedLightRunning: status.Metrics.LightRunning,
		}
		facts = append(facts, Fact{Code: "WORKER_STATE_OBSERVED", Summary: "Worker " + status.WorkerID + " is " + status.State, ObservedAt: now})
	}
	execution, found, err := service.executions.ActiveExecution(ctx, summary.JobID, summary.CurrentStepID)
	if err != nil {
		return JobDiagnostic{}, nil, nil, nil, false, err
	}
	if found {
		item.Execution = &execution
		facts = append(facts, Fact{Code: "RESOURCE_RESERVED", Summary: "The current Step has an active resource reservation", ObservedAt: execution.ReservedAt})
		if execution.HasLease {
			facts = append(facts, Fact{Code: "LEASE_HEARTBEAT_OBSERVED", Summary: "The Worker execution loop renewed its lease", ObservedAt: execution.LeaseHeartbeatAt})
		}
	}
	var hypotheses []Hypothesis
	var actions []SuggestedAction
	report := false
	active := summary.StepState == workflow.Reserved || summary.StepState == workflow.Running || summary.StepState == workflow.Verifying
	if summary.StepState == workflow.Failed || item.ErrorClass != "" {
		report = true
		facts = append(facts, Fact{Code: "REPORTED_FAILURE", Summary: "The Step reported " + item.ErrorClass, ObservedAt: summary.LastEventAt})
		actions = append(actions, SuggestedAction{Code: "INSPECT_FAILURE", Summary: "Review the structured error and safe log reference before retrying"})
	}
	if summary.StepState == workflow.Waiting {
		report = summary.WaitReason == vocabulary.WaitInput || summary.WaitReason == vocabulary.WaitApproval || summary.WaitReason == vocabulary.WaitDevice
		facts = append(facts, Fact{Code: "REPORTED_WAIT", Summary: "The Step is waiting for " + string(summary.WaitReason), ObservedAt: summary.LastEventAt})
		if report {
			actions = append(actions, SuggestedAction{Code: "RESOLVE_WAIT", Summary: "Show the persisted wait target and ask for the required human action"})
		}
	}
	if active && (!found || !execution.HasLease) {
		report = true
		facts = append(facts, Fact{Code: "EXECUTION_LEASE_MISSING", Summary: "The active Step has no current execution lease", ObservedAt: now})
		actions = append(actions, SuggestedAction{Code: "RECONCILE_EXECUTION", Summary: "Reconcile the Step lifecycle before assigning duplicate work"})
	} else if active && !now.Before(execution.LeaseExpiresAt) {
		report = true
		facts = append(facts, Fact{Code: "EXECUTION_LEASE_EXPIRED", Summary: "The active Step execution lease expired", ObservedAt: execution.LeaseExpiresAt})
		actions = append(actions, SuggestedAction{Code: "RECONCILE_EXECUTION", Summary: "Confirm the Worker process state and recover the expired lease"})
	}
	if found && execution.HasLease && item.Worker != nil && (summary.StepState == workflow.Running || summary.StepState == workflow.Verifying) &&
		((execution.ResourceClass == vocabulary.ResourceHeavy && item.Worker.ReportedHeavyRunning == 0) ||
			(execution.ResourceClass == vocabulary.ResourceLight && item.Worker.ReportedLightRunning == 0)) {
		report = true
		facts = append(facts, Fact{Code: "RESERVATION_LOAD_MISMATCH", Summary: "Control has an active execution reservation that the latest Worker load snapshot does not report", ObservedAt: now})
		hypotheses = append(hypotheses, Hypothesis{
			Code: "WORKER_LOAD_NOT_REPORTED", Statement: "The Worker snapshot may lag or the subprocess may no longer be registered as running",
			Basis:     []string{"An active reservation and lease exist while the matching Worker running count is zero"},
			NextCheck: "Read the next Worker heartbeat and structured operation progress event",
		})
	}
	if active && item.Worker != nil && (item.Worker.State == "OFFLINE" || item.Worker.State == "DISABLED") {
		report = true
		facts = append(facts, Fact{Code: "WORKER_UNAVAILABLE", Summary: "The assigned Worker is " + item.Worker.State, ObservedAt: now})
		actions = append(actions, SuggestedAction{Code: "RESTORE_WORKER", Summary: "Restore the assigned Worker connection before changing the work"})
	}
	if active && !summary.LastEventAt.IsZero() && now.Sub(summary.LastEventAt) >= service.staleAfter {
		report = true
		hypotheses = append(hypotheses, Hypothesis{
			Code: "PROGRESS_UNOBSERVED", Statement: "The subprocess may still be running or may be stalled; current evidence cannot distinguish them",
			Basis:     []string{"No new persisted Step progress since " + summary.LastEventAt.UTC().Format(time.RFC3339)},
			NextCheck: "Read the Worker's next structured progress sample or sanitized operation log event",
		})
		actions = append(actions, SuggestedAction{Code: "OBSERVE_PROGRESS", Summary: "Request a new structured Worker progress sample before retrying or cancelling"})
	}
	return item, facts, hypotheses, dedupeActions(actions), report, nil
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
		if scheme != "worker" && scheme != "media" && scheme != "archive" && scheme != "fixture" {
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
	}
	result.Facts = append([]Fact(nil), snapshot.Facts...)
	result.Hypotheses = append([]Hypothesis(nil), snapshot.Hypotheses...)
	for index := range result.Hypotheses {
		result.Hypotheses[index].Basis = append([]string(nil), result.Hypotheses[index].Basis...)
	}
	result.SuggestedActions = append([]SuggestedAction(nil), snapshot.SuggestedActions...)
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
		if item.Execution != nil {
			source.WriteString("/lease=" + boolString(item.Execution.HasLease))
		}
	}
	for _, value := range snapshot.Facts {
		source.WriteByte(0)
		source.WriteString(value.Code)
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
