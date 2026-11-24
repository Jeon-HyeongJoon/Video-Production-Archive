package archivistruntime

import (
	"errors"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidSession         = errors.New("invalid archivist session")
	ErrPinnedHostUnavailable  = errors.New("PINNED_HOST_UNAVAILABLE")
	ErrInvalidTurn            = errors.New("invalid archivist turn")
	ErrTurnLeaseHeld          = errors.New("archivist turn lease is already held")
	ErrStaleTurnLease         = errors.New("STALE_ARCHIVIST_TURN_LEASE")
	ErrTurnRequestMismatch    = errors.New("archivist turn request does not match")
	ErrInvalidTurnTransition  = errors.New("invalid archivist turn transition")
	ErrRuntimeBindingMismatch = errors.New("runtime binding does not match durable session")
	ErrInvalidResumePlan      = errors.New("invalid runtime resume plan")
	ErrUnsafeRuntimeExecution = errors.New("UNSAFE_RUNTIME_EXECUTION")
	ErrInvalidRuntimeEvent    = errors.New("invalid runtime event")
)

type SessionSpec struct {
	ID              string
	Name            string
	ArchivistHostID string
	Backend         vocabulary.RuntimeBackend
	CLIID           string
	WorkspaceRootID string
}

type Session struct {
	ID              string
	Name            string
	ArchivistHostID string
	Backend         vocabulary.RuntimeBackend
	CLIID           string
	WorkspaceRootID string
}

func NewSession(spec SessionSpec) (Session, error) {
	session := Session{
		ID: strings.TrimSpace(spec.ID), Name: strings.TrimSpace(spec.Name), ArchivistHostID: strings.TrimSpace(spec.ArchivistHostID), Backend: spec.Backend,
		CLIID: strings.TrimSpace(spec.CLIID), WorkspaceRootID: strings.TrimSpace(spec.WorkspaceRootID),
	}
	if session.Name == "" {
		session.Name = session.ID
	}
	if session.ID == "" || session.ArchivistHostID == "" || !session.Backend.Valid() ||
		session.CLIID == "" || session.WorkspaceRootID == "" {
		return Session{}, ErrInvalidSession
	}
	return session, nil
}

func SelectExecutionHost(session Session, states map[string]vocabulary.ArchivistHostState) (string, error) {
	if _, err := NewSession(SessionSpec(session)); err != nil {
		return "", err
	}
	if states[session.ArchivistHostID] != vocabulary.ArchivistHostOnline {
		return "", ErrPinnedHostUnavailable
	}
	return session.ArchivistHostID, nil
}

type TurnLease struct {
	ArchivistHostID string
	FencingToken    uint64
	ExpiresAt       time.Time
}

type Turn struct {
	ID                 string
	SessionID          string
	RequestID          string
	State              vocabulary.ArchivistTurnState
	Lease              *TurnLease
	LastFencingToken   uint64
	DeliveredRequestID string
	LastRuntimeCursor  uint64
	FinalResponse      *FinalResponse
	DriveSelection     *DriveSelection
	WorkReference      *WorkReference
	WorkContext        *ArchiveRunWorkContext
	SubmittedBy        vocabulary.ActorType
	ToolPolicy         ArchivistTurnToolPolicy
}

type ArchivistTurnToolPolicy string

const (
	ToolPolicyDiagnostic  ArchivistTurnToolPolicy = "DIAGNOSTIC"
	ToolPolicyStandard    ArchivistTurnToolPolicy = "STANDARD"
	ToolPolicyAdvisory    ArchivistTurnToolPolicy = "ADVISORY"
	ToolPolicyRemediation ArchivistTurnToolPolicy = "REMEDIATION"
)

func (policy ArchivistTurnToolPolicy) Effective() ArchivistTurnToolPolicy {
	if policy == "" {
		return ToolPolicyStandard
	}
	return policy
}

func (policy ArchivistTurnToolPolicy) Valid() bool {
	return policy == ToolPolicyDiagnostic || policy == ToolPolicyStandard || policy == ToolPolicyAdvisory || policy == ToolPolicyRemediation
}

type WorkKind string

const (
	WorkJob        WorkKind = "JOB"
	WorkArchiveRun WorkKind = "ARCHIVE_RUN"
)

type WorkReference struct {
	Kind     WorkKind `json:"kind"`
	WorkID   string   `json:"work_id"`
	Revision uint64   `json:"revision"`
}

