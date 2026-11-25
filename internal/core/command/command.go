package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"

	"videoarchive/internal/core/dispatch"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var (
	ErrInvalidEnvelope     = errors.New("invalid command envelope")
	ErrInvalidEvent        = errors.New("invalid domain event")
	ErrInvalidChange       = errors.New("invalid domain state change")
	ErrRevisionConflict    = errors.New("expected revision does not match current revision")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with a different command")
	ErrTargetNotFound      = errors.New("command target was not found")
)

type ActorType = vocabulary.ActorType

const (
	HumanActor     = vocabulary.ActorHuman
	ArchivistActor = vocabulary.ActorArchivist
	WorkerActor    = vocabulary.ActorWorker
	SystemActor    = vocabulary.ActorSystem
)

type Actor struct {
	Type ActorType `json:"type"`
	ID   string    `json:"id"`
}

type Envelope struct {
	CommandID        string         `json:"command_id"`
	CommandType      string         `json:"command_type"`
	Actor            Actor          `json:"actor"`
	RequestedVia     string         `json:"requested_via"`
	TargetID         string         `json:"target_id"`
	ExpectedRevision uint64         `json:"expected_revision"`
	IdempotencyKey   string         `json:"idempotency_key"`
	Parameters       map[string]any `json:"parameters,omitempty"`
	Reason           string         `json:"reason"`
}

func (envelope Envelope) Validate() error {
	if envelope.CommandID == "" || envelope.CommandType == "" || !envelope.Actor.Type.Valid() || envelope.Actor.ID == "" ||
		envelope.RequestedVia == "" || envelope.TargetID == "" || envelope.IdempotencyKey == "" || envelope.Reason == "" {
		return ErrInvalidEnvelope
	}
	return nil
}

type Snapshot struct {
	TargetID string          `json:"target_id"`
	Revision uint64          `json:"revision"`
	State    json.RawMessage `json:"state"`
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.State = append(json.RawMessage(nil), snapshot.State...)
	return snapshot
}

// Change is staged by a Handler. The Store applies it only together with its
// command record, events and idempotency receipt.
type Change struct {
	Revision uint64
	State    json.RawMessage
	Value    any
	Events   []Event
}

type Result struct {
	CommandID string
	TargetID  string
	Revision  uint64
	Value     any
	Events    []Event
}

type Event struct {
	EventID        string         `json:"event_id"`
	EventType      string         `json:"event_type"`
	CommandID      string         `json:"command_id"`
	Actor          Actor          `json:"actor"`
	RequestedVia   string         `json:"requested_via"`
	TargetID       string         `json:"target_id"`
	BeforeRevision uint64         `json:"before_revision"`
	AfterRevision  uint64         `json:"after_revision"`
	Reason         string         `json:"reason"`
	Payload        map[string]any `json:"payload,omitempty"`
}

func (event Event) Validate() error {
	if event.EventID == "" || event.EventType == "" || event.CommandID == "" || !event.Actor.Type.Valid() ||
		event.Actor.ID == "" || event.RequestedVia == "" || event.TargetID == "" || event.Reason == "" ||
		event.AfterRevision <= event.BeforeRevision {
		return ErrInvalidEvent
	}
	return nil
}

type CommandRecord struct {
	Envelope Envelope `json:"envelope"`
	Accepted bool     `json:"accepted"`
	Error    string   `json:"error,omitempty"`
}

// ExecutionFailure marks a handler error whose result is safe to replay.
// Unclassified errors are retryable and never become idempotency receipts.
type ExecutionFailure struct {
	Code          string
	Message       string
	Deterministic bool
	cause         error
}

func (failure *ExecutionFailure) Error() string { return failure.Message }
func (failure *ExecutionFailure) Unwrap() error { return failure.cause }

func DeterministicFailure(code string, cause error) error {
	if code == "" || cause == nil {
		panic("deterministic command failure requires code and cause")
	}
	return &ExecutionFailure{Code: code, Message: cause.Error(), Deterministic: true, cause: cause}
}

func asDeterministicFailure(err error) (*ExecutionFailure, bool) {
	var failure *ExecutionFailure
	return failure, errors.As(err, &failure) && failure.Deterministic
}

type Receipt struct {
	Digest  string
	Result  Result
	Failure *ExecutionFailure
}

