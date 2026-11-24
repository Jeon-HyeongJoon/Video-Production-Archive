package archivistcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidConfiguration   = errors.New("archivist control configuration is invalid")
	ErrInvalidRequest         = errors.New("archivist control request is invalid")
	ErrSessionNotFound        = errors.New("archivist session not found")
	ErrSessionBusy            = errors.New("archivist session has unfinished work")
	ErrSessionRemoved         = errors.New("archivist session is removed")
	ErrTurnNotFound           = errors.New("archivist turn not found")
	ErrArchiveRunNotBound     = errors.New("archive run is not bound to an archivist turn")
	ErrDriveSelectionNotBound = errors.New("drive selection is not bound to an archivist turn")
	ErrWorkReferenceNotBound  = errors.New("work reference is not bound to an archivist turn")
)

type SessionRecord struct {
	Session   archivistruntime.Session
	JobID     string
	CreatedAt time.Time
	RemovedAt time.Time
}

// DiagnosticCheck preserves the comparable observations from the moment the
// automatic Turn was requested. Older Turns and diagnostics without observed Jobs have no comparison.
type DiagnosticCheck struct {
	Subjects   []string `json:"subjects"`
	FailedStep string   `json:"failed_step,omitempty"`
	States     []string `json:"states"`
	Causes     []string `json:"causes"`
	Actions    []string `json:"actions"`
}

