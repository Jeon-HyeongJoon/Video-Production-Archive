package notificationsse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/notification"
)

type Handler struct {
	mu          sync.Mutex
	feed        *notification.Feed
	subscribers map[chan notification.Notification]struct{}
}

func New(feed *notification.Feed) *Handler {
	return &Handler{feed: feed, subscribers: make(map[chan notification.Notification]struct{})}
}

func (handler *Handler) Publish(sequence uint64, event command.Event) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	message, err := handler.feed.AppendEvent(sequence, event)
	if err != nil {
		return err
	}
	for subscriber := range handler.subscribers {
		subscriber <- message
	}
	return nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	lastEventID := uint64(0)
	if raw := request.Header.Get("Last-Event-ID"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			http.Error(writer, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
		lastEventID = parsed
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")

	subscriber := make(chan notification.Notification, 16)
	handler.mu.Lock()
	replay := handler.feed.ReplayAfter(lastEventID)
	handler.subscribers[subscriber] = struct{}{}
	handler.mu.Unlock()
	defer func() {
		handler.mu.Lock()
		delete(handler.subscribers, subscriber)
		handler.mu.Unlock()
	}()

	for _, message := range replay {
		if err := writeNotification(writer, message); err != nil {
			return
		}
	}
	flusher.Flush()
	for {
		select {
		case <-request.Context().Done():
			return
		case message := <-subscriber:
			if err := writeNotification(writer, message); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeNotification(writer http.ResponseWriter, message notification.Notification) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", message.EventID, message.Type, payload)
	return err
}