func NewWorkReference(source WorkReference) (WorkReference, error) {
	reference := source
	reference.WorkID = strings.TrimSpace(source.WorkID)
	if (reference.Kind != WorkJob && reference.Kind != WorkArchiveRun) || reference.WorkID == "" || reference.Revision == 0 {
		return WorkReference{}, ErrInvalidTurn
	}
	return reference, nil
}

func AttachWorkReference(turn Turn, work WorkReference) (Turn, error) {
	normalized, err := NewWorkReference(work)
	if err != nil || turn.ID == "" {
		return turn, ErrInvalidTurn
	}
	if turn.WorkReference != nil && *turn.WorkReference != normalized {
		return turn, ErrInvalidTurn
	}
	turn.WorkReference = &normalized
	return turn, nil
}

type ArchiveRunWorkTrigger string

const (
	WorkArchiveRunStarted  ArchiveRunWorkTrigger = "ARCHIVE_RUN_STARTED"
	WorkStageOneComplete   ArchiveRunWorkTrigger = "STAGE_ONE_COMPLETE"
	WorkNewHold            ArchiveRunWorkTrigger = "NEW_HOLD"
	WorkArchiveRunComplete ArchiveRunWorkTrigger = "ARCHIVE_RUN_COMPLETE"
)

type ArchiveRunWorkContext struct {
	WorkID       string                `json:"work_id"`
	OriginTurnID string                `json:"origin_turn_id"`
	Trigger      ArchiveRunWorkTrigger `json:"trigger"`
}

func NewArchiveRunWorkContext(workID, originTurnID string, trigger ArchiveRunWorkTrigger) (ArchiveRunWorkContext, error) {
	context := ArchiveRunWorkContext{
		WorkID: strings.TrimSpace(workID), OriginTurnID: strings.TrimSpace(originTurnID), Trigger: trigger,
	}
	if context.WorkID == "" || context.OriginTurnID == "" || !context.Trigger.Valid() {
		return ArchiveRunWorkContext{}, ErrInvalidTurn
	}
	return context, nil
}

func (trigger ArchiveRunWorkTrigger) Valid() bool {
	return trigger == WorkArchiveRunStarted || trigger == WorkStageOneComplete || trigger == WorkNewHold || trigger == WorkArchiveRunComplete
}

func AttachArchiveRunWorkContext(turn Turn, work ArchiveRunWorkContext) (Turn, error) {
	normalized, err := NewArchiveRunWorkContext(work.WorkID, work.OriginTurnID, work.Trigger)
	if err != nil || turn.ID == "" || (turn.ID != normalized.OriginTurnID && turn.SubmittedBy != vocabulary.ActorSystem) {
		return turn, ErrInvalidTurn
	}
	if turn.WorkContext != nil && *turn.WorkContext != normalized {
		return turn, ErrInvalidTurn
	}
	turn.WorkContext = &normalized
	return turn, nil
}

type DriveSelection struct {
	ArchiveRunID            string               `json:"archive_run_id,omitempty"`
	ArchiveRunRevision      uint64               `json:"archive_run_revision,omitempty"`
	WorkerID                string               `json:"worker_id"`
	WorkspaceDriveID        string               `json:"workspace_drive_id"`
	IntakeSessionID         string               `json:"intake_session_id"`
	SourceVolumeID          string               `json:"source_volume_id"`
	SourceStorageID         string               `json:"source_storage_id"`
	ArchiveStorageID        string               `json:"archive_storage_id"`
	CandidateRevision       uint64               `json:"candidate_revision"`
	FolderDiscoveryRevision uint64               `json:"folder_discovery_revision"`
	FolderIDs               []string             `json:"folder_ids"`
	SelectedByType          vocabulary.ActorType `json:"selected_by_type,omitempty"`
	SelectedBy              string               `json:"selected_by"`
}

