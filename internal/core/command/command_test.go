package command

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func validEnvelope() Envelope {
	return Envelope{
		CommandID: "CMD-1", CommandType: "render.create", Actor: Actor{Type: HumanActor, ID: "op-1"},
		RequestedVia: "dashboard", TargetID: "JOB-1", ExpectedRevision: 3,
		IdempotencyKey: "JOB-1:render:v1", Parameters: map[string]any{"profile": "proxy-v1"}, Reason: "create proxy",
	}
}

func testStore(revision uint64) *MemoryStore {
	return NewMemoryStore(map[string]Snapshot{
		"JOB-1": {TargetID: "JOB-1", Revision: revision, State: json.RawMessage(`{"status":"before"}`)},
	})
}

func successfulHandler(envelope Envelope, current Snapshot) (Change, error) {
	return Change{
		Revision: current.Revision + 1,
		State:    json.RawMessage(`{"status":"after"}`),
		Value:    "ok",
		Events:   []Event{{EventID: "EVT-1", EventType: "RenditionPublished"}},
	}, nil
}

// Requirement: CMD-001
func TestCommandEnvelopeRequiresAuditAndConcurrencyFields(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Envelope)
	}{
		{"command id", func(e *Envelope) { e.CommandID = "" }},
		{"command type", func(e *Envelope) { e.CommandType = "" }},
		{"actor type", func(e *Envelope) { e.Actor.Type = "" }},
		{"actor id", func(e *Envelope) { e.Actor.ID = "" }},
		{"interface", func(e *Envelope) { e.RequestedVia = "" }},
		{"target", func(e *Envelope) { e.TargetID = "" }},
		{"idempotency", func(e *Envelope) { e.IdempotencyKey = "" }},
		{"reason", func(e *Envelope) { e.Reason = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope := validEnvelope()
			testCase.edit(&envelope)
			if !errors.Is(envelope.Validate(), ErrInvalidEnvelope) {
				t.Fatal("expected invalid envelope")
			}
		})
	}
}