type Commit struct {
	ExpectedRevision *uint64
	CreateTarget     bool
	NextState        *Snapshot
	Record           CommandRecord
	Events           []Event
	Receipt          *Receipt
}

type DispatchCommit struct {
	Commit
	Reservation      dispatch.ResourceReservation
	Lease            workflow.ExecutionLease
	Capacity         dispatch.Capacity
	DispatchSequence uint64
}

func (commit DispatchCommit) Validate(idempotencyKey string) error {
	if err := validateCommit(idempotencyKey, commit.Commit); err != nil {
		return err
	}
	if err := commit.Reservation.Validate(); err != nil || commit.NextState == nil ||
		commit.Reservation.JobID != commit.NextState.TargetID || commit.Lease.OwnerWorkerID != commit.Reservation.WorkerID ||
		commit.Lease.FencingToken == 0 || commit.Lease.AcquiredAt.IsZero() || commit.Lease.HeartbeatAt.IsZero() ||
		!commit.Lease.ExpiresAt.After(commit.Lease.AcquiredAt) || commit.DispatchSequence == 0 ||
		commit.Capacity.MaxHeavy < 0 || commit.Capacity.MaxLight < 0 {
		return ErrInvalidChange
	}
	return nil
}

// Store is the Unit of Work boundary. Commit implementations must atomically
// persist all non-nil fields and compare ExpectedRevision in that transaction.
type Store interface {
	Load(targetID string) (Snapshot, error)
	FindReceipt(idempotencyKey string) (Receipt, bool, error)
	AcceptedEnvelope(commandID, idempotencyKey string) (Envelope, error)
	Commit(idempotencyKey string, commit Commit) error
}

type DispatchStore interface {
	RecordDispatchObservation(context.Context, dispatch.Observation) error
	Store
	ListSnapshots() ([]Snapshot, error)
	ListActiveReservations() ([]dispatch.ResourceReservation, error)
	NextFencingToken(stepID string) (uint64, error)
	NextDispatchSequence() (uint64, error)
	CommitDispatch(idempotencyKey string, commit DispatchCommit) error
}

func (store *MemoryStore) ListSnapshots() ([]Snapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	ids := make([]string, 0, len(store.states))
	for id := range store.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneSnapshot(store.states[id]))
	}
	return result, nil
}

func (store *MemoryStore) ListActiveReservations() ([]dispatch.ResourceReservation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	ids := make([]string, 0, len(store.reservations))
	for id := range store.reservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]dispatch.ResourceReservation, 0, len(ids))
	for _, id := range ids {
		reservation := store.reservations[id]
		reservation.PhysicalDeviceIDs = append([]string(nil), reservation.PhysicalDeviceIDs...)
		result = append(result, reservation)
	}
	return result, nil
}

// MemoryStore is the executable reference implementation of Store atomicity.
type MemoryStore struct {
	dispatchObservations []dispatch.Observation
	mu                   sync.Mutex
	states               map[string]Snapshot
	records              []CommandRecord
	events               []Event
	receipts             map[string]Receipt
	reservations         map[string]dispatch.ResourceReservation
	leases               map[string]workflow.ExecutionLease
	fencingTokens        map[string]uint64
	jobDispatchSequences map[string]uint64
	dispatchSequence     uint64
}

func NewMemoryStore(states map[string]Snapshot) *MemoryStore {
	cloned := make(map[string]Snapshot, len(states))
	for id, snapshot := range states {
		cloned[id] = cloneSnapshot(snapshot)
	}
	return &MemoryStore{
		states: cloned, receipts: make(map[string]Receipt), reservations: make(map[string]dispatch.ResourceReservation),
		leases: make(map[string]workflow.ExecutionLease), fencingTokens: make(map[string]uint64),
		jobDispatchSequences: make(map[string]uint64),
	}
}

func (store *MemoryStore) NextFencingToken(stepID string) (uint64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if stepID == "" {
		return 0, ErrInvalidChange
	}
	return store.fencingTokens[stepID] + 1, nil
}

func (store *MemoryStore) NextDispatchSequence() (uint64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.dispatchSequence + 1, nil
}