func NewDriveSelection(source DriveSelection) (DriveSelection, error) {
	selection := source
	selection.ArchiveRunID = strings.TrimSpace(source.ArchiveRunID)
	selection.WorkerID = strings.TrimSpace(source.WorkerID)
	selection.WorkspaceDriveID = strings.TrimSpace(source.WorkspaceDriveID)
	selection.IntakeSessionID = strings.TrimSpace(source.IntakeSessionID)
	selection.SourceVolumeID = strings.TrimSpace(source.SourceVolumeID)
	selection.SourceStorageID = strings.TrimSpace(source.SourceStorageID)
	selection.ArchiveStorageID = strings.TrimSpace(source.ArchiveStorageID)
	selection.SelectedBy = strings.TrimSpace(source.SelectedBy)
	if selection.SelectedByType == "" {
		// Existing durable Turn payloads predate explicit selection actor type
		// and were created only by a human operator.
		selection.SelectedByType = vocabulary.ActorHuman
	}
	selection.FolderIDs = append([]string(nil), source.FolderIDs...)
	seen := make(map[string]bool, len(selection.FolderIDs))
	for index, raw := range selection.FolderIDs {
		normalized := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
		clean := path.Clean(normalized)
		if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
			strings.HasPrefix(clean, "//") || clean != normalized || seen[clean] {
			return DriveSelection{}, ErrInvalidTurn
		}
		seen[clean] = true
		selection.FolderIDs[index] = clean
	}
	sort.Strings(selection.FolderIDs)
	if selection.WorkerID == "" || selection.WorkspaceDriveID == "" || selection.IntakeSessionID == "" || selection.SourceVolumeID == "" ||
		selection.SourceStorageID == "" || selection.ArchiveStorageID == "" ||
		selection.SourceStorageID == selection.ArchiveStorageID || selection.CandidateRevision == 0 ||
		selection.FolderDiscoveryRevision == 0 || len(selection.FolderIDs) == 0 ||
		(selection.SelectedByType != vocabulary.ActorHuman && selection.SelectedByType != vocabulary.ActorArchivist) || selection.SelectedBy == "" {
		return DriveSelection{}, ErrInvalidTurn
	}
	if (selection.ArchiveRunID == "") != (selection.ArchiveRunRevision == 0) {
		return DriveSelection{}, ErrInvalidTurn
	}
	return selection, nil
}

func NewTurn(id, sessionID, requestID string) (Turn, error) {
	return NewTurnWithReferences(id, sessionID, requestID, nil, nil)
}

func NewTurnWithDriveSelection(id, sessionID, requestID string, source *DriveSelection) (Turn, error) {
	return NewTurnWithReferences(id, sessionID, requestID, source, nil)
}

func NewTurnWithReferences(id, sessionID, requestID string, source *DriveSelection, work *WorkReference) (Turn, error) {
	turn := Turn{
		ID: strings.TrimSpace(id), SessionID: strings.TrimSpace(sessionID), RequestID: strings.TrimSpace(requestID),
		State: vocabulary.ArchivistTurnQueued, SubmittedBy: vocabulary.ActorHuman, ToolPolicy: ToolPolicyStandard,
	}
	if turn.ID == "" || turn.SessionID == "" || turn.RequestID == "" {
		return Turn{}, ErrInvalidTurn
	}
	if source != nil {
		selection, err := NewDriveSelection(*source)
		if err != nil {
			return Turn{}, err
		}
		turn.DriveSelection = &selection
	}
	if work != nil {
		updated, err := AttachWorkReference(turn, *work)
		if err != nil {
			return Turn{}, err
		}
		turn = updated
	}
	return turn, nil
}

func NewSystemTurnWithDriveSelection(id, sessionID, requestID string, source DriveSelection) (Turn, error) {
	return NewSystemTurnWithReferences(id, sessionID, requestID, source, nil)
}

func NewSystemTurnWithReferences(id, sessionID, requestID string, source DriveSelection, work *WorkReference) (Turn, error) {
	turn, err := NewTurnWithReferences(id, sessionID, requestID, &source, work)
	if err != nil {
		return Turn{}, err
	}
	turn.SubmittedBy = vocabulary.ActorSystem
	return turn, nil
}

func NewAdvisorySystemTurn(id, sessionID, requestID string, work *WorkReference) (Turn, error) {
	turn, err := NewTurnWithReferences(id, sessionID, requestID, nil, work)
	if err != nil {
		return Turn{}, err
	}
	turn.SubmittedBy = vocabulary.ActorSystem
	turn.ToolPolicy = ToolPolicyAdvisory
	return turn, nil
}