// Requirement: CMD-002
func TestCommandRejectsStaleExpectedRevision(t *testing.T) {
	calls := 0
	executor := NewExecutor(testStore(4), func(Envelope, Snapshot) (Change, error) {
		calls++
		return Change{}, nil
	})
	_, err := executor.Execute(validEnvelope())
	if !errors.Is(err, ErrRevisionConflict) || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

// Requirement: CMD-003
func TestCommandIdempotentReplayReturnsStoredResultOnce(t *testing.T) {
	calls := 0
	executor := NewExecutor(testStore(3), func(envelope Envelope, current Snapshot) (Change, error) {
		calls++
		return successfulHandler(envelope, current)
	})
	first, err := executor.Execute(validEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	replay := validEnvelope()
	replay.CommandID = "CMD-2"
	replay.RequestedVia = "mcp"
	replay.Reason = "same intent retried through another adapter"
	second, err := executor.Execute(replay)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(first, second) {
		t.Fatalf("calls=%d first=%v second=%v", calls, first, second)
	}
}

// Requirement: CMD-004
func TestCommandRejectsIdempotencyKeyReuseWithDifferentPayload(t *testing.T) {
	executor := NewExecutor(testStore(3), successfulHandler)
	if _, err := executor.Execute(validEnvelope()); err != nil {
		t.Fatal(err)
	}
	changed := validEnvelope()
	changed.Parameters["profile"] = "archive-v2"
	_, err := executor.Execute(changed)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v, want idempotency conflict", err)
	}
}

type observingStore struct {
	Store
	loadCalls int
}

func (store *observingStore) Load(targetID string) (Snapshot, error) {
	store.loadCalls++
	return store.Store.Load(targetID)
}

// Requirement: CMD-005
func TestCommandAuthorizationRunsBeforeRevisionReadAndHandler(t *testing.T) {
	denied := errors.New("not authorized")
	store := &observingStore{Store: testStore(3)}
	handlerCalls := 0
	executor := NewAuthorizedExecutor(store, func(Envelope) error { return denied }, func(Envelope, Snapshot) (Change, error) {
		handlerCalls++
		return Change{}, nil
	})
	if _, err := executor.Execute(validEnvelope()); !errors.Is(err, denied) {
		t.Fatalf("got %v, want authorization denial", err)
	}
	if store.loadCalls != 0 || handlerCalls != 0 {
		t.Fatalf("authorization ran too late: loadCalls=%d handlerCalls=%d", store.loadCalls, handlerCalls)
	}
}

// Requirement: CMD-006
func TestCommandReplayReturnsStoredFailureWithoutCallingHandlerAgain(t *testing.T) {
	unsupportedCodec := errors.New("unsupported codec")
	calls := 0
	executor := NewExecutor(testStore(3), func(Envelope, Snapshot) (Change, error) {
		calls++
		return Change{}, DeterministicFailure("UNSUPPORTED_CODEC", unsupportedCodec)
	})
	if _, err := executor.Execute(validEnvelope()); !errors.Is(err, unsupportedCodec) {
		t.Fatalf("first error=%v", err)
	}
	replay := validEnvelope()
	replay.CommandID = "CMD-2"
	if _, err := executor.Execute(replay); !errors.Is(err, unsupportedCodec) {
		t.Fatalf("replayed error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("handler called %d times", calls)
	}
}

// Requirement: CMD-007
func TestAcceptedCommandCommitsStateRecordEventsAndReceiptAtomically(t *testing.T) {
	store := testStore(3)
	executor := NewExecutor(store, successfulHandler)
	if _, err := executor.Execute(validEnvelope()); err != nil {
		t.Fatal(err)
	}
	state, records, events, _ := store.Snapshot("JOB-1")
	receipt, found, err := store.FindReceipt(validEnvelope().IdempotencyKey)
	if err != nil || !found || state.Revision != 4 || string(state.State) != `{"status":"after"}` ||
		len(records) != 1 || !records[0].Accepted || len(events) != 1 || receipt.Result.Revision != 4 {
		t.Fatalf("state=%+v records=%+v events=%+v receipt=%+v found=%v err=%v", state, records, events, receipt, found, err)
	}
	event := events[0]
	if event.CommandID != "CMD-1" || event.Actor.ID != "op-1" || event.RequestedVia != "dashboard" ||
		event.BeforeRevision != 3 || event.AfterRevision != 4 || event.Reason != "create proxy" {
		t.Fatalf("event lacks command provenance: %+v", event)
	}
}

// Requirement: CMD-008
func TestRejectedCommandAppendsCommandRecordWithoutDomainEvent(t *testing.T) {
	store := testStore(3)
	denied := errors.New("automation policy denied")
	executor := NewAuthorizedExecutor(store, func(Envelope) error { return denied }, func(Envelope, Snapshot) (Change, error) {
		t.Fatal("handler must not run")
		return Change{}, nil
	})
	if _, err := executor.Execute(validEnvelope()); !errors.Is(err, denied) {
		t.Fatalf("error=%v", err)
	}
	state, records, events, _ := store.Snapshot("JOB-1")
	if state.Revision != 3 || len(records) != 1 || records[0].Accepted || records[0].Error != denied.Error() || len(events) != 0 {
		t.Fatalf("state=%+v records=%+v events=%+v", state, records, events)
	}
}

type failAcceptedCommitOnce struct {
	Store
	err    error
	failed bool
}

func (store *failAcceptedCommitOnce) Commit(key string, commit Commit) error {
	if commit.NextState != nil && !store.failed {
		store.failed = true
		return store.err
	}
	return store.Store.Commit(key, commit)
}

// Requirement: CMD-009
func TestFailedUnitOfWorkCommitLeavesStateAndReceiptUnchanged(t *testing.T) {
	base := testStore(3)
	commitFailure := errors.New("database commit failed")
	store := &failAcceptedCommitOnce{Store: base, err: commitFailure}
	calls := 0
	executor := NewExecutor(store, func(envelope Envelope, current Snapshot) (Change, error) {
		calls++
		return successfulHandler(envelope, current)
	})
	if _, err := executor.Execute(validEnvelope()); !errors.Is(err, commitFailure) {
		t.Fatalf("first error=%v", err)
	}
	state, records, events, _ := base.Snapshot("JOB-1")
	_, found, _ := base.FindReceipt(validEnvelope().IdempotencyKey)
	if state.Revision != 3 || len(records) != 0 || len(events) != 0 || found {
		t.Fatalf("partial commit: state=%+v records=%+v events=%+v receipt=%v", state, records, events, found)
	}
	if _, err := executor.Execute(validEnvelope()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	state, records, events, _ = base.Snapshot("JOB-1")
	if calls != 2 || state.Revision != 4 || len(records) != 1 || len(events) != 1 {
		t.Fatalf("retry result: calls=%d state=%+v records=%+v events=%+v", calls, state, records, events)
	}
}

// Requirement: CMD-010
func TestRetryableHandlerFailureIsRecordedButNotCachedAsReceipt(t *testing.T) {
	temporary := errors.New("temporary network failure")
	calls := 0
	executor := NewExecutor(testStore(3), func(envelope Envelope, current Snapshot) (Change, error) {
		calls++
		if calls == 1 {
			return Change{}, temporary
		}
		return successfulHandler(envelope, current)
	})
	if _, err := executor.Execute(validEnvelope()); !errors.Is(err, temporary) {
		t.Fatalf("first error=%v", err)
	}
	if _, err := executor.Execute(validEnvelope()); err != nil {
		t.Fatalf("retry was frozen by receipt: %v", err)
	}
	if calls != 2 {
		t.Fatalf("handler calls=%d", calls)
	}
}
