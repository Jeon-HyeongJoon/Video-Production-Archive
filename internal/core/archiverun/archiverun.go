// Package archiverun owns the persistent consistency boundary for one drive
// organization and transcription flow. An ArchiveRun is not a command run
// receipt: it keeps the human selection references and fixed folder scope that
// later stage commands operate on.
package archiverun

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"videoarchive/internal/core/command"
	"videoarchive/internal/core/organization"
)

const CreateCommand = "archive_run.create"

var (
	ErrInvalidArchiveRun      = errors.New("archive run is invalid")
	ErrUnsupportedCommandType = errors.New("archive run command type is unsupported")
)

type StageKind string

const (
	OrganizationStage  StageKind = "ORGANIZATION"
	TranscriptionStage StageKind = "TRANSCRIPTION"
)

func (kind StageKind) Valid() bool {
	return kind == OrganizationStage || kind == TranscriptionStage
}

// StageOwnership records that both top-level stages belong to this
// ArchiveRun. It intentionally carries no gate, Job or completion semantics;
// those contracts are decided independently.
type StageOwnership struct {
	Kind StageKind `json:"kind"`
}

type State struct {
	ClassificationPolicy    uint32                               `json:"classification_policy,omitempty"`
	ClassificationContexts  map[string]organization.EventContext `json:"classification_contexts,omitempty"`
	ID                      string                               `json:"id"`
	Revision                uint64                               `json:"revision"`
	WorkerID                string                               `json:"worker_id"`
	SourceStorageID         string                               `json:"source_storage_id"`
	SourceVolumeID          string                               `json:"source_volume_id"`
	SourcePhysicalDeviceID  string                               `json:"source_physical_device_id"`
	ArchiveStorageID        string                               `json:"archive_storage_id"`
	StartIdempotencyKey     string                               `json:"start_idempotency_key"`
	FolderDiscoveryRevision uint64                               `json:"folder_discovery_revision"`
	FolderIDs               []string                             `json:"folder_ids"`
	SelectedBy              command.Actor                        `json:"selected_by"`
	StageOwnerships         []StageOwnership                     `json:"stage_ownerships"`
	CreatedAt               time.Time                            `json:"created_at"`
}

type CreateParameters struct {
	ClassificationPolicy    uint32                               `json:"classification_policy,omitempty"`
	ClassificationContexts  map[string]organization.EventContext `json:"classification_contexts,omitempty"`
	WorkerID                string                               `json:"worker_id"`
	SourceStorageID         string                               `json:"source_storage_id"`
	SourceVolumeID          string                               `json:"source_volume_id"`
	SourcePhysicalDeviceID  string                               `json:"source_physical_device_id"`
	ArchiveStorageID        string                               `json:"archive_storage_id"`
	StartIdempotencyKey     string                               `json:"start_idempotency_key"`
	FolderDiscoveryRevision uint64                               `json:"folder_discovery_revision"`
	FolderIDs               []string                             `json:"folder_ids"`
	SelectedBy              command.Actor                        `json:"selected_by"`
}

type Handler struct {
	now func() time.Time
}

func NewHandler(now func() time.Time) (*Handler, error) {
	if now == nil {
		return nil, ErrInvalidArchiveRun
	}
	return &Handler{now: now}, nil
}

