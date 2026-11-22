package controlruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: MCP-007
func TestArchiveRunCommandAuthorizerAcceptsOnlyHumanApplicationOrAuthenticatedArchivistMCP(t *testing.T) {
	tests := []struct {
		name      string
		envelope  command.Envelope
		wantError bool
	}{
		{name: "human application", envelope: command.Envelope{Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, RequestedVia: "APPLICATION"}},
		{name: "archivist MCP", envelope: command.Envelope{Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:session-1"}, RequestedVia: "mcp"}},
		{name: "archivist application", envelope: command.Envelope{Actor: command.Actor{Type: vocabulary.ActorArchivist, ID: "archivist:session-1"}, RequestedVia: "APPLICATION"}, wantError: true},
		{name: "human MCP", envelope: command.Envelope{Actor: command.Actor{Type: vocabulary.ActorHuman, ID: "operator-1"}, RequestedVia: "mcp"}, wantError: true},
		{name: "system MCP", envelope: command.Envelope{Actor: command.Actor{Type: vocabulary.ActorSystem, ID: "system"}, RequestedVia: "mcp"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := authorizeArchiveRunEnvelope(test.envelope, security.AuthDisabled)
			if (err != nil) != test.wantError {
				t.Fatalf("authorization mismatch: error=%v want_error=%t", err, test.wantError)
			}
		})
	}
}

// Requirement: OBS-012
func TestControlMonitorLogsEachOutageAndRecoveryWithItsRunBoundary(t *testing.T) {
	var out bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(prior)
	health := make(monitorHealth)
	ctx := context.Background()
	failed := errors.New("private-error-sentinel")
	a, b := monitorKey{"AUTO_TRANSCRIPTION_START", "RUN-A"}, monitorKey{"ARCHIVIST_RECALL", "RUN-B"}
	health.observe(ctx, a, 4, nil)
	health.observe(ctx, a, 4, failed)
	health.observe(ctx, a, 4, failed)
	health.observe(ctx, b, 7, failed)
	health.observe(ctx, a, 5, nil)
	health.observe(ctx, a, 5, nil)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	health.observe(canceled, b, 7, context.Canceled)
	if strings.Contains(out.String(), "private-error-sentinel") {
		t.Fatal("raw application error disclosed")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("repeated or lost monitor transitions: %s", out.String())
	}
	for i, want := range []struct {
		key      monitorKey
		revision uint64
		level    string
	}{{a, 4, "WARN"}, {b, 7, "WARN"}, {a, 5, "INFO"}} {
		var v struct {
			Operation string    `json:"operation"`
			RunID     string    `json:"archive_run_id"`
			Revision  uint64    `json:"revision"`
			Level     string    `json:"level"`
			Time      time.Time `json:"time"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &v); err != nil || v.Operation != want.key.operation || v.RunID != want.key.runID || v.Revision != want.revision || v.Level != want.level || v.Time.IsZero() {
			t.Fatalf("unexpected transition %s", lines[i])
		}
	}
}
