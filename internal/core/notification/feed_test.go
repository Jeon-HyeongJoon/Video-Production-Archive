package notification

import (
	"reflect"
	"testing"

	"videoarchive/internal/core/command"
)

func domainEvent(id, eventType string) command.Event {
	return command.Event{
		EventID: id, EventType: eventType, CommandID: "CMD-1",
		Actor: command.Actor{Type: command.SystemActor, ID: "control"}, RequestedVia: "system",
		TargetID: "STEP-1", BeforeRevision: 1, AfterRevision: 2, Reason: "state changed",
	}
}

// Requirement: NTF-001
func TestReconnectReplaysOnlyPersistedDomainEventsAfterCursor(t *testing.T) {
	feed := &Feed{}
	first, err := feed.AppendEvent(101, domainEvent("EVT-1", "StepQueued"))
	if err != nil {
		t.Fatal(err)
	}
	feed.Transient(KindProgress, "RenderProgress", 42)
	feed.Transient(KindResource, "MemorySample", 71)
	second, err := feed.AppendEvent(102, domainEvent("EVT-2", "StepCompleted"))
	if err != nil {
		t.Fatal(err)
	}

	replayed := feed.ReplayAfter(first.EventID)
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0], second) {
		t.Fatalf("replayed=%+v want=%+v", replayed, second)
	}
}