// NewRemediationSystemTurn creates a SYSTEM Turn that may inspect its fixed
// work and apply only the evidence-bound recovery capability. It deliberately
// remains distinct from both ordinary user authority and new-drive advice.
func NewRemediationSystemTurn(id, sessionID, requestID string, work *WorkReference) (Turn, error) {
	turn, err := NewTurnWithReferences(id, sessionID, requestID, nil, work)
	if err != nil {
		return Turn{}, err
	}
	turn.SubmittedBy = vocabulary.ActorSystem
	turn.ToolPolicy = ToolPolicyRemediation
	return turn, nil
}

func ClaimTurn(turn Turn, archivistHostID string, now time.Time, ttl time.Duration) (Turn, TurnLease, error) {
	archivistHostID = strings.TrimSpace(archivistHostID)
	if turn.ID == "" || !turn.State.Valid() || archivistHostID == "" || now.IsZero() || ttl <= 0 || terminalTurnState(turn.State) {
		return turn, TurnLease{}, ErrInvalidTurn
	}
	if turn.Lease != nil && now.Before(turn.Lease.ExpiresAt) {
		return turn, TurnLease{}, ErrTurnLeaseHeld
	}
	turn.LastFencingToken++
	lease := TurnLease{ArchivistHostID: archivistHostID, FencingToken: turn.LastFencingToken, ExpiresAt: now.Add(ttl)}
	turn.Lease = &lease
	turn.State = vocabulary.ArchivistTurnLeased
	return turn, lease, nil
}

func ValidateTurnReport(turn Turn, archivistHostID string, fencingToken uint64, now time.Time) error {
	if turn.Lease == nil || archivistHostID == "" || fencingToken == 0 || now.IsZero() ||
		turn.Lease.ArchivistHostID != archivistHostID || turn.Lease.FencingToken != fencingToken ||
		!now.Before(turn.Lease.ExpiresAt) {
		return ErrStaleTurnLease
	}
	return nil
}

func RenewTurnLease(turn Turn, archivistHostID string, fencingToken uint64, now time.Time, ttl time.Duration) (Turn, error) {
	if ttl <= 0 {
		return turn, ErrInvalidTurn
	}
	if err := ValidateTurnReport(turn, archivistHostID, fencingToken, now); err != nil {
		return turn, err
	}
	turn.Lease.ExpiresAt = now.Add(ttl)
	return turn, nil
}

