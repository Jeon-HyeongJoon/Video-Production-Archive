package sqlitecommand

import (
	"context"
	"encoding/json"
	"testing"

	"videoarchive/internal/core/command"
)

// Requirement: PER-001
func TestSQLiteCommitRollsBackEveryUnitOfWorkRecord(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	before := command.Snapshot{TargetID: "JOB-1", Revision: 3, State: json.RawMessage(`{"status":"before"}`)}
	if err := store.Seed(ctx, before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_domain_event_insert
		BEFORE INSERT ON domain_events
		BEGIN
			SELECT RAISE(ABORT, 'injected event insert failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	expectedRevision := uint64(3)
	event := command.Event{
		EventID: "EVT-1", EventType: "RenditionPublished", CommandID: "CMD-1",
		Actor: command.Actor{Type: command.HumanActor, ID: "operator-1"}, RequestedVia: "dashboard",
		TargetID: "JOB-1", BeforeRevision: 3, AfterRevision: 4, Reason: "create proxy",
	}
	commit := command.Commit{
		ExpectedRevision: &expectedRevision,
		NextState:        &command.Snapshot{TargetID: "JOB-1", Revision: 4, State: json.RawMessage(`{"status":"after"}`)},
		Record: command.CommandRecord{Envelope: command.Envelope{
			CommandID: "CMD-1", CommandType: "render.create", Actor: command.Actor{Type: command.HumanActor, ID: "operator-1"},
			RequestedVia: "dashboard", TargetID: "JOB-1", ExpectedRevision: 3,
			IdempotencyKey: "JOB-1:render:v1", Reason: "create proxy",
		}, Accepted: true},
		Events:  []command.Event{event},
		Receipt: &command.Receipt{Digest: "digest-1", Result: command.Result{CommandID: "CMD-1", TargetID: "JOB-1", Revision: 4, Events: []command.Event{event}}},
	}
	if err := store.Commit("JOB-1:render:v1", commit); err == nil {
		t.Fatal("injected SQLite failure did not abort commit")
	}

	after, err := store.Load("JOB-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || string(after.State) != string(before.State) {
		t.Fatalf("state partially committed: before=%+v after=%+v", before, after)
	}
	for _, table := range []string{"command_records", "domain_events", "idempotency_receipts"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s contains %d partial records", table, count)
		}
	}
}