type TurnRecord struct {
	Check     *DiagnosticCheck `json:",omitempty"`
	Turn      archivistruntime.Turn
	Prompt    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type SessionProjection struct {
	Session     archivistruntime.Session
	JobID       string
	CreatedAt   time.Time
	Turns       []TurnProjection
	ActionCards []ActionCardProjection
}

type TurnProjection struct {
	Check          *DiagnosticCheck
	ID             string
	RequestID      string
	State          string
	Prompt         string
	SubmittedBy    vocabulary.ActorType
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinalResponse  *archivistruntime.FinalResponse
	Entries        []archivistruntime.TurnEntry
	ToolCalls      []ToolCallProjection
	DriveSelection *archivistruntime.DriveSelection
	WorkReference  *archivistruntime.WorkReference
	WorkContext    *archivistruntime.ArchiveRunWorkContext
	ToolPolicy     archivistruntime.ArchivistTurnToolPolicy
}

type ToolCallProjection struct {
	ID        string
	ToolName  string
	TargetID  string
	State     string
	UpdatedAt time.Time
}

type ActionCardProjection struct {
	ID               string
	ArchivistTurnID  string
	CommandType      string
	TargetID         string
	ExpectedRevision uint64
	State            string
	ApprovalRequired bool
	CommandID        string
	IdempotencyKey   string
	Reason           string
	Basis            []string
	Parameters       map[string]any
	RejectedBy       string
	RejectedAt       time.Time
	RejectionReason  string
}

type ToolCallRecord struct {
	SessionID string
	TurnID    string
	Call      ToolCallProjection
}

type ActionCardRecord struct {
	SessionID string
	Card      ActionCardProjection
}

type ActionCardRejection struct {
	ActionCardID string
	ActorID      string
	Reason       string
	RejectedAt   time.Time
}

type ArchiveRunBinding struct {
	SessionID string
	TurnID    string
	Selection archivistruntime.DriveSelection
}

type DriveSelectionBinding struct {
	SessionID string
	TurnID    string
	Selection archivistruntime.DriveSelection
}

type ClaimedTurn struct {
	Session archivistruntime.Session
	Turn    archivistruntime.Turn
	Prompt  string
	Lease   archivistruntime.TurnLease
}

type Store interface {
	CreateSession(context.Context, SessionRecord) error
	RetireSession(context.Context, string, time.Time) error
	Session(context.Context, string) (SessionRecord, error)
	ListSessions(context.Context) ([]SessionRecord, error)
	CreateTurn(context.Context, TurnRecord) error
	Turn(context.Context, string) (TurnRecord, error)
	ListTurns(context.Context, string) ([]TurnRecord, error)
	PutToolCall(context.Context, ToolCallRecord) error
	ListToolCalls(context.Context, string) ([]ToolCallProjection, error)
	ListTurnEntries(context.Context, string) ([]archivistruntime.TurnEntry, error)
	PutActionCard(context.Context, ActionCardRecord) error
	ListActionCards(context.Context, string) ([]ActionCardProjection, error)
	ActionCard(context.Context, string) (ActionCardRecord, error)
	ClaimActionCard(context.Context, string) (ActionCardRecord, error)
	SetActionCardState(context.Context, string, string) error
	ReleaseActionCard(context.Context, string) error
	RejectActionCard(context.Context, ActionCardRejection) error
	ClaimTurn(context.Context, string, time.Time, time.Duration) (ClaimedTurn, error)
	PrepareDelivery(context.Context, string, string, uint64, time.Time, string) (TurnRecord, bool, error)
	ApplyRuntimeEvent(context.Context, string, string, uint64, time.Time, archivistruntime.RuntimeEvent) (TurnRecord, bool, error)
	RenewTurnLease(context.Context, string, string, uint64, time.Time, time.Duration) (TurnRecord, error)
	BindDriveSelection(context.Context, DriveSelectionBinding) error
	BindArchiveRun(context.Context, ArchiveRunBinding) error
	ArchiveRunBinding(context.Context, string) (ArchiveRunBinding, error)
}

type Service struct {
	store    Store
	hosts    *archivistruntime.HostRegistry
	now      func() time.Time
	leaseTTL time.Duration
	createMu sync.Mutex
}

type RecallTrigger string

const (
	RecallStageOneComplete   RecallTrigger = "STAGE_ONE_COMPLETE"
	RecallArchiveRunComplete RecallTrigger = "ARCHIVE_RUN_COMPLETE"
	RecallNewHold            RecallTrigger = "NEW_HOLD"
)

type RecallEvent struct {
	SessionID    string
	ArchiveRunID string
	Trigger      RecallTrigger
	TriggerID    string
	HoldCount    int
	Selection    archivistruntime.DriveSelection
}

type AdvisoryKind string

const (
	AdvisoryWorkDiagnostic AdvisoryKind = "WORK_DIAGNOSTIC"
	AdvisoryNewDrive       AdvisoryKind = "NEW_DRIVE"
)

type AdvisoryEvent struct {
	Check     *DiagnosticCheck
	SessionID string
	Kind      AdvisoryKind
	TriggerID string
	Prompt    string
	Work      *archivistruntime.WorkReference
}

func New(store Store, hosts *archivistruntime.HostRegistry, now func() time.Time, leaseTTL time.Duration) (*Service, error) {
	if store == nil || hosts == nil || now == nil || leaseTTL <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Service{store: store, hosts: hosts, now: now, leaseTTL: leaseTTL}, nil
}

func (service *Service) CreateSession(ctx context.Context, spec archivistruntime.SessionSpec, jobID string) (SessionRecord, error) {
	if !service.hosts.Allows(spec) {
		return SessionRecord{}, archivistruntime.ErrArchivistHostNotConfigured
	}
	session, err := archivistruntime.NewSession(spec)
	if err != nil {
		return SessionRecord{}, err
	}
	record := SessionRecord{Session: session, JobID: strings.TrimSpace(jobID), CreatedAt: service.now().UTC()}
	if err := service.store.CreateSession(ctx, record); err != nil {
		return SessionRecord{}, err
	}
	return record, nil
}

func (service *Service) CreateArchivist(ctx context.Context, sessionID, name, archivistHostID, cliID string) (SessionRecord, error) {
	service.createMu.Lock()
	defer service.createMu.Unlock()
	spec, err := service.hosts.ConfiguredSessionSpec(
		strings.TrimSpace(sessionID), strings.TrimSpace(name), strings.TrimSpace(archivistHostID), strings.TrimSpace(cliID),
	)
	if err != nil {
		return SessionRecord{}, err
	}
	// The browser generates a fresh technical ID for each submission. Treat an
	// exact active name/Host/CLI match as the same Agent so a double submit or
	// retried response cannot create duplicate collaboration entries.
	existing, err := service.store.ListSessions(ctx)
	if err != nil {
		return SessionRecord{}, err
	}
	for _, record := range existing {
		if record.RemovedAt.IsZero() && record.Session.Name == spec.Name &&
			record.Session.ArchivistHostID == spec.ArchivistHostID && record.Session.CLIID == spec.CLIID {
			return record, nil
		}
	}
	return service.CreateSession(ctx, spec, "")
}

// RemoveSession retires the Agent from active collaboration without deleting
// its durable Turn, Tool Call, or ActionCard history. An unfinished Turn or
// unresolved ActionCard keeps the Session addressable until it is settled.
func (service *Service) RemoveSession(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ErrInvalidRequest
	}
	return service.store.RetireSession(ctx, sessionID, service.now().UTC())
}

