package archivistruntime

import (
	"errors"
	"testing"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: COL-002
func TestArchivistCommandPolicyRoutesByActionClassWithoutCreationMode(t *testing.T) {
	tests := []struct {
		name   string
		action vocabulary.ActionClass
		want   CommandRoute
		denied bool
	}{
		{name: "process", action: vocabulary.ActionProcess, want: RouteDirectCommand},
		{name: "gated", action: vocabulary.ActionGated, want: RouteActionCard},
		{name: "destructive", action: vocabulary.ActionDestructive, want: RouteActionCard},
		{name: "read is not a state change", action: vocabulary.ActionRead, denied: true},
		{name: "manage is outside MCP", action: vocabulary.ActionManage, denied: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := RouteArchivistCommand(test.action)
			if test.denied {
				if !errors.Is(err, ErrArchivistCommandDenied) || route != "" {
					t.Fatalf("denied case returned route=%s err=%v", route, err)
				}
				return
			}
			if err != nil || route != test.want {
				t.Fatalf("route=%s want=%s err=%v", route, test.want, err)
			}
		})
	}
}