func (store *MemoryStore) CommitDispatch(key string, commit DispatchCommit) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := commit.Validate(key); err != nil {
		return err
	}
	if current, exists := store.leases[commit.Reservation.StepID]; exists && commit.Reservation.CreatedAt.Before(current.ExpiresAt) {
		return dispatch.ErrExecutionLeaseHeld
	}
	if commit.Lease.FencingToken != store.fencingTokens[commit.Reservation.StepID]+1 {
		return dispatch.ErrFencingTokenConflict
	}
	if commit.DispatchSequence != store.dispatchSequence+1 {
		return ErrRevisionConflict
	}
	var memoryBytes, scratchBytes uint64
	var slots int
	for _, reservation := range store.reservations {
		if reservation.WorkerID != commit.Reservation.WorkerID {
			continue
		}
		memoryBytes += reservation.MemoryBytes
		scratchBytes += reservation.ScratchBytes
		if reservation.ResourceClass == commit.Reservation.ResourceClass {
			slots++
		}
	}
	if exceedsCapacity(commit, memoryBytes, scratchBytes, slots) {
		return dispatch.ErrCapacityUnavailable
	}
	for _, deviceID := range commit.Reservation.PhysicalDeviceIDs {
		for _, existing := range store.reservations {
			for _, reservedID := range existing.PhysicalDeviceIDs {
				if deviceID == reservedID {
					return dispatch.ErrPhysicalDeviceReserved
				}
			}
		}
	}
	if err := store.commitLocked(key, commit.Commit); err != nil {
		return err
	}
	store.reservations[commit.Reservation.ID] = commit.Reservation
	store.leases[commit.Reservation.StepID] = commit.Lease
	store.fencingTokens[commit.Reservation.StepID] = commit.Lease.FencingToken
	store.jobDispatchSequences[commit.Reservation.JobID] = commit.DispatchSequence
	store.dispatchSequence = commit.DispatchSequence
	return nil
}

func (store *MemoryStore) Load(targetID string) (Snapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot, ok := store.states[targetID]
	if !ok {
		return Snapshot{}, ErrTargetNotFound
	}
	return cloneSnapshot(snapshot), nil
}

func (store *MemoryStore) FindReceipt(key string) (Receipt, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	receipt, ok := store.receipts[key]
	return receipt, ok, nil
}

func (store *MemoryStore) AcceptedEnvelope(commandID, key string) (Envelope, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range store.records {
		if record.Accepted && record.Envelope.CommandID == commandID && record.Envelope.IdempotencyKey == key {
			return record.Envelope, nil
		}
	}
	return Envelope{}, ErrTargetNotFound
}

func (store *MemoryStore) Commit(key string, commit Commit) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.commitLocked(key, commit)
}

func (store *MemoryStore) commitLocked(key string, commit Commit) error {
	if err := validateCommit(key, commit); err != nil {
		return err
	}
	if commit.NextState != nil {
		current, ok := store.states[commit.NextState.TargetID]
		if commit.CreateTarget && ok {
			return ErrRevisionConflict
		}
		if !commit.CreateTarget && !ok {
			return ErrTargetNotFound
		}
		if !commit.CreateTarget && current.Revision != *commit.ExpectedRevision {
			return ErrRevisionConflict
		}
	}
	if commit.Receipt != nil {
		if _, exists := store.receipts[key]; exists {
			return ErrIdempotencyConflict
		}
	}

	// Nothing above this point mutates Store data.
	if commit.NextState != nil {
		store.states[commit.NextState.TargetID] = cloneSnapshot(*commit.NextState)
	}
	store.records = append(store.records, commit.Record)
	store.events = append(store.events, commit.Events...)
	if commit.Receipt != nil {
		store.receipts[key] = *commit.Receipt
	}
	return nil
}

func exceedsCapacity(commit DispatchCommit, memoryReserved, scratchReserved uint64, slotReservations int) bool {
	if memoryReserved > commit.Capacity.AvailableMemoryBytes ||
		commit.Reservation.MemoryBytes > commit.Capacity.AvailableMemoryBytes-memoryReserved ||
		scratchReserved > commit.Capacity.ScratchFreeBytes ||
		commit.Reservation.ScratchBytes > commit.Capacity.ScratchFreeBytes-scratchReserved {
		return true
	}
	switch commit.Reservation.ResourceClass {
	case vocabulary.ResourceHeavy:
		return commit.Capacity.HeavyRunning+slotReservations >= commit.Capacity.MaxHeavy
	case vocabulary.ResourceLight:
		return commit.Capacity.LightRunning+slotReservations >= commit.Capacity.MaxLight
	default:
		return false
	}
}