func (service *Service) SubmitTurn(ctx context.Context, sessionID, turnID, requestID, prompt string) (TurnRecord, error) {
	return service.SubmitTurnWithDriveSelection(ctx, sessionID, turnID, requestID, prompt, nil)
}

func (service *Service) SubmitTurnWithDriveSelection(ctx context.Context, sessionID, turnID, requestID, prompt string, selection *archivistruntime.DriveSelection) (TurnRecord, error) {
	return service.SubmitTurnWithReferences(ctx, sessionID, turnID, requestID, prompt, selection, nil)
}

func (service *Service) SubmitTurnWithReferences(ctx context.Context, sessionID, turnID, requestID, prompt string, selection *archivistruntime.DriveSelection, work *archivistruntime.WorkReference) (TurnRecord, error) {
	return service.submitTurn(ctx, sessionID, turnID, requestID, prompt, selection, work, archivistruntime.ToolPolicyStandard)
}

func (service *Service) SubmitDiagnosticTurn(ctx context.Context, sessionID, turnID, requestID, prompt string, work *archivistruntime.WorkReference) (TurnRecord, error) {
	if work == nil || work.Kind != archivistruntime.WorkJob {
		return TurnRecord{}, ErrInvalidRequest
	}
	return service.submitTurn(ctx, sessionID, turnID, requestID, prompt, nil, work, archivistruntime.ToolPolicyDiagnostic)
}

func (service *Service) submitTurn(ctx context.Context, sessionID, turnID, requestID, prompt string, selection *archivistruntime.DriveSelection, work *archivistruntime.WorkReference, policy archivistruntime.ArchivistTurnToolPolicy) (TurnRecord, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return TurnRecord{}, ErrInvalidRequest
	}
	session, err := service.store.Session(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return TurnRecord{}, err
	}
	if !session.RemovedAt.IsZero() {
		return TurnRecord{}, ErrSessionRemoved
	}
	turn, err := archivistruntime.NewTurnWithReferences(turnID, sessionID, requestID, selection, work)
	if err != nil {
		return TurnRecord{}, err
	}
	turn.ToolPolicy = policy
	now := service.now().UTC()
	record := TurnRecord{Turn: turn, Prompt: prompt, CreatedAt: now, UpdatedAt: now}
	if err := service.store.CreateTurn(ctx, record); err != nil {
		return TurnRecord{}, err
	}
	return record, nil
}

