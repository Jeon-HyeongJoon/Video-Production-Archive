package sqlitearchivist

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: LRH-013
func TestRuntimeProgressAndFailureEntriesCommitOnceWithoutRawTerminalFields(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "archivist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err := archivistruntime.NewSession(archivistruntime.SessionSpec{
		ID: "SESSION-1", ArchivistHostID: "HOST-1", Backend: vocabulary.RuntimeEmbeddedCore,
		CLIID: "codex", WorkspaceRootID: "video-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, archivistcontrol.SessionRecord{Session: session, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	turn, err := archivistruntime.NewTurn("TURN-1", session.ID, "REQUEST-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTurn(ctx, archivistcontrol.TurnRecord{Turn: turn, Prompt: "archive work", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTurn(ctx, "HOST-1", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	record, deliver, err := store.PrepareDelivery(ctx, turn.ID, "HOST-1", claimed.Lease.FencingToken, now, turn.RequestID)
	if err != nil || !deliver {
		t.Fatalf("prepare delivery: deliver=%v record=%+v err=%v", deliver, record, err)
	}
	apply := func(at time.Time, event archivistruntime.RuntimeEvent) bool {
		t.Helper()
		_, applied, err := store.ApplyRuntimeEvent(ctx, turn.ID, "HOST-1", claimed.Lease.FencingToken, at, event)
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}
	if !apply(now, archivistruntime.RuntimeEvent{Cursor: 1, RequestID: turn.RequestID, Kind: archivistruntime.RuntimeInputAccepted}) ||
		!apply(now.Add(time.Second), archivistruntime.RuntimeEvent{Cursor: 2, RequestID: turn.RequestID, Kind: archivistruntime.RuntimeTurnStarted}) {
		t.Fatal("runtime start events were not applied")
	}
	progress, err := archivistruntime.NewProgressUpdate("아카이브 자료 조회 완료")
	if err != nil {
		t.Fatal(err)
	}
	progressEvent := archivistruntime.RuntimeEvent{
		Cursor: 3, RequestID: turn.RequestID, Kind: archivistruntime.RuntimeTurnProgress, Progress: &progress,
	}
	if !apply(now.Add(2*time.Second), progressEvent) || apply(now.Add(3*time.Second), progressEvent) {
		t.Fatal("runtime progress cursor was not applied exactly once")
	}
	failure, err := archivistruntime.NewFailureReport("RUNTIME_FAILED", "Archivist 실행 중단")
	if err != nil {
		t.Fatal(err)
	}
	if !apply(now.Add(4*time.Second), archivistruntime.RuntimeEvent{
		Cursor: 4, RequestID: turn.RequestID, Kind: archivistruntime.RuntimeTurnFailed, Failure: &failure,
	}) {
		t.Fatal("runtime failure event was not applied")
	}
	entries, err := store.ListTurnEntries(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Kind != archivistruntime.TurnEntryProgress || entries[0].Cursor != 3 ||
		entries[1].Kind != archivistruntime.TurnEntryFailure || entries[1].Code != "RUNTIME_FAILED" || entries[1].Cursor != 4 {
		t.Fatalf("unexpected durable turn entries: %+v", entries)
	}
	stored, err := store.Turn(ctx, turn.ID)
	if err != nil || stored.Turn.State != vocabulary.ArchivistTurnFailed || stored.Turn.LastRuntimeCursor != 4 {
		t.Fatalf("entry and Turn state were not committed together: %+v err=%v", stored, err)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"stdout", "stderr", "credential", "filesystem_path", "raw_terminal"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("turn entry response exposed forbidden raw field %q: %s", forbidden, encoded)
		}
	}
	rows, err := store.db.QueryContext(ctx, `PRAGMA table_info(archivist_turn_entries)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "raw") || name == "payload" || name == "stdout" || name == "stderr" || name == "path" || name == "credential" {
			t.Fatalf("turn entry table contains unrestricted field %q", name)
		}
	}
}
