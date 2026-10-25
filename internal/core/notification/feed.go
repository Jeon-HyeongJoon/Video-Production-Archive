package notification

import (
	"errors"
	"sync"

	"videoarchive/internal/core/command"
)

type Kind string

const (
	KindEvent    Kind = "EVENT"
	KindProgress Kind = "PROGRESS"
	KindResource Kind = "RESOURCE"
	KindLog      Kind = "LOG"
)

var ErrEventSequence = errors.New("domain event sequence must increase")

type Notification struct {
	EventID uint64
	Kind    Kind
	Type    string
	Payload any
}

type persistedEvent struct {
	sequence uint64
	event    command.Event
}

// Feed is a replay projection over the authoritative command.Event stream. It
// never creates another Event identity; the persistence adapter supplies the
// monotonically increasing sequence used as the reconnect cursor.
type Feed struct {
	mu     sync.Mutex
	events []persistedEvent
}

func (feed *Feed) AppendEvent(sequence uint64, event command.Event) (Notification, error) {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if sequence == 0 || event.Validate() != nil || (len(feed.events) > 0 && sequence <= feed.events[len(feed.events)-1].sequence) {
		return Notification{}, ErrEventSequence
	}
	stored := persistedEvent{sequence: sequence, event: event}
	feed.events = append(feed.events, stored)
	return eventNotification(stored), nil
}

func (feed *Feed) Transient(kind Kind, notificationType string, payload any) Notification {
	return Notification{Kind: kind, Type: notificationType, Payload: payload}
}

func (feed *Feed) ReplayAfter(lastEventID uint64) []Notification {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	result := make([]Notification, 0, len(feed.events))
	for _, event := range feed.events {
		if event.sequence > lastEventID {
			result = append(result, eventNotification(event))
		}
	}
	return result
}

func eventNotification(stored persistedEvent) Notification {
	return Notification{EventID: stored.sequence, Kind: KindEvent, Type: stored.event.EventType, Payload: stored.event}
}