// HandleRecallEvent queues one SYSTEM Turn for a durable ArchiveRun trigger.
// It does not require the pinned Archivist Host to be online.
func (service *Service) HandleRecallEvent(ctx context.Context, event RecallEvent) (TurnRecord, bool, error) {
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.ArchiveRunID = strings.TrimSpace(event.ArchiveRunID)
	event.TriggerID = strings.TrimSpace(event.TriggerID)
	if event.SessionID == "" || event.ArchiveRunID == "" || event.TriggerID == "" || event.HoldCount < 0 ||
		(event.Trigger != RecallStageOneComplete && event.Trigger != RecallArchiveRunComplete && event.Trigger != RecallNewHold) {
		return TurnRecord{}, false, ErrInvalidRequest
	}
	session, err := service.store.Session(ctx, event.SessionID)
	if err != nil {
		return TurnRecord{}, false, err
	}
	if !session.RemovedAt.IsZero() {
		return TurnRecord{}, true, nil
	}
	binding, err := service.store.ArchiveRunBinding(ctx, event.ArchiveRunID)
	if err != nil || binding.SessionID != event.SessionID {
		return TurnRecord{}, false, ErrInvalidRequest
	}
	selection, err := archivistruntime.NewDriveSelection(event.Selection)
	if err != nil || selection.ArchiveRunID != event.ArchiveRunID || selection.ArchiveRunRevision == 0 ||
		selection.ArchiveRunID != binding.Selection.ArchiveRunID {
		return TurnRecord{}, false, ErrInvalidRequest
	}
	digest := sha256.Sum256([]byte(event.SessionID + "\x00" + event.ArchiveRunID + "\x00" + string(event.Trigger) + "\x00" + event.TriggerID))
	turnID := "SYSTEM-RECALL-" + hex.EncodeToString(digest[:12])
	if stored, loadErr := service.store.Turn(ctx, turnID); loadErr == nil {
		if stored.Turn.SessionID != event.SessionID || stored.Turn.SubmittedBy != vocabulary.ActorSystem {
			return TurnRecord{}, false, command.ErrIdempotencyConflict
		}
		return stored, true, nil
	} else if !errors.Is(loadErr, ErrTurnNotFound) {
		return TurnRecord{}, false, loadErr
	}
	workReference := archivistruntime.WorkReference{
		Kind: archivistruntime.WorkArchiveRun, WorkID: event.ArchiveRunID, Revision: selection.ArchiveRunRevision,
	}
	turn, err := archivistruntime.NewSystemTurnWithReferences(turnID, event.SessionID, "REQUEST-"+turnID, selection, &workReference)
	if err != nil {
		return TurnRecord{}, false, err
	}
	workContext, err := archivistruntime.NewArchiveRunWorkContext(event.ArchiveRunID, binding.TurnID, workTrigger(event.Trigger))
	if err != nil {
		return TurnRecord{}, false, err
	}
	turn, err = archivistruntime.AttachArchiveRunWorkContext(turn, workContext)
	if err != nil {
		return TurnRecord{}, false, err
	}
	prompt := recallPrompt(event)
	now := service.now().UTC()
	record := TurnRecord{Turn: turn, Prompt: prompt, CreatedAt: now, UpdatedAt: now}
	if err := service.store.CreateTurn(ctx, record); err != nil {
		stored, loadErr := service.store.Turn(ctx, turnID)
		if loadErr == nil && stored.Turn.SessionID == event.SessionID && stored.Turn.SubmittedBy == vocabulary.ActorSystem {
			return stored, true, nil
		}
		return TurnRecord{}, false, err
	}
	return record, false, nil
}

func workTrigger(trigger RecallTrigger) archivistruntime.ArchiveRunWorkTrigger {
	switch trigger {
	case RecallStageOneComplete:
		return archivistruntime.WorkStageOneComplete
	case RecallNewHold:
		return archivistruntime.WorkNewHold
	default:
		return archivistruntime.WorkArchiveRunComplete
	}
}

func recallPrompt(event RecallEvent) string {
	switch event.Trigger {
	case RecallStageOneComplete:
		return "ArchiveRun " + event.ArchiveRunID + " stage one complete; inspect results and start transcription"
	case RecallArchiveRunComplete:
		return "ArchiveRun " + event.ArchiveRunID + " complete; report counts, holds, and archive target"
	default:
		return "ArchiveRun " + event.ArchiveRunID + " has " + strconv.Itoa(event.HoldCount) + " hold decisions; review the selected hold list, classify each item only when structured evidence is sufficient, and report the remaining holds"
	}
}

