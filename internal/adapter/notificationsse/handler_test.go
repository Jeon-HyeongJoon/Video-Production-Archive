package notificationsse

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/notification"
)

// Requirement: NTF-002
func TestSSEReconnectReplaysMissedNotificationsThenContinuesLive(t *testing.T) {
	handler := New(&notification.Feed{})
	server := httptest.NewServer(handler)
	defer server.Close()

	if err := handler.Publish(1, sseTestEvent("EVT-1")); err != nil {
		t.Fatal(err)
	}
	if err := handler.Publish(2, sseTestEvent("EVT-2")); err != nil {
		t.Fatal(err)
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstRequest, _ := http.NewRequestWithContext(firstContext, http.MethodGet, server.URL, nil)
	firstResponse, err := http.DefaultClient.Do(firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	firstReader := bufio.NewReader(firstResponse.Body)
	if id := readSSEID(t, firstReader); id != 1 {
		t.Fatalf("first notification id=%d", id)
	}
	if id := readSSEID(t, firstReader); id != 2 {
		t.Fatalf("second notification id=%d", id)
	}
	cancelFirst()
	firstResponse.Body.Close()

	if err := handler.Publish(3, sseTestEvent("EVT-3")); err != nil {
		t.Fatal(err)
	}
	secondContext, cancelSecond := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSecond()
	secondRequest, _ := http.NewRequestWithContext(secondContext, http.MethodGet, server.URL, nil)
	secondRequest.Header.Set("Last-Event-ID", "2")
	secondResponse, err := http.DefaultClient.Do(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer secondResponse.Body.Close()
	secondReader := bufio.NewReader(secondResponse.Body)
	if id := readSSEID(t, secondReader); id != 3 {
		t.Fatalf("replayed notification id=%d, want 3", id)
	}
	if err := handler.Publish(4, sseTestEvent("EVT-4")); err != nil {
		t.Fatal(err)
	}
	if id := readSSEID(t, secondReader); id != 4 {
		t.Fatalf("live notification id=%d, want 4", id)
	}
}

func readSSEID(t *testing.T, reader *bufio.Reader) uint64 {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "id:") {
			id, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
	}
}

func sseTestEvent(id string) command.Event {
	return command.Event{
		EventID: id, EventType: "StepChanged", CommandID: "CMD-" + id,
		Actor: command.Actor{Type: command.SystemActor, ID: "scheduler"}, RequestedVia: "policy",
		TargetID: "STEP-1", BeforeRevision: 1, AfterRevision: 2, Reason: "state changed",
	}
}