func (handler *Handler) Handle(envelope command.Envelope, snapshot command.Snapshot) (command.Change, error) {
	if envelope.CommandType != CreateCommand {
		return command.Change{}, command.DeterministicFailure("UNSUPPORTED_ARCHIVE_RUN_COMMAND", ErrUnsupportedCommandType)
	}
	if snapshot.Revision != 0 || len(snapshot.State) != 0 {
		return command.Change{}, command.DeterministicFailure("ARCHIVE_RUN_ALREADY_EXISTS", command.ErrRevisionConflict)
	}

	var parameters CreateParameters
	if err := decodeParameters(envelope.Parameters, &parameters); err != nil {
		return command.Change{}, command.DeterministicFailure("INVALID_ARCHIVE_RUN", ErrInvalidArchiveRun)
	}
	parameters.WorkerID = strings.TrimSpace(parameters.WorkerID)
	parameters.SourceStorageID = strings.TrimSpace(parameters.SourceStorageID)
	parameters.SourceVolumeID = strings.TrimSpace(parameters.SourceVolumeID)
	parameters.SourcePhysicalDeviceID = strings.TrimSpace(parameters.SourcePhysicalDeviceID)
	parameters.ArchiveStorageID = strings.TrimSpace(parameters.ArchiveStorageID)
	parameters.StartIdempotencyKey = strings.TrimSpace(parameters.StartIdempotencyKey)
	if parameters.StartIdempotencyKey == "" {
		parameters.StartIdempotencyKey = strings.TrimSpace(envelope.IdempotencyKey)
	}
	parameters.SelectedBy.ID = strings.TrimSpace(parameters.SelectedBy.ID)
	if parameters.WorkerID == "" || parameters.SourceStorageID == "" || parameters.SourceVolumeID == "" ||
		parameters.SourcePhysicalDeviceID == "" || parameters.ArchiveStorageID == "" || parameters.StartIdempotencyKey == "" ||
		!validSelectionActor(parameters.SelectedBy) {
		return command.Change{}, command.DeterministicFailure("INVALID_ARCHIVE_RUN", ErrInvalidArchiveRun)
	}

	folderScope, err := organization.FixFolderScope(organization.FixFolderScopeRequest{
		DiscoveryRevision:   parameters.FolderDiscoveryRevision,
		DiscoveredFolderIDs: parameters.FolderIDs,
		SelectAll:           true,
	})
	if err != nil {
		return command.Change{}, command.DeterministicFailure("INVALID_ARCHIVE_RUN", ErrInvalidArchiveRun)
	}

	if parameters.ClassificationPolicy != 0 && (parameters.ClassificationPolicy < organization.ContextPolicyVersion || parameters.ClassificationPolicy > organization.DrivePolicyVersion || len(parameters.ClassificationContexts) != len(folderScope.FolderIDs())) {
		return command.Change{}, command.DeterministicFailure("INVALID_ARCHIVE_RUN", ErrInvalidArchiveRun)
	}
	for folder, c := range parameters.ClassificationContexts {
		if !folderScope.Includes(folder) || c.InputRevision != parameters.FolderDiscoveryRevision {
			return command.Change{}, command.DeterministicFailure("INVALID_ARCHIVE_RUN", ErrInvalidArchiveRun)
		}
	}
	state := State{
		ClassificationPolicy: parameters.ClassificationPolicy, ClassificationContexts: parameters.ClassificationContexts,
		ID:                      envelope.TargetID,
		Revision:                1,
		WorkerID:                parameters.WorkerID,
		SourceStorageID:         parameters.SourceStorageID,
		SourceVolumeID:          parameters.SourceVolumeID,
		SourcePhysicalDeviceID:  parameters.SourcePhysicalDeviceID,
		ArchiveStorageID:        parameters.ArchiveStorageID,
		StartIdempotencyKey:     parameters.StartIdempotencyKey,
		FolderDiscoveryRevision: folderScope.DiscoveryRevision(),
		FolderIDs:               folderScope.FolderIDs(),
		SelectedBy:              parameters.SelectedBy,
		StageOwnerships: []StageOwnership{
			{Kind: OrganizationStage},
			{Kind: TranscriptionStage},
		},
		CreatedAt: handler.now().UTC(),
	}
	return stateChange(envelope, state)
}

func Decode(snapshot command.Snapshot) (State, error) {
	var state State
	if snapshot.TargetID == "" || snapshot.Revision == 0 || len(snapshot.State) == 0 ||
		json.Unmarshal(snapshot.State, &state) != nil || state.ID != snapshot.TargetID || state.Revision != snapshot.Revision ||
		strings.TrimSpace(state.WorkerID) == "" || strings.TrimSpace(state.SourceStorageID) == "" ||
		strings.TrimSpace(state.SourceVolumeID) == "" || strings.TrimSpace(state.SourcePhysicalDeviceID) == "" ||
		strings.TrimSpace(state.ArchiveStorageID) == "" || strings.TrimSpace(state.StartIdempotencyKey) == "" || !validSelectionActor(state.SelectedBy) ||
		state.CreatedAt.IsZero() || !validStageOwnerships(state.StageOwnerships) {
		return State{}, ErrInvalidArchiveRun
	}
	if _, err := state.FolderScope(); err != nil {
		return State{}, ErrInvalidArchiveRun
	}
	return state, nil
}

func validSelectionActor(actor command.Actor) bool {
	return (actor.Type == command.HumanActor || actor.Type == command.ArchivistActor) && strings.TrimSpace(actor.ID) != ""
}

func (state State) FolderScope() (organization.FolderScope, error) {
	return organization.FixFolderScope(organization.FixFolderScopeRequest{
		DiscoveryRevision:   state.FolderDiscoveryRevision,
		DiscoveredFolderIDs: state.FolderIDs,
		SelectAll:           true,
	})
}

func validStageOwnerships(ownerships []StageOwnership) bool {
	return len(ownerships) == 2 && ownerships[0].Kind == OrganizationStage && ownerships[1].Kind == TranscriptionStage
}

func stateChange(envelope command.Envelope, state State) (command.Change, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return command.Change{}, err
	}
	return command.Change{
		Revision: state.Revision,
		State:    encoded,
		Value:    state,
		Events: []command.Event{{
			EventID:   envelope.CommandID + ":event",
			EventType: "ArchiveRunCreated",
			Payload: map[string]any{
				"worker_id":                 state.WorkerID,
				"source_storage_id":         state.SourceStorageID,
				"source_volume_id":          state.SourceVolumeID,
				"archive_storage_id":        state.ArchiveStorageID,
				"start_idempotency_key":     state.StartIdempotencyKey,
				"folder_discovery_revision": state.FolderDiscoveryRevision,
				"folder_count":              len(state.FolderIDs),
				"selected_by":               state.SelectedBy.ID,
			},
		}},
	}, nil
}

func decodeParameters(source map[string]any, target any) error {
	payload, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}