// HandleAdvisoryEvent queues one policy-limited SYSTEM Turn for one immutable
// observation fingerprint. Work diagnostics may use evidence-bound remediation;
// new-drive advice remains read-mostly. The Host may be offline; durable
// delivery happens when that pinned Host reconnects.
func (service *Service) HandleAdvisoryEvent(ctx context.Context, event AdvisoryEvent) (TurnRecord, bool, error) {
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.TriggerID = strings.TrimSpace(event.TriggerID)
	event.Prompt = strings.TrimSpace(event.Prompt)
	if event.SessionID == "" || event.TriggerID == "" || event.Prompt == "" ||
		(event.Kind != AdvisoryWorkDiagnostic && event.Kind != AdvisoryNewDrive) ||
		(event.Kind == AdvisoryWorkDiagnostic && event.Work == nil) ||
		(event.Kind == AdvisoryNewDrive && event.Work != nil) {
		return TurnRecord{}, false, ErrInvalidRequest
	}
	session, err := service.store.Session(ctx, event.SessionID)
	if err != nil {
		return TurnRecord{}, false, err
	}
	if !session.RemovedAt.IsZero() {
		return TurnRecord{}, true, nil
	}
	identity := event.SessionID + "\x00" + string(event.Kind) + "\x00" + event.TriggerID
	if event.Kind == AdvisoryNewDrive {
		identity = string(event.Kind) + "\x00" + event.TriggerID
	}
	digest := sha256.Sum256([]byte(identity))
	turnID := "SYSTEM-ADVISORY-" + hex.EncodeToString(digest[:12])
	if stored, loadErr := service.store.Turn(ctx, turnID); loadErr == nil {
		expectedPolicy := advisoryPolicy(event.Kind)
		legacyDiagnostic := event.Kind == AdvisoryWorkDiagnostic && stored.Turn.ToolPolicy.Effective() == archivistruntime.ToolPolicyAdvisory &&
			stored.Turn.WorkReference != nil && event.Work != nil && *stored.Turn.WorkReference == *event.Work
		if (event.Kind != AdvisoryNewDrive && stored.Turn.SessionID != event.SessionID) || stored.Turn.SubmittedBy != vocabulary.ActorSystem ||
			(stored.Turn.ToolPolicy.Effective() != expectedPolicy && !legacyDiagnostic) {
			return TurnRecord{}, false, command.ErrIdempotencyConflict
		}
		return stored, true, nil
	} else if !errors.Is(loadErr, ErrTurnNotFound) {
		return TurnRecord{}, false, loadErr
	}
	var turn archivistruntime.Turn
	if event.Kind == AdvisoryWorkDiagnostic {
		turn, err = archivistruntime.NewRemediationSystemTurn(turnID, event.SessionID, "REQUEST-"+turnID, event.Work)
	} else {
		turn, err = archivistruntime.NewAdvisorySystemTurn(turnID, event.SessionID, "REQUEST-"+turnID, event.Work)
	}
	if err != nil {
		return TurnRecord{}, false, err
	}
	now := service.now().UTC()
	record := TurnRecord{Check: event.Check, Turn: turn, Prompt: event.Prompt, CreatedAt: now, UpdatedAt: now}
	if err := service.store.CreateTurn(ctx, record); err != nil {
		stored, loadErr := service.store.Turn(ctx, turnID)
		if loadErr == nil && (event.Kind == AdvisoryNewDrive || stored.Turn.SessionID == event.SessionID) &&
			stored.Turn.ToolPolicy.Effective() == advisoryPolicy(event.Kind) {
			return stored, true, nil
		}
		return TurnRecord{}, false, err
	}
	return record, false, nil
}

func advisoryPolicy(kind AdvisoryKind) archivistruntime.ArchivistTurnToolPolicy {
	if kind == AdvisoryWorkDiagnostic {
		return archivistruntime.ToolPolicyRemediation
	}
	return archivistruntime.ToolPolicyAdvisory
}

func (service *Service) TurnDriveSelection(ctx context.Context, sessionID, turnID string) (archivistruntime.DriveSelection, error) {
	turn, err := service.store.Turn(ctx, strings.TrimSpace(turnID))
	if err != nil {
		return archivistruntime.DriveSelection{}, err
	}
	if turn.Turn.SessionID != strings.TrimSpace(sessionID) {
		return archivistruntime.DriveSelection{}, ErrInvalidRequest
	}
	if turn.Turn.DriveSelection == nil {
		return archivistruntime.DriveSelection{}, ErrDriveSelectionNotBound
	}
	return archivistruntime.NewDriveSelection(*turn.Turn.DriveSelection)
}

