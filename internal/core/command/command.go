package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"videoarchive/internal/core/vocabulary"
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
	HumanActor  = vocabulary.ActorHuman
	AgentActor  = vocabulary.ActorAgent
	WorkerActor = vocabulary.ActorWorker
	SystemActor = vocabulary.ActorSystem
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
	NextState        *Snapshot
	Record           CommandRecord
	Events           []Event
	Receipt          *Receipt
}

// Store is the Unit of Work boundary. Commit implementations must atomically
// persist all non-nil fields and compare ExpectedRevision in that transaction.
type Store interface {
	Load(targetID string) (Snapshot, error)
	FindReceipt(idempotencyKey string) (Receipt, bool, error)
	Commit(idempotencyKey string, commit Commit) error
}

// MemoryStore is the executable reference implementation of Store atomicity.
type MemoryStore struct {
	mu       sync.Mutex
	states   map[string]Snapshot
	records  []CommandRecord
	events   []Event
	receipts map[string]Receipt
}

func NewMemoryStore(states map[string]Snapshot) *MemoryStore {
	cloned := make(map[string]Snapshot, len(states))
	for id, snapshot := range states {
		cloned[id] = cloneSnapshot(snapshot)
	}
	return &MemoryStore{states: cloned, receipts: make(map[string]Receipt)}
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

func (store *MemoryStore) Commit(key string, commit Commit) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	if err := validateCommit(key, commit); err != nil {
		return err
	}
	if commit.NextState != nil {
		current, ok := store.states[commit.NextState.TargetID]
		if !ok {
			return ErrTargetNotFound
		}
		if current.Revision != *commit.ExpectedRevision {
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
}

func NewExecutor(store Store, handler Handler) *Executor {
	return NewAuthorizedExecutor(store, func(Envelope) error { return nil }, handler)
}

func NewAuthorizedExecutor(store Store, authorize Authorizer, handler Handler) *Executor {
	return &Executor{store: store, authorize: authorize, handler: handler}
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
