package httparchivist

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
)

type Handler struct {
	service *archivistcontrol.Service
	router  http.Handler
}

func New(service *archivistcontrol.Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("runtime HTTP service is required")
	}
	handler := &Handler{service: service}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /archivist/v1/heartbeat", handler.heartbeat)
	mux.HandleFunc("POST /archivist/v1/turns/claim", handler.claim)
	mux.HandleFunc("POST /archivist/v1/turns/{turnID}/delivery", handler.delivery)
	mux.HandleFunc("POST /archivist/v1/turns/{turnID}/events", handler.runtimeEvent)
	mux.HandleFunc("POST /archivist/v1/turns/{turnID}/lease", handler.renewLease)
	handler.router = mux
	return handler, nil
}

type leaseRequest struct {
	FencingToken uint64 `json:"fencing_token"`
}

func (handler *Handler) renewLease(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.service.Authenticate(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "ARCHIVIST_HOST_AUTHENTICATION_FAILED")
		return
	}
	var payload leaseRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	record, err := handler.service.RenewLease(request.Context(), request.PathValue("turnID"), hostID, payload.FencingToken)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"turn_id": record.Turn.ID, "lease_expires_at": record.Turn.Lease.ExpiresAt,
	}})
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.router.ServeHTTP(writer, request)
}

func (handler *Handler) heartbeat(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.service.Heartbeat(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "ARCHIVIST_HOST_AUTHENTICATION_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"archivist_host_id": hostID, "state": "ONLINE"}})
}

func (handler *Handler) claim(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.service.Authenticate(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "ARCHIVIST_HOST_AUTHENTICATION_FAILED")
		return
	}
	claimed, err := handler.service.Claim(request.Context(), hostID)
	if errors.Is(err, archivistcontrol.ErrTurnNotFound) {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, archivistruntime.ErrArchivistHostOffline) {
		writeError(writer, http.StatusConflict, "ARCHIVIST_HOST_OFFLINE")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TURN_CLAIM_FAILED")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"session_id": claimed.Session.ID, "turn_id": claimed.Turn.ID, "request_id": claimed.Turn.RequestID,
		"backend": claimed.Session.Backend, "cli_id": claimed.Session.CLIID,
		"workspace_root_id": claimed.Session.WorkspaceRootID, "prompt": claimed.Prompt,
		"fencing_token": claimed.Lease.FencingToken, "lease_expires_at": claimed.Lease.ExpiresAt,
		"delivered_request_id": claimed.Turn.DeliveredRequestID, "last_runtime_cursor": claimed.Turn.LastRuntimeCursor,
	}})
}

type deliveryRequest struct {
	FencingToken uint64 `json:"fencing_token"`
	RequestID    string `json:"request_id"`
}

func (handler *Handler) delivery(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.service.Authenticate(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "ARCHIVIST_HOST_AUTHENTICATION_FAILED")
		return
	}
	var payload deliveryRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	record, deliver, err := handler.service.PrepareDelivery(request.Context(), request.PathValue("turnID"), hostID, payload.FencingToken, payload.RequestID)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"turn_id": record.Turn.ID, "deliver": deliver}})
}

type eventRequest struct {
	FencingToken  uint64                            `json:"fencing_token"`
	Cursor        uint64                            `json:"cursor"`
	RequestID     string                            `json:"request_id"`
	Kind          archivistruntime.RuntimeEventKind `json:"kind"`
	FinalResponse *archivistruntime.FinalResponse   `json:"final_response,omitempty"`
	Progress      *archivistruntime.ProgressUpdate  `json:"progress,omitempty"`
	Failure       *archivistruntime.FailureReport   `json:"failure,omitempty"`
}

func (handler *Handler) runtimeEvent(writer http.ResponseWriter, request *http.Request) {
	hostID, credential, ok := identity(request)
	if !ok || handler.service.Authenticate(hostID, credential) != nil {
		writeError(writer, http.StatusUnauthorized, "ARCHIVIST_HOST_AUTHENTICATION_FAILED")
		return
	}
	var payload eventRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	record, applied, err := handler.service.ApplyRuntimeEvent(request.Context(), request.PathValue("turnID"), hostID, payload.FencingToken, archivistruntime.RuntimeEvent{
		Cursor: payload.Cursor, RequestID: payload.RequestID, Kind: payload.Kind, FinalResponse: payload.FinalResponse,
		Progress: payload.Progress, Failure: payload.Failure,
	})
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"turn_id": record.Turn.ID, "state": record.Turn.State, "applied": applied}})
}

func identity(request *http.Request) (string, string, bool) {
	hostID := strings.TrimSpace(request.Header.Get("X-Archivist-Host-ID"))
	authorization := request.Header.Get("Authorization")
	if hostID == "" || !strings.HasPrefix(authorization, "Bearer ") {
		return "", "", false
	}
	credential := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	return hostID, credential, credential != ""
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	if request.Header.Get("Content-Type") != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	return true
}

func writeRuntimeError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, archivistruntime.ErrStaleTurnLease), errors.Is(err, archivistruntime.ErrTurnRequestMismatch), errors.Is(err, archivistruntime.ErrInvalidTurnTransition):
		writeError(writer, http.StatusConflict, "TURN_CONFLICT")
	case errors.Is(err, archivistcontrol.ErrTurnNotFound):
		writeError(writer, http.StatusNotFound, "TURN_NOT_FOUND")
	default:
		writeError(writer, http.StatusBadRequest, "INVALID_RUNTIME_EVENT")
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}