func (service *Service) TurnWorkReference(ctx context.Context, sessionID, turnID string) (archivistruntime.WorkReference, error) {
	turn, err := service.store.Turn(ctx, strings.TrimSpace(turnID))
	if err != nil {
		return archivistruntime.WorkReference{}, err
	}
	if turn.Turn.SessionID != strings.TrimSpace(sessionID) {
		return archivistruntime.WorkReference{}, ErrInvalidRequest
	}
	if turn.Turn.WorkReference == nil {
		return archivistruntime.WorkReference{}, ErrWorkReferenceNotBound
	}
	return archivistruntime.NewWorkReference(*turn.Turn.WorkReference)
}

func (service *Service) BindDriveSelection(ctx context.Context, sessionID, turnID string, source archivistruntime.DriveSelection) error {
	selection, err := archivistruntime.NewDriveSelection(source)
	if err != nil || selection.ArchiveRunID != "" || selection.ArchiveRunRevision != 0 {
		return ErrInvalidRequest
	}
	return service.store.BindDriveSelection(ctx, DriveSelectionBinding{
		SessionID: strings.TrimSpace(sessionID), TurnID: strings.TrimSpace(turnID), Selection: selection,
	})
}

func (service *Service) BindArchiveRun(ctx context.Context, sessionID, turnID, archiveRunID string, revision uint64) error {
	selection, err := service.TurnDriveSelection(ctx, sessionID, turnID)
	if err != nil {
		return err
	}
	selection.ArchiveRunID = strings.TrimSpace(archiveRunID)
	selection.ArchiveRunRevision = revision
	selection, err = archivistruntime.NewDriveSelection(selection)
	if err != nil {
		return ErrInvalidRequest
	}
	return service.store.BindArchiveRun(ctx, ArchiveRunBinding{
		SessionID: strings.TrimSpace(sessionID), TurnID: strings.TrimSpace(turnID), Selection: selection,
	})
}

func (service *Service) RecallArchiveRunProjection(ctx context.Context, archiveRunID string, revision uint64, batchState string, holdCount int) error {
	binding, err := service.store.ArchiveRunBinding(ctx, strings.TrimSpace(archiveRunID))
	if err != nil {
		return err
	}
	events := make([]RecallEvent, 0, 2)
	if holdCount > 0 {
		events = append(events, RecallEvent{
			SessionID: binding.SessionID, ArchiveRunID: archiveRunID, Trigger: RecallNewHold,
			TriggerID: archiveRunID + ":hold:" + strconv.Itoa(holdCount), HoldCount: holdCount, Selection: binding.Selection,
		})
	}
	switch batchState {
	case "TRANSCRIPTION_READY":
		events = append(events, RecallEvent{
			SessionID: binding.SessionID, ArchiveRunID: archiveRunID, Trigger: RecallStageOneComplete,
			TriggerID: archiveRunID + ":stage-one:" + strconv.FormatUint(revision, 10), HoldCount: holdCount, Selection: binding.Selection,
		})
	case "COMPLETED":
		events = append(events, RecallEvent{
			SessionID: binding.SessionID, ArchiveRunID: archiveRunID, Trigger: RecallArchiveRunComplete,
			TriggerID: archiveRunID + ":complete:" + strconv.FormatUint(revision, 10), HoldCount: holdCount, Selection: binding.Selection,
		})
	}
	for _, event := range events {
		if _, _, err := service.HandleRecallEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) Projection(ctx context.Context) ([]SessionProjection, error) {
	sessions, err := service.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]SessionProjection, 0, len(sessions))
	for _, session := range sessions {
		if !session.RemovedAt.IsZero() {
			continue
		}
		turns, err := service.store.ListTurns(ctx, session.Session.ID)
		if err != nil {
			return nil, err
		}
		projection := SessionProjection{Session: session.Session, JobID: session.JobID, CreatedAt: session.CreatedAt}
		for _, turn := range turns {
			toolCalls, err := service.store.ListToolCalls(ctx, turn.Turn.ID)
			if err != nil {
				return nil, err
			}
			entries, err := service.store.ListTurnEntries(ctx, turn.Turn.ID)
			if err != nil {
				return nil, err
			}
			projection.Turns = append(projection.Turns, TurnProjection{
				ID: turn.Turn.ID, RequestID: turn.Turn.RequestID, State: string(turn.Turn.State),
				Prompt: turn.Prompt, SubmittedBy: turn.Turn.SubmittedBy,
				CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt,
				FinalResponse: turn.Turn.FinalResponse, Entries: entries, ToolCalls: toolCalls,
				DriveSelection: turn.Turn.DriveSelection, WorkReference: turn.Turn.WorkReference, WorkContext: turn.Turn.WorkContext,
				ToolPolicy: turn.Turn.ToolPolicy.Effective(), Check: turn.Check,
			})
		}
		projection.ActionCards, err = service.store.ListActionCards(ctx, session.Session.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, projection)
	}
	return result, nil
}

