package controlruntime

import (
	"testing"

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