func validateCommit(key string, commit Commit) error {
	for _, event := range commit.Events {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	if commit.NextState == nil {
		if len(commit.Events) != 0 || commit.ExpectedRevision != nil {
			return ErrInvalidChange
		}
	} else if commit.ExpectedRevision == nil || commit.NextState.TargetID == "" ||
		commit.NextState.Revision != *commit.ExpectedRevision+1 || !commit.Record.Accepted {
		return ErrInvalidChange
	}
	if commit.CreateTarget && (commit.NextState == nil || *commit.ExpectedRevision != 0) {
		return ErrInvalidChange
	}
	if commit.Receipt != nil && (key == "" || commit.Receipt.Digest == "") {
		return ErrInvalidChange
	}
	return nil
}

func (store *MemoryStore) Snapshot(targetID string) (Snapshot, []CommandRecord, []Event, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, ok := store.states[targetID]
	return cloneSnapshot(state), append([]CommandRecord(nil), store.records...), append([]Event(nil), store.events...), ok
}

type Handler func(Envelope, Snapshot) (Change, error)
type Authorizer func(Envelope) error

type Executor struct {
	mu        sync.Mutex
	store     Store
	handler   Handler
	authorize Authorizer
	create    bool
}

func NewExecutor(store Store, handler Handler) *Executor {
	return NewAuthorizedExecutor(store, func(Envelope) error { return nil }, handler)
}

func NewAuthorizedExecutor(store Store, authorize Authorizer, handler Handler) *Executor {
	return &Executor{store: store, authorize: authorize, handler: handler}
}

// NewAuthorizedCreatingExecutor allows the Handler to initialize an absent
// target only when the command expects revision zero. The resulting state,
// CommandRecord, Event and receipt are still committed as one Store unit.
func NewAuthorizedCreatingExecutor(store Store, authorize Authorizer, handler Handler) *Executor {
	return &Executor{store: store, authorize: authorize, handler: handler, create: true}
}

func (executor *Executor) Execute(envelope Envelope) (Result, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()

	if err := envelope.Validate(); err != nil {
		return Result{}, executor.recordRejection(envelope, err, nil)
	}
	digest, err := commandDigest(envelope)
	if err != nil {
		return Result{}, executor.recordRejection(envelope, fmt.Errorf("digest command: %w", err), nil)
	}

	previous, found, err := executor.store.FindReceipt(envelope.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if found {
		if envelope.Actor.Type == ArchivistActor && envelope.RequestedVia == "mcp" && envelope.Parameters["archivist_tool_call_id"] != nil && previous.Failure == nil {
			// A reclaimed Turn has a new lease. Control checked that lease before
			// mapping the Tool Call. Compare the business request to its first
			// committed envelope, including its Session, Turn, Tool Call and actor.
			original, err := executor.store.AcceptedEnvelope(previous.Result.CommandID, envelope.IdempotencyKey)
			if err != nil {
				return Result{}, err
			}
			request := envelope
			request.Parameters = maps.Clone(envelope.Parameters)
			request.Parameters["archivist_turn_lease_fencing_token"] = original.Parameters["archivist_turn_lease_fencing_token"]
			first, err := json.Marshal(original)
			if err != nil {
				return Result{}, err
			}
			replay, err := json.Marshal(request)
			if err != nil {
				return Result{}, err
			}
			if !bytes.Equal(first, replay) {
				return Result{}, executor.recordRejection(envelope, ErrIdempotencyConflict, nil)
			}
			return previous.Result, nil
		}
		if previous.Digest != digest {
			return Result{}, executor.recordRejection(envelope, ErrIdempotencyConflict, nil)
		}
		if previous.Failure != nil {
			return Result{}, executor.recordRejection(envelope, previous.Failure, nil)
		}
		if err := executor.store.Commit(envelope.IdempotencyKey, Commit{Record: CommandRecord{Envelope: envelope, Accepted: true}}); err != nil {
			return Result{}, err
		}
		return previous.Result, nil
	}

	if executor.authorize != nil {
		if err := executor.authorize(envelope); err != nil {
			return Result{}, executor.recordRejection(envelope, err, nil)
		}
	}

	current, err := executor.store.Load(envelope.TargetID)
	creating := errors.Is(err, ErrTargetNotFound) && executor.create && envelope.ExpectedRevision == 0
	if creating {
		current = Snapshot{TargetID: envelope.TargetID}
		err = nil
	}
	if err != nil {
		return Result{}, executor.recordRejection(envelope, err, nil)
	}
	if current.Revision != envelope.ExpectedRevision {
		return Result{}, executor.recordRejection(envelope, ErrRevisionConflict, nil)
	}

	change, err := executor.handler(envelope, cloneSnapshot(current))
	if err != nil {
		if failure, deterministic := asDeterministicFailure(err); deterministic {
			return Result{}, executor.recordRejection(envelope, err, &Receipt{Digest: digest, Failure: failure})
		}
		return Result{}, executor.recordRejection(envelope, err, nil)
	}
	if change.Revision != current.Revision+1 {
		return Result{}, executor.recordRejection(envelope, ErrInvalidChange, nil)
	}

	result := Result{
		CommandID: envelope.CommandID, TargetID: envelope.TargetID, Revision: change.Revision,
		Value: change.Value, Events: enrichEvents(envelope, change.Revision, change.Events),
	}
	next := Snapshot{TargetID: envelope.TargetID, Revision: change.Revision, State: append(json.RawMessage(nil), change.State...)}
	expected := envelope.ExpectedRevision
	commit := Commit{
		ExpectedRevision: &expected,
		CreateTarget:     creating,
		NextState:        &next,
		Record:           CommandRecord{Envelope: envelope, Accepted: true},
		Events:           result.Events,
		Receipt:          &Receipt{Digest: digest, Result: result},
	}
	if err := executor.store.Commit(envelope.IdempotencyKey, commit); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (executor *Executor) recordRejection(envelope Envelope, executionErr error, receipt *Receipt) error {
	record := CommandRecord{Envelope: envelope, Accepted: false, Error: executionErr.Error()}
	if err := executor.store.Commit(envelope.IdempotencyKey, Commit{Record: record, Receipt: receipt}); err != nil {
		return err
	}
	return executionErr
}

func enrichEvents(envelope Envelope, afterRevision uint64, source []Event) []Event {
	events := append([]Event(nil), source...)
	for index := range events {
		events[index].CommandID = envelope.CommandID
		events[index].Actor = envelope.Actor
		events[index].RequestedVia = envelope.RequestedVia
		events[index].TargetID = envelope.TargetID
		events[index].BeforeRevision = envelope.ExpectedRevision
		events[index].AfterRevision = afterRevision
		events[index].Reason = envelope.Reason
	}
	return events
}

func commandDigest(envelope Envelope) (string, error) {
	identity := struct {
		CommandType      string         `json:"command_type"`
		Actor            Actor          `json:"actor"`
		TargetID         string         `json:"target_id"`
		ExpectedRevision uint64         `json:"expected_revision"`
		Parameters       map[string]any `json:"parameters,omitempty"`
	}{
		CommandType: envelope.CommandType, Actor: envelope.Actor, TargetID: envelope.TargetID,
		ExpectedRevision: envelope.ExpectedRevision, Parameters: envelope.Parameters,
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (store *MemoryStore) RecordDispatchObservation(_ context.Context, item dispatch.Observation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.states[item.JobID].Revision != item.JobRevision {
		return nil
	}
	for i := len(store.dispatchObservations) - 1; i >= 0; i-- {
		last := store.dispatchObservations[i]
		if last.JobID == item.JobID && last.StepID == item.StepID {
			if last.SameDecision(item) {
				return nil
			}
			break
		}
	}
	item.Blockers = append([]dispatch.Blocker(nil), item.Blockers...)
	store.dispatchObservations = append(store.dispatchObservations, item)
	return nil
}
func (store *MemoryStore) DispatchObservations(_ context.Context, jobID string, revision uint64) ([]dispatch.Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var items []dispatch.Observation
	seen := map[string]bool{}
	for i := len(store.dispatchObservations) - 1; i >= 0; i-- {
		item := store.dispatchObservations[i]
		if item.JobID == jobID && item.JobRevision == revision && !seen[item.StepID] {
			item.Blockers = append([]dispatch.Blocker(nil), item.Blockers...)
			items = append(items, item)
			seen[item.StepID] = true
		}
	}
	return items, nil
}
