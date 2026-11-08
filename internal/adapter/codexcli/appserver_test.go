package codexcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"videoarchive/internal/application/archivistloop"
)

type fixtureTools struct {
	available []archivistloop.AvailableTool
	calls     []archivistloop.ToolEnvelope
}

func (tools *fixtureTools) List(context.Context) ([]archivistloop.AvailableTool, error) {
	return tools.available, nil
}

func (tools *fixtureTools) Call(_ context.Context, envelope archivistloop.ToolEnvelope) (archivistloop.ToolResult, error) {
	tools.calls = append(tools.calls, envelope)
	return archivistloop.ToolResult{StatusCode: 200, Body: json.RawMessage(`{"archive_run_id":"RUN-1"}`)}, nil
}

type emittedRecord struct {
	kind      string
	summary   string
	resultIDs []string
}

// Requirement: LRH-018
func TestCodexAppServerResumesNativeThreadAndPublishesOnlySanitizedTurnEvents(t *testing.T) {
	temporary := t.TempDir()
	fixture := filepath.Join(temporary, "fake-codex")
	if runtime.GOOS == "windows" {
		fixture += ".exe"
	}
	build := exec.Command("go", "build", "-o", fixture, "./testdata/fakecodex")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	trace := filepath.Join(temporary, "trace.txt")
	t.Setenv("ARCHIVE_CODEX_FIXTURE_TRACE", trace)
	state := filepath.Join(temporary, "state")
	working := filepath.Join(temporary, "working")
	if err := os.Mkdir(working, 0o700); err != nil {
		t.Fatal(err)
	}
	tools := &fixtureTools{available: []archivistloop.AvailableTool{
		{Name: "archive.run.status.v1", ActionClass: "READ"},
		{Name: "run_shell", ActionClass: "PROCESS"},
	}}
	var diagnostic bytes.Buffer
	var records []emittedRecord
	emit := func(kind, summary string, resultIDs []string) error {
		records = append(records, emittedRecord{kind: kind, summary: summary, resultIDs: resultIDs})
		return nil
	}
	for index := 1; index <= 2; index++ {
		err := Run(context.Background(), Options{
			CodexExecutable: fixture, MCPExecutable: fixture, WorkingDirectory: working,
			StateDirectory: state, MCPURL: "http://127.0.0.1:18080/api/v1/archivist/mcp",
			Runtime: archivistloop.RuntimeContext{
				SessionID: "SESSION-1", TurnID: "TURN-" + string(rune('0'+index)), FencingToken: uint64(index),
			},
			MaximumCalls: 4, Log: &diagnostic,
		}, tools, `PRIVATE_PROMPT C:\Archive\private\clip.mov 상태를 확인해`, emit)
		if err != nil {
			t.Fatalf("run %d: %v", index, err)
		}
	}
	if len(records) != 8 {
		t.Fatalf("expected four public records per turn, got %#v", records)
	}
	for _, record := range records {
		if strings.Contains(record.summary, "PRIVATE") || strings.Contains(record.summary, `C:\`) || strings.Contains(record.summary, working) {
			t.Fatalf("private runtime data escaped into a public record: %#v", record)
		}
	}
	if records[1].summary != "선택 범위를 [경로] 에서 확인합니다." || records[2].summary != "아카이브 실행 상태 확인" ||
		records[3].kind != "final" || records[3].summary != "아카이브 상태를 확인했습니다." {
		t.Fatalf("unexpected first-turn web-chat records: %#v", records[:4])
	}
	if strings.Contains(diagnostic.String(), "PRIVATE") || strings.Contains(diagnostic.String(), working) || strings.Contains(diagnostic.String(), `C:\`) {
		t.Fatalf("private runtime data escaped into diagnostics: %s", diagnostic.String())
	}
	tracePayload, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := strings.Join([]string{
		"initialize", "initialized", "thread/start", "turn/start",
		"initialize", "initialized", "thread/resume", "turn/start", "",
	}, "\n")
	if string(tracePayload) != wantTrace {
		t.Fatalf("native thread lifecycle mismatch\nwant: %q\n got: %q", wantTrace, tracePayload)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one native thread mapping: entries=%v err=%v", entries, err)
	}
	mapping, err := os.ReadFile(filepath.Join(state, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mapping, []byte("codex-thread-fixed")) || bytes.Contains(mapping, []byte("PRIVATE")) ||
		bytes.Contains(mapping, []byte("clip.mov")) || bytes.Contains(mapping, []byte(working)) {
		t.Fatalf("native thread mapping contains the wrong data: %s", mapping)
	}
}

// Requirement: LRH-019
func TestCodexAppServerRunsNativeToolsWithoutApprovalSandbox(t *testing.T) {
	temporary := t.TempDir()
	fixture := filepath.Join(temporary, "fake-codex")
	if runtime.GOOS == "windows" {
		fixture += ".exe"
	}
	build := exec.Command("go", "build", "-o", fixture, "./testdata/fakecodex")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	t.Setenv("ARCHIVE_CODEX_FIXTURE_NATIVE_TOOLS", "1")
	state := filepath.Join(temporary, "state")
	working := filepath.Join(temporary, "working")
	if err := os.Mkdir(working, 0o700); err != nil {
		t.Fatal(err)
	}
	tools := &fixtureTools{available: []archivistloop.AvailableTool{{Name: "archive.run.status.v1", ActionClass: "READ"}}}
	var records []emittedRecord
	err := Run(context.Background(), Options{
		CodexExecutable: fixture, MCPExecutable: fixture, WorkingDirectory: working,
		StateDirectory: state, MCPURL: "http://127.0.0.1:18080/api/v1/archivist/mcp",
		Runtime: archivistloop.RuntimeContext{
			SessionID: "SESSION-NATIVE", TurnID: "TURN-NATIVE", FencingToken: 1,
		},
		MaximumCalls: 4,
	}, tools, "터미널과 보조 Agent를 사용해 상태를 확인해", func(kind, summary string, resultIDs []string) error {
		records = append(records, emittedRecord{kind: kind, summary: summary, resultIDs: resultIDs})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"터미널 명령 실행", "파일 변경", "보조 Agent 작업", "아카이브 실행 상태 확인"}
	for _, summary := range want {
		found := false
		for _, record := range records {
			if record.summary == summary {
				found = true
			}
			if strings.Contains(record.summary, "secret.txt") || strings.Contains(record.summary, `C:\`) {
				t.Fatalf("native tool detail escaped into a public record: %#v", record)
			}
		}
		if !found {
			t.Fatalf("missing normalized progress %q in %#v", summary, records)
		}
	}
	if records[len(records)-1].kind != "final" || records[len(records)-1].summary != "아카이브 상태를 확인했습니다." {
		t.Fatalf("unexpected final record: %#v", records)
	}
}