func (service *Service) RecordToolCall(ctx context.Context, sessionID, turnID, id, toolName, targetID, state string) error {
	record := ToolCallRecord{SessionID: strings.TrimSpace(sessionID), TurnID: strings.TrimSpace(turnID), Call: ToolCallProjection{
		ID: strings.TrimSpace(id), ToolName: strings.TrimSpace(toolName), TargetID: strings.TrimSpace(targetID),
		State: strings.TrimSpace(state), UpdatedAt: service.now().UTC(),
	}}
	if record.SessionID == "" || record.TurnID == "" || record.Call.ID == "" || record.Call.ToolName == "" || record.Call.State == "" {
		return ErrInvalidRequest
	}
	return service.store.PutToolCall(ctx, record)
}

func (service *Service) RecordActionCard(ctx context.Context, sessionID string, card ActionCardProjection) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(card.ID) == "" || strings.TrimSpace(card.ArchivistTurnID) == "" ||
		strings.TrimSpace(card.CommandType) == "" || strings.TrimSpace(card.TargetID) == "" || strings.TrimSpace(card.State) == "" {
		return ErrInvalidRequest
	}
	return service.store.PutActionCard(ctx, ActionCardRecord{SessionID: strings.TrimSpace(sessionID), Card: card})
}

func (service *Service) PrepareActionCard(ctx context.Context, cardID, actorID string) (command.Envelope, error) {
	record, err := service.store.ClaimActionCard(ctx, strings.TrimSpace(cardID))
	if err != nil {
		return command.Envelope{}, err
	}
	if record.Card.State != "EXECUTING" {
		return command.Envelope{}, ErrInvalidRequest
	}
	decision, err := archivistruntime.ResolveActionCard(archivistruntime.ActionCard{
		ID: record.Card.ID, ArchivistTurnID: record.Card.ArchivistTurnID, CommandID: record.Card.CommandID,
		CommandType: record.Card.CommandType, TargetID: record.Card.TargetID,
		ExpectedRevision: record.Card.ExpectedRevision, IdempotencyKey: record.Card.IdempotencyKey,
		Reason: record.Card.Reason, Basis: append([]string(nil), record.Card.Basis...),
		ApprovalRequired: record.Card.ApprovalRequired,
		Parameters:       record.Card.Parameters,
	}, archivistruntime.ActionCardRequest{
		Source: archivistruntime.ActionFromCard, ActorType: vocabulary.ActorHuman,
		ActorID: strings.TrimSpace(actorID), CurrentRevision: record.Card.ExpectedRevision,
	})
	if err != nil || decision.Command == nil {
		_ = service.store.ReleaseActionCard(context.WithoutCancel(ctx), record.Card.ID)
		if err == nil {
			err = ErrInvalidRequest
		}
		return command.Envelope{}, err
	}
	return *decision.Command, nil
}

func (service *Service) ReleaseActionCard(ctx context.Context, cardID string) error {
	return service.store.ReleaseActionCard(ctx, strings.TrimSpace(cardID))
}

func (service *Service) BindActionCardArchiveRun(ctx context.Context, cardID, archiveRunID string, revision uint64) error {
	record, err := service.store.ActionCard(ctx, strings.TrimSpace(cardID))
	if err != nil || record.Card.ArchivistTurnID == "" {
		return ErrInvalidRequest
	}
	return service.BindArchiveRun(ctx, record.SessionID, record.Card.ArchivistTurnID, archiveRunID, revision)
}

