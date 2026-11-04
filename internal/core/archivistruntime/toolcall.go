package archivistruntime

import (
	"errors"
	"strings"

	"videoarchive/internal/core/command"
)

var (
	ErrInvalidToolCall  = errors.New("invalid archivist tool call")
	ErrToolCallConflict = errors.New("archivist tool call receipt conflicts with request")
)

type ToolCallRequest struct {
	ID                 string
	CommandID          string
	ArchivistHostID    string
	ArchivistSessionID string
	ArchivistTurnID    string
	ArchivistID        string
	FencingToken       uint64
	ToolName           string
	InputDigest        string
	IdempotencyKey     string
	TargetID           string
	ExpectedRevision   uint64
	Reason             string
	Parameters         map[string]any
}

type ToolCallReceipt struct {
	ArchivistToolCallID string
	InputDigest         string
	IdempotencyKey      string
	Result              command.Result
}

type ToolCallDecision struct {
	Command *command.Envelope
	Replay  *ToolCallReceipt
}

func PrepareToolCall(request ToolCallRequest, receipts []ToolCallReceipt) (ToolCallDecision, error) {
	request.ID = strings.TrimSpace(request.ID)
	request.CommandID = strings.TrimSpace(request.CommandID)
	request.ArchivistHostID = strings.TrimSpace(request.ArchivistHostID)
	request.ArchivistSessionID = strings.TrimSpace(request.ArchivistSessionID)
	request.ArchivistTurnID = strings.TrimSpace(request.ArchivistTurnID)
	request.ArchivistID = strings.TrimSpace(request.ArchivistID)
	request.ToolName = strings.TrimSpace(request.ToolName)
	request.InputDigest = strings.TrimSpace(request.InputDigest)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.TargetID = strings.TrimSpace(request.TargetID)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.ID == "" || request.CommandID == "" || request.ArchivistHostID == "" || request.ArchivistSessionID == "" ||
		request.ArchivistTurnID == "" || request.ArchivistID == "" || request.FencingToken == 0 || request.ToolName == "" ||
		request.InputDigest == "" || request.IdempotencyKey == "" || request.TargetID == "" || request.Reason == "" {
		return ToolCallDecision{}, ErrInvalidToolCall
	}
	for index := range receipts {
		receipt := receipts[index]
		if receipt.ArchivistToolCallID != request.ID {
			continue
		}
		if receipt.InputDigest != request.InputDigest || receipt.IdempotencyKey != request.IdempotencyKey || receipt.Result.CommandID == "" {
			return ToolCallDecision{}, ErrToolCallConflict
		}
		replay := receipt
		return ToolCallDecision{Replay: &replay}, nil
	}
	parameters := make(map[string]any, len(request.Parameters)+5)
	for key, value := range request.Parameters {
		parameters[key] = value
	}
	parameters["archivist_host_id"] = request.ArchivistHostID
	parameters["archivist_session_id"] = request.ArchivistSessionID
	parameters["archivist_turn_id"] = request.ArchivistTurnID
	parameters["archivist_tool_call_id"] = request.ID
	parameters["archivist_turn_lease_fencing_token"] = request.FencingToken
	envelope := command.Envelope{
		CommandID: request.CommandID, CommandType: request.ToolName,
		Actor: command.Actor{Type: command.ArchivistActor, ID: request.ArchivistID}, RequestedVia: "mcp",
		TargetID: request.TargetID, ExpectedRevision: request.ExpectedRevision,
		IdempotencyKey: request.IdempotencyKey, Parameters: parameters, Reason: request.Reason,
	}
	if err := envelope.Validate(); err != nil {
		return ToolCallDecision{}, ErrInvalidToolCall
	}
	return ToolCallDecision{Command: &envelope}, nil
}
