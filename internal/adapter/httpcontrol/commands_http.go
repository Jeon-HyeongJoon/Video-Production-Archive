package httpcontrol

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/security"
	"videoarchive/internal/core/workflow"
)

func (handler *Handler) session(writer http.ResponseWriter, request *http.Request) {
	csrfToken := ""
	if cookie, err := request.Cookie("archive_csrf"); err == nil {
		csrfToken = strings.TrimSpace(cookie.Value)
	}
	if csrfToken == "" {
		var err error
		csrfToken, err = randomToken()
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "SESSION_UNAVAILABLE")
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name: "archive_csrf", Value: csrfToken, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: request.TLS != nil,
		})
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{"csrf_token": csrfToken}})
}

type createJobRequest struct {
	JobID            string `json:"job_id"`
	CommandID        string `json:"command_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	Reason           string `json:"reason"`
	TemplateName     string `json:"template_name"`
	TargetTitle      string `json:"target_title"`
	ControlMode      string `json:"control_mode"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type commandRequest struct {
	CommandID        string `json:"command_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	Reason           string `json:"reason"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

func (handler *Handler) createJob(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload createJobRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.commands.Execute(request.Context(), handler.envelope(request, command.Envelope{
		CommandID: payload.CommandID, CommandType: job.CreateCommand, TargetID: payload.JobID,
		ExpectedRevision: payload.ExpectedRevision, IdempotencyKey: payload.IdempotencyKey, Reason: payload.Reason,
		Parameters: map[string]any{
			"template_name": payload.TemplateName, "target_title": payload.TargetTitle, "control_mode": payload.ControlMode,
		},
	}))
	if err != nil {
		writeCommandError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, commandResultView(result))
}

func (handler *Handler) cancelJob(writer http.ResponseWriter, request *http.Request) {
	if !handler.allowMutation(writer, request) {
		return
	}
	var payload commandRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.commands.Execute(request.Context(), handler.envelope(request, command.Envelope{
		CommandID: payload.CommandID, CommandType: job.CancelCommand, TargetID: request.PathValue("jobID"),
		ExpectedRevision: payload.ExpectedRevision, IdempotencyKey: payload.IdempotencyKey, Reason: payload.Reason,
	}))
	if err != nil {
		writeCommandError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, commandResultView(result))
}

func (handler *Handler) envelope(request *http.Request, envelope command.Envelope) command.Envelope {
	principal, _ := request.Context().Value(principalContextKey{}).(security.Principal)
	envelope.Actor = command.Actor{Type: principal.ActorType, ID: principal.ID}
	envelope.RequestedVia = "archive_workspace"
	return envelope
}

func (handler *Handler) allowMutation(writer http.ResponseWriter, request *http.Request) bool {
	cookie, err := request.Cookie("archive_csrf")
	if err != nil || security.ValidateBrowserMutation(security.BrowserMutation{
		Method: request.Method, Origin: request.Header.Get("Origin"), AllowedOrigins: handler.allowedOrigins,
		ContentType: request.Header.Get("Content-Type"), CSRFHeader: request.Header.Get("X-CSRF-Token"),
		CSRFCookie: cookieValue(cookie),
	}) != nil {
		writeError(writer, http.StatusForbidden, "BROWSER_MUTATION_DENIED")
		return false
	}
	return true
}

func cookieValue(cookie *http.Cookie) string {
	if cookie == nil {
		return ""
	}
	return cookie.Value
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
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

func commandResultView(result command.Result) map[string]any {
	return map[string]any{"data": map[string]any{
		"command_id": result.CommandID, "target_id": result.TargetID, "revision": result.Revision,
	}}
}

func writeCommandError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, command.ErrRevisionConflict), errors.Is(err, command.ErrIdempotencyConflict),
		errors.Is(err, workflow.ErrIllegalTransition):
		writeError(writer, http.StatusConflict, "COMMAND_CONFLICT")
	case errors.Is(err, command.ErrInvalidEnvelope), errors.Is(err, job.ErrInvalidJob):
		writeError(writer, http.StatusBadRequest, "INVALID_COMMAND")
	default:
		writeError(writer, http.StatusInternalServerError, "COMMAND_FAILED")
	}
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