func (service *Service) CompleteActionCard(ctx context.Context, cardID string) error {
	return service.store.SetActionCardState(ctx, strings.TrimSpace(cardID), "COMPLETED")
}

func (service *Service) RejectActionCard(ctx context.Context, cardID, actorID, reason string) error {
	rejection := ActionCardRejection{
		ActionCardID: strings.TrimSpace(cardID), ActorID: strings.TrimSpace(actorID),
		Reason: strings.TrimSpace(reason), RejectedAt: service.now().UTC(),
	}
	if rejection.ActionCardID == "" || rejection.ActorID == "" || rejection.Reason == "" {
		return ErrInvalidRequest
	}
	return service.store.RejectActionCard(ctx, rejection)
}

func (service *Service) Heartbeat(archivistHostID, credential string) error {
	return service.hosts.Connect(archivistHostID, credential, service.now().UTC())
}

func (service *Service) Authenticate(archivistHostID, credential string) error {
	return service.hosts.Authenticate(archivistHostID, credential)
}

func (service *Service) HostStatuses() []archivistruntime.HostStatus {
	return service.hosts.Statuses(service.now().UTC())
}

func (service *Service) HostProfiles() []archivistruntime.HostProfile {
	return service.hosts.Profiles(service.now().UTC())
}

func (service *Service) Claim(ctx context.Context, archivistHostID string) (ClaimedTurn, error) {
	now := service.now().UTC()
	if !service.hosts.Online(archivistHostID, now) {
		return ClaimedTurn{}, archivistruntime.ErrArchivistHostOffline
	}
	return service.store.ClaimTurn(ctx, archivistHostID, now, service.leaseTTL)
}

func (service *Service) PrepareDelivery(ctx context.Context, turnID, archivistHostID string, fencingToken uint64, requestID string) (TurnRecord, bool, error) {
	return service.store.PrepareDelivery(ctx, turnID, archivistHostID, fencingToken, service.now().UTC(), requestID)
}

func (service *Service) ApplyRuntimeEvent(ctx context.Context, turnID, archivistHostID string, fencingToken uint64, event archivistruntime.RuntimeEvent) (TurnRecord, bool, error) {
	return service.store.ApplyRuntimeEvent(ctx, turnID, archivistHostID, fencingToken, service.now().UTC(), event)
}

func (service *Service) RenewLease(ctx context.Context, turnID, archivistHostID string, fencingToken uint64) (TurnRecord, error) {
	return service.store.RenewTurnLease(ctx, turnID, archivistHostID, fencingToken, service.now().UTC(), service.leaseTTL)
}

func (service *Service) ValidateToolContext(ctx context.Context, archivistHostID, sessionID, turnID string, fencingToken uint64) error {
	session, err := service.store.Session(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return err
	}
	turn, err := service.store.Turn(ctx, strings.TrimSpace(turnID))
	if err != nil {
		return err
	}
	if session.Session.ArchivistHostID != archivistHostID || turn.Turn.SessionID != session.Session.ID {
		return archivistruntime.ErrRuntimeBindingMismatch
	}
	if err := archivistruntime.ValidateTurnReport(turn.Turn, archivistHostID, fencingToken, service.now().UTC()); err != nil {
		return err
	}
	if turn.Turn.State != vocabulary.ArchivistTurnRunning && turn.Turn.State != vocabulary.ArchivistTurnWaiting {
		return archivistruntime.ErrInvalidRuntimeEvent
	}
	return nil
}

func (service *Service) TurnToolPolicy(ctx context.Context, sessionID, turnID string) (archivistruntime.ArchivistTurnToolPolicy, error) {
	turn, err := service.store.Turn(ctx, strings.TrimSpace(turnID))
	if err != nil {
		return "", err
	}
	if turn.Turn.SessionID != strings.TrimSpace(sessionID) {
		return "", ErrInvalidRequest
	}
	policy := turn.Turn.ToolPolicy.Effective()
	if !policy.Valid() {
		return "", ErrInvalidRequest
	}
	return policy, nil
}