func PrepareTurnDelivery(turn Turn, archivistHostID string, fencingToken uint64, now time.Time, requestID string) (Turn, bool, error) {
	if err := ValidateTurnReport(turn, archivistHostID, fencingToken, now); err != nil {
		return turn, false, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || requestID != turn.RequestID {
		return turn, false, ErrTurnRequestMismatch
	}
	if turn.DeliveredRequestID == requestID {
		return turn, false, nil
	}
	if turn.DeliveredRequestID != "" {
		return turn, false, ErrTurnRequestMismatch
	}
	turn.DeliveredRequestID = requestID
	return turn, true, nil
}

type RuntimeEventKind string

const (
	RuntimeInputAccepted RuntimeEventKind = "INPUT_ACCEPTED"
	RuntimeTurnStarted   RuntimeEventKind = "TURN_STARTED"
	RuntimeTurnProgress  RuntimeEventKind = "TURN_PROGRESS"
	RuntimeTurnWaiting   RuntimeEventKind = "TURN_WAITING"
	RuntimeTerminalIdle  RuntimeEventKind = "TERMINAL_IDLE"
	RuntimeTurnCompleted RuntimeEventKind = "TURN_COMPLETED"
	RuntimeTurnFailed    RuntimeEventKind = "TURN_FAILED"
)

type RuntimeEvent struct {
	Cursor        uint64
	RequestID     string
	Kind          RuntimeEventKind
	FinalResponse *FinalResponse
	Progress      *ProgressUpdate
	Failure       *FailureReport
}

type FinalResponse struct {
	Summary   string   `json:"summary"`
	ResultIDs []string `json:"result_ids"`
}

type ProgressUpdate struct {
	Summary string `json:"summary"`
}

type FailureReport struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

// RuntimeExecutionFailure is a sanitized failure emitted by an allowlisted
// local CLI. It carries no stderr, command text, credential, or path.
type RuntimeExecutionFailure struct {
	Report FailureReport
}

func (failure *RuntimeExecutionFailure) Error() string {
	if failure == nil {
		return "runtime execution failed"
	}
	return failure.Report.Code
}

func NewRuntimeExecutionFailure(code, summary string) (*RuntimeExecutionFailure, error) {
	report, err := NewFailureReport(code, summary)
	if err != nil {
		return nil, err
	}
	return &RuntimeExecutionFailure{Report: report}, nil
}

type TurnEntryKind string

const (
	TurnEntryProgress TurnEntryKind = "PROGRESS"
	TurnEntryFailure  TurnEntryKind = "FAILURE"
)

type TurnEntry struct {
	Cursor    uint64        `json:"cursor"`
	Kind      TurnEntryKind `json:"kind"`
	Summary   string        `json:"summary"`
	Code      string        `json:"code,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

func NewFinalResponse(summary string, resultIDs []string) (FinalResponse, error) {
	normalized, err := normalizeFinalResponse(&FinalResponse{Summary: summary, ResultIDs: resultIDs})
	if err != nil {
		return FinalResponse{}, err
	}
	return *normalized, nil
}

func NewProgressUpdate(summary string) (ProgressUpdate, error) {
	normalized, err := normalizeEntrySummary(summary)
	if err != nil {
		return ProgressUpdate{}, err
	}
	return ProgressUpdate{Summary: normalized}, nil
}

func NewFailureReport(code, summary string) (FailureReport, error) {
	report := FailureReport{Code: strings.TrimSpace(code)}
	var err error
	report.Summary, err = normalizeEntrySummary(summary)
	if err != nil || !validFailureCode(report.Code) {
		return FailureReport{}, ErrInvalidRuntimeEvent
	}
	return report, nil
}

func ReduceRuntimeEvent(turn Turn, archivistHostID string, fencingToken uint64, now time.Time, event RuntimeEvent) (Turn, bool, error) {
	if err := ValidateTurnReport(turn, archivistHostID, fencingToken, now); err != nil {
		return turn, false, err
	}
	if event.Cursor == 0 || event.RequestID == "" || event.RequestID != turn.RequestID ||
		turn.DeliveredRequestID != turn.RequestID {
		return turn, false, ErrInvalidRuntimeEvent
	}
	response, responseErr := normalizeFinalResponse(event.FinalResponse)
	progress, progressErr := normalizeProgressUpdate(event.Progress)
	failure, failureErr := normalizeFailureReport(event.Failure)
	switch event.Kind {
	case RuntimeTurnCompleted:
		if responseErr != nil || event.Progress != nil || event.Failure != nil {
			return turn, false, ErrInvalidRuntimeEvent
		}
	case RuntimeTurnProgress:
		if progressErr != nil || event.FinalResponse != nil || event.Failure != nil {
			return turn, false, ErrInvalidRuntimeEvent
		}
	case RuntimeTurnFailed:
		if failureErr != nil || event.FinalResponse != nil || event.Progress != nil {
			return turn, false, ErrInvalidRuntimeEvent
		}
	default:
		if event.FinalResponse != nil || event.Progress != nil || event.Failure != nil {
			return turn, false, ErrInvalidRuntimeEvent
		}
	}
	if event.Cursor <= turn.LastRuntimeCursor {
		return turn, false, nil
	}
	nextState, err := runtimeEventState(turn.State, event.Kind)
	if errors.Is(err, ErrInvalidTurnTransition) && turn.State == vocabulary.ArchivistTurnLeased &&
		event.Kind == RuntimeTurnStarted && turn.DeliveredRequestID == turn.RequestID {
		nextState, err = vocabulary.ArchivistTurnRunning, nil
	}
	if err != nil {
		return turn, false, err
	}
	turn.LastRuntimeCursor = event.Cursor
	turn.State = nextState
	if event.Kind == RuntimeTurnCompleted {
		turn.FinalResponse = response
	}
	_ = progress
	_ = failure
	return turn, true, nil
}

func TurnEntryFromRuntimeEvent(event RuntimeEvent, createdAt time.Time) (TurnEntry, bool, error) {
	if createdAt.IsZero() || event.Cursor == 0 {
		return TurnEntry{}, false, ErrInvalidRuntimeEvent
	}
	switch event.Kind {
	case RuntimeTurnProgress:
		progress, err := normalizeProgressUpdate(event.Progress)
		if err != nil {
			return TurnEntry{}, false, ErrInvalidRuntimeEvent
		}
		return TurnEntry{Cursor: event.Cursor, Kind: TurnEntryProgress, Summary: progress.Summary, CreatedAt: createdAt.UTC()}, true, nil
	case RuntimeTurnFailed:
		failure, err := normalizeFailureReport(event.Failure)
		if err != nil {
			return TurnEntry{}, false, ErrInvalidRuntimeEvent
		}
		return TurnEntry{Cursor: event.Cursor, Kind: TurnEntryFailure, Code: failure.Code, Summary: failure.Summary, CreatedAt: createdAt.UTC()}, true, nil
	default:
		return TurnEntry{}, false, nil
	}
}

func normalizeFinalResponse(source *FinalResponse) (*FinalResponse, error) {
	if source == nil {
		return nil, ErrInvalidRuntimeEvent
	}
	response := &FinalResponse{Summary: strings.TrimSpace(source.Summary)}
	if response.Summary == "" || len(response.Summary) > 16<<10 || strings.ContainsRune(response.Summary, '\x00') {
		return nil, ErrInvalidRuntimeEvent
	}
	seen := make(map[string]bool, len(source.ResultIDs))
	for _, raw := range source.ResultIDs {
		id := strings.TrimSpace(raw)
		if id == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n/\\") || seen[id] {
			return nil, ErrInvalidRuntimeEvent
		}
		seen[id] = true
		response.ResultIDs = append(response.ResultIDs, id)
	}
	return response, nil
}

func normalizeProgressUpdate(source *ProgressUpdate) (*ProgressUpdate, error) {
	if source == nil {
		return nil, ErrInvalidRuntimeEvent
	}
	summary, err := normalizeEntrySummary(source.Summary)
	if err != nil {
		return nil, err
	}
	return &ProgressUpdate{Summary: summary}, nil
}

func normalizeFailureReport(source *FailureReport) (*FailureReport, error) {
	if source == nil {
		return nil, ErrInvalidRuntimeEvent
	}
	report, err := NewFailureReport(source.Code, source.Summary)
	if err != nil {
		return nil, err
	}
	return &report, nil
}

func normalizeEntrySummary(summary string) (string, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" || len(summary) > 4<<10 || strings.ContainsRune(summary, '\x00') {
		return "", ErrInvalidRuntimeEvent
	}
	return summary, nil
}

func validFailureCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, character := range code {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func runtimeEventState(current vocabulary.ArchivistTurnState, kind RuntimeEventKind) (vocabulary.ArchivistTurnState, error) {
	switch kind {
	case RuntimeInputAccepted:
		if current != vocabulary.ArchivistTurnLeased && current != vocabulary.ArchivistTurnSubmitted {
			return current, ErrInvalidTurnTransition
		}
		return vocabulary.ArchivistTurnSubmitted, nil
	case RuntimeTurnStarted:
		if current != vocabulary.ArchivistTurnSubmitted && current != vocabulary.ArchivistTurnRunning {
			return current, ErrInvalidTurnTransition
		}
		return vocabulary.ArchivistTurnRunning, nil
	case RuntimeTurnProgress:
		if current != vocabulary.ArchivistTurnRunning && current != vocabulary.ArchivistTurnWaiting {
			return current, ErrInvalidTurnTransition
		}
		return current, nil
	case RuntimeTurnWaiting:
		if current != vocabulary.ArchivistTurnRunning && current != vocabulary.ArchivistTurnWaiting {
			return current, ErrInvalidTurnTransition
		}
		return vocabulary.ArchivistTurnWaiting, nil
	case RuntimeTerminalIdle:
		if current != vocabulary.ArchivistTurnSubmitted && current != vocabulary.ArchivistTurnRunning && current != vocabulary.ArchivistTurnWaiting {
			return current, ErrInvalidTurnTransition
		}
		return current, nil
	case RuntimeTurnCompleted:
		if current != vocabulary.ArchivistTurnRunning && current != vocabulary.ArchivistTurnWaiting {
			return current, ErrInvalidTurnTransition
		}
		return vocabulary.ArchivistTurnCompleted, nil
	case RuntimeTurnFailed:
		if current != vocabulary.ArchivistTurnRunning && current != vocabulary.ArchivistTurnWaiting {
			return current, ErrInvalidTurnTransition
		}
		return vocabulary.ArchivistTurnFailed, nil
	default:
		return current, ErrInvalidRuntimeEvent
	}
}

func terminalTurnState(state vocabulary.ArchivistTurnState) bool {
	return state == vocabulary.ArchivistTurnCompleted || state == vocabulary.ArchivistTurnFailed || state == vocabulary.ArchivistTurnCancelled
}

type RuntimeBinding struct {
	SessionID       string
	ResumeReference string
	Handle          string
}

func RebindRuntime(session Session, previous, discovered RuntimeBinding) (RuntimeBinding, error) {
	if session.ID == "" || previous.SessionID != session.ID || discovered.SessionID != session.ID ||
		previous.ResumeReference == "" || discovered.ResumeReference != previous.ResumeReference || discovered.Handle == "" {
		return RuntimeBinding{}, ErrRuntimeBindingMismatch
	}
	return discovered, nil
}

type ResumeMode string

const (
	ResumeNative     ResumeMode = "NATIVE"
	ResumeCheckpoint ResumeMode = "CHECKPOINT"
)

type CheckpointToolCall struct {
	ID             string
	IdempotencyKey string
	Completed      bool
	ResultSummary  string
}

type PendingToolCall struct {
	ID             string
	IdempotencyKey string
}

type ResumeCheckpointData struct {
	Goal             string
	ConfirmedState   string
	CompletedResults map[string]string
	PendingToolCalls []PendingToolCall
}

type ResumePlan struct {
	Mode                  ResumeMode
	NativeResumeReference string
	Checkpoint            *ResumeCheckpointData
}

func PlanResume(goal, confirmedState, nativeResumeReference string, calls []CheckpointToolCall) (ResumePlan, error) {
	nativeResumeReference = strings.TrimSpace(nativeResumeReference)
	if nativeResumeReference != "" {
		return ResumePlan{Mode: ResumeNative, NativeResumeReference: nativeResumeReference}, nil
	}
	goal = strings.TrimSpace(goal)
	confirmedState = strings.TrimSpace(confirmedState)
	if goal == "" || confirmedState == "" {
		return ResumePlan{}, ErrInvalidResumePlan
	}
	checkpoint := &ResumeCheckpointData{
		Goal: goal, ConfirmedState: confirmedState, CompletedResults: make(map[string]string),
	}
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		call.ID = strings.TrimSpace(call.ID)
		call.IdempotencyKey = strings.TrimSpace(call.IdempotencyKey)
		if call.ID == "" || call.IdempotencyKey == "" || seen[call.ID] || (call.Completed && strings.TrimSpace(call.ResultSummary) == "") {
			return ResumePlan{}, ErrInvalidResumePlan
		}
		seen[call.ID] = true
		if call.Completed {
			checkpoint.CompletedResults[call.ID] = strings.TrimSpace(call.ResultSummary)
			continue
		}
		checkpoint.PendingToolCalls = append(checkpoint.PendingToolCalls, PendingToolCall{ID: call.ID, IdempotencyKey: call.IdempotencyKey})
	}
	return ResumePlan{Mode: ResumeCheckpoint, Checkpoint: checkpoint}, nil
}

type CLIExecutable struct {
	Executable     string
	FixedArguments []string
}

type LaunchPolicy struct {
	CLIs           map[string]CLIExecutable
	WorkspaceRoots map[string]string
}

type LaunchPlan struct {
	Executable       string
	Arguments        []string
	WorkingDirectory string
}

func BuildLaunchPlan(session Session, policy LaunchPolicy) (LaunchPlan, error) {
	if _, err := NewSession(SessionSpec(session)); err != nil {
		return LaunchPlan{}, ErrUnsafeRuntimeExecution
	}
	cli, cliAllowed := policy.CLIs[session.CLIID]
	root, rootAllowed := policy.WorkspaceRoots[session.WorkspaceRootID]
	cli.Executable = strings.TrimSpace(cli.Executable)
	root = strings.TrimSpace(root)
	if !cliAllowed || !rootAllowed || !absolutePath(cli.Executable) || !absolutePath(root) ||
		strings.ContainsAny(cli.Executable, "\x00\r\n") || strings.ContainsAny(root, "\x00\r\n") {
		return LaunchPlan{}, ErrUnsafeRuntimeExecution
	}
	return LaunchPlan{
		Executable: cli.Executable, Arguments: append([]string(nil), cli.FixedArguments...), WorkingDirectory: root,
	}, nil
}

func absolutePath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}
