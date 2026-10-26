package httpworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"videoarchive/internal/core/worker"
)

var ErrInvalidConfiguration = errors.New("worker HTTP configuration is invalid")

type Handler struct {
	registry *worker.Registry
	now      func() time.Time
}

type heartbeatRequest struct {
	WorkerID             string   `json:"worker_id"`
	AcceptingWork        bool     `json:"accepting_work"`
	HeavyRunning         int      `json:"heavy_running"`
	AccessibleStorageIDs []string `json:"accessible_storage_ids"`
}

func New(registry *worker.Registry, now func() time.Time) (*Handler, error) {
	if registry == nil || now == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Handler{registry: registry, now: now}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/worker/v1/heartbeat" {
		writeError(writer, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return
	}
	credential := bearerCredential(request.Header.Get("Authorization"))
	if credential == "" {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var payload heartbeatRequest
	if err := decoder.Decode(&payload); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || strings.TrimSpace(payload.WorkerID) == "" || payload.HeavyRunning < 0 {
		writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
		return
	}
	accessible := make(map[string]bool, len(payload.AccessibleStorageIDs))
	for _, raw := range payload.AccessibleStorageIDs {
		storageID := strings.TrimSpace(raw)
		if storageID == "" || accessible[storageID] {
			writeError(writer, http.StatusBadRequest, "INVALID_HEARTBEAT")
			return
		}
		accessible[storageID] = true
	}
	if err := handler.registry.Connect(strings.TrimSpace(payload.WorkerID), credential, handler.now().UTC(), worker.Metrics{
		AcceptingWork: payload.AcceptingWork, HeavyRunning: payload.HeavyRunning, AccessibleStorageIDs: accessible,
	}); err != nil {
		writeError(writer, http.StatusUnauthorized, "WORKER_AUTHENTICATION_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"state": "ONLINE"}})
}

func (handler *Handler) Statuses(context.Context) ([]worker.Status, error) {
	return handler.registry.Statuses(handler.now().UTC()), nil
}

func bearerCredential(header string) string {
	prefix, credential, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(credential)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}
