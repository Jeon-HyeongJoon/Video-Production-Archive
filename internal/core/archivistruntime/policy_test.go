package archivistruntime

import (
	"errors"
	"testing"

	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: COL-002
func TestArchivistCommandPolicyRoutesDirectActionCardOrDenialFromModeAndActionClass(t *testing.T) {
	tests := []struct {
		name   string
		mode   workflow.ControlMode
		action vocabulary.ActionClass
		want   CommandRoute
		denied bool
	}{
		{name: "manual process", mode: workflow.ManualMode, action: vocabulary.ActionProcess, denied: true},
		{name: "manual destructive", mode: workflow.ManualMode, action: vocabulary.ActionDestructive, denied: true},
		{name: "assisted process", mode: workflow.AssistedMode, action: vocabulary.ActionProcess, want: RouteActionCard},
		{name: "archivist gated", mode: workflow.ArchivistMode, action: vocabulary.ActionGated, want: RouteActionCard},
		{name: "archivist destructive", mode: workflow.ArchivistMode, action: vocabulary.ActionDestructive, want: RouteActionCard},
		{name: "archivist process", mode: workflow.ArchivistMode, action: vocabulary.ActionProcess, want: RouteDirectCommand},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := RouteArchivistCommand(test.mode, test.action)
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
