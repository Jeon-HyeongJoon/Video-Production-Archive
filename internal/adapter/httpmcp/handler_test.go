package httpmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archiveorganization"
	"videoarchive/internal/application/archivetranscription"
	"videoarchive/internal/application/archiveworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

// Requirement: MCP-001
func TestAuthenticatedMCPListsAndRoutesVersionedDriveBusinessCapabilities(t *testing.T) {
	handler, fixture := driveMCPFixture(t)
	tools := mcpTestRequest(handler, http.MethodGet, "/mcp/v1/tools", "", "secret")
	body := tools.Body.String()
	for _, tool := range []string{
		ToolDriveSelectionGet, ToolArchiveRunStatus, ToolOrganizationHoldsList, ToolOrganizationHoldGet,
		ToolOrganizationHoldClassify, ToolArchiveRunStart, ToolArchiveTranscriptionStart,
	} {
		if tools.Code != http.StatusOK || !strings.Contains(body, tool) {
			t.Fatalf("versioned drive capability %s is missing: %d %s", tool, tools.Code, body)
		}
	}
	for _, forbidden := range []string{"shell", "terminal", "arbitrary_path", "run_command"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("generic execution capability leaked into MCP: %s", body)
		}
	}
	status := archiveToolCall("CALL-STATUS", ToolArchiveRunStatus, `{}`)
	if response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", status, "secret"); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"workspace_drive_id":"VOLUME-1"`) {
		t.Fatalf("drive status capability did not route: %d %s", response.Code, response.Body.String())
	}
	start := archiveToolCall("CALL-START", ToolArchiveRunStart, `{"archive_run_id":"ARCHIVE-RUN-1","control_mode":"ARCHIVIST"}`)
	if response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", start, "secret"); response.Code != http.StatusOK {
		t.Fatalf("archive run start did not route: %d %s", response.Code, response.Body.String())
	}
	stt := archiveToolCall("CALL-STT", ToolArchiveTranscriptionStart, `{"control_mode":"ARCHIVIST"}`)
	if response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", stt, "secret"); response.Code != http.StatusOK {
		t.Fatalf("archive transcription start did not route: %d %s", response.Code, response.Body.String())
	}
	if fixture.runs.calls != 1 || fixture.transcriptions.calls != 1 || fixture.runs.request.ArchiveRunID != "ARCHIVE-RUN-1" ||
		fixture.transcriptions.request.ArchiveRunID != "ARCHIVE-RUN-1" {
		t.Fatalf("drive capabilities did not reach their application boundaries: %+v", fixture)
	}
	cards := []archivistcontrol.ActionCardProjection{}
	assistedRuns := &fixtureArchiveRuns{}
	assisted, err := New(Configuration{
		Archivists: fixtureArchivistRuntime{selection: archiveIntakeSelectionFixture(t), controlMode: workflow.AssistedMode, cards: &cards},
		Catalog:    fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{}, Drives: fixtureDrives{},
		ArchiveRuns: assistedRuns, ArchiveTranscriptions: &fixtureArchiveTranscriptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := mcpTestRequest(assisted, http.MethodPost, "/mcp/v1/call", start, "secret")
	if proposal.Code != http.StatusAccepted || assistedRuns.calls != 0 || len(cards) != 1 ||
		cards[0].CommandType != archiveorganization.StartActionCardCommand || cards[0].Parameters["archive_storage_id"] != "ARCHIVE-1" {
		t.Fatalf("stored ASSISTED mode did not produce a fixed-selection ActionCard: %d %s calls=%d cards=%+v", proposal.Code, proposal.Body.String(), assistedRuns.calls, cards)
	}
}

// Requirement: MCP-003
func TestDriveMCPKeepsSelectionAndDestructiveAuthorityOutsideArchivist(t *testing.T) {
	handler, fixture := driveMCPFixture(t)
	tools := mcpTestRequest(handler, http.MethodGet, "/mcp/v1/tools", "", "secret")
	body := strings.ToLower(tools.Body.String())
	if !strings.Contains(body, ToolOrganizationHoldClassify) {
		t.Fatalf("selected HOLD classification capability is missing: %s", body)
	}
	for _, forbidden := range []string{"storage.select", "hold.override", "format", "delete", "path.open"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("selection or destructive drive tool was listed: %s", body)
		}
		call := archiveToolCall("CALL-FORBIDDEN-"+strings.ReplaceAll(forbidden, ".", "-"), "archive.drive."+forbidden+".v1", `{}`)
		response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", call, "secret")
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "MCP_CAPABILITY_NOT_FOUND") {
			t.Fatalf("forbidden tool %s reached a call path: %d %s", forbidden, response.Code, response.Body.String())
		}
	}
	if fixture.runs.calls != 0 || fixture.transcriptions.calls != 0 {
		t.Fatalf("forbidden tools changed drive state: %+v", fixture)
	}
	manualRuns := &fixtureArchiveRuns{}
	manual, err := New(Configuration{
		Archivists: fixtureArchivistRuntime{selection: archiveIntakeSelectionFixture(t), controlMode: workflow.ManualMode},
		Catalog:    fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{}, Drives: fixtureDrives{},
		ArchiveRuns: manualRuns, ArchiveTranscriptions: &fixtureArchiveTranscriptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	denied := mcpTestRequest(manual, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-MANUAL", ToolArchiveRunStart, `{"archive_run_id":"ARCHIVE-RUN-1","control_mode":"ARCHIVIST"}`), "secret")
	if denied.Code != http.StatusForbidden || manualRuns.calls != 0 {
		t.Fatalf("message input bypassed stored MANUAL mode: %d %s calls=%d", denied.Code, denied.Body.String(), manualRuns.calls)
	}
}

// Requirement: MCP-004
func TestMCPListsSelectedHoldsAndReturnsSingleStructuredEvidenceWithoutStoragePaths(t *testing.T) {
	state := holdReviewDriveState(t)
	handler := holdReviewMCPHandler(t, state, workflow.ArchivistMode)

	list := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-HOLDS", ToolOrganizationHoldsList, `{}`), "secret")
	listBody := list.Body.String()
	if list.Code != http.StatusOK || !strings.Contains(listBody, `"folder":"recordings"`) ||
		strings.Contains(listBody, `"folder":"outside"`) || strings.Contains(listBody, `"folder":"ready"`) {
		t.Fatalf("HOLD list crossed the fixed selection or included a non-HOLD: %d %s", list.Code, listBody)
	}

	detail := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-HOLD-DETAIL", ToolOrganizationHoldGet, `{"folder":"recordings"}`), "secret")
	detailBody := detail.Body.String()
	for _, expected := range []string{`"decision_id":"DECISION-HOLD"`, `"input_revision":7`, `"relative_path":"recordings/a.mov"`,
		`"checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"codec":"h264"`, `"width":3840`, `"bit_rate":50000000`} {
		if detail.Code != http.StatusOK || !strings.Contains(detailBody, expected) {
			t.Fatalf("single HOLD evidence is incomplete, missing %s: %d %s", expected, detail.Code, detailBody)
		}
	}
	for _, forbidden := range []string{`C:\\source`, `/mnt/source`, `credential`, `storage_root`} {
		if strings.Contains(strings.ToLower(detailBody), strings.ToLower(forbidden)) {
			t.Fatalf("single HOLD evidence exposed a storage boundary: %s", detailBody)
		}
	}
	outside := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-HOLD-OUTSIDE", ToolOrganizationHoldGet, `{"folder":"outside"}`), "secret")
	if outside.Code != http.StatusNotFound || !strings.Contains(outside.Body.String(), "MCP_HOLD_NOT_FOUND") {
		t.Fatalf("unselected HOLD detail was exposed: %d %s", outside.Code, outside.Body.String())
	}
}

// Requirement: MCP-005
func TestMCPClassifiesOneSelectedCurrentHoldWithArchivistProvenance(t *testing.T) {
	state := holdReviewDriveState(t)
	handler := holdReviewMCPHandler(t, state, workflow.ArchivistMode)
	input := `{"folder":"recordings","expected_input_revision":7,"disposition":"RENDER_RECORDINGS","target_video_bitrate_kbps":20000,"reason":"two independent long recordings","basis":["ASSET-A","ASSET-B"]}`
	response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-CLASSIFY", ToolOrganizationHoldClassify, input), "secret")
	if response.Code != http.StatusOK || len(state.overrides) != 1 {
		t.Fatalf("current selected HOLD was not classified: %d %s overrides=%+v", response.Code, response.Body.String(), state.overrides)
	}
	request := state.overrides[0]
	decision := state.decisions["recordings"]
	if request.Actor.Type != vocabulary.ActorArchivist || request.Actor.ID != "archivist:SESSION-1" ||
		decision.Sequence != 2 || decision.Disposition != vocabulary.OrganizationRenderRecordings ||
		decision.DecidedByType != vocabulary.ActorArchivist || decision.DecidedBy != "archivist:SESSION-1" ||
		decision.DecisionReason != "two independent long recordings" || len(decision.DecisionBasis) != 2 {
		t.Fatalf("Archivist classification provenance or decision is wrong: request=%+v decision=%+v", request, decision)
	}

	stale := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-STALE", ToolOrganizationHoldClassify,
		`{"folder":"recordings","expected_input_revision":6,"disposition":"RENDER_RECORDINGS","target_video_bitrate_kbps":6000,"reason":"stale","basis":["ASSET-A"]}`), "secret")
	outside := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-OUTSIDE", ToolOrganizationHoldClassify,
		`{"folder":"outside","expected_input_revision":7,"disposition":"RENDER_RECORDINGS","target_video_bitrate_kbps":6000,"reason":"outside","basis":["ASSET-X"]}`), "secret")
	if stale.Code != http.StatusConflict || outside.Code != http.StatusConflict || len(state.overrides) != 1 {
		t.Fatalf("stale or unselected classification changed a HOLD: stale=%d outside=%d overrides=%+v", stale.Code, outside.Code, state.overrides)
	}
}

// Requirement: COL-003
func TestDriveToolUsesTurnSelectionIDsAndRevisionInsteadOfConflictingMessageInput(t *testing.T) {
	handler, fixture := driveMCPFixture(t)
	call := archiveToolCall("CALL-FIXED-SELECTION", ToolArchiveRunStart,
		`{"archive_run_id":"ARCHIVE-RUN-1","control_mode":"ARCHIVIST","worker_id":"WORKER-EVIL","archive_storage_id":"ARCHIVE-EVIL","folder_ids":["other"]}`)
	response := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", call, "secret")
	request := fixture.runs.request
	if response.Code != http.StatusOK || request.ExpectedWorkerID != "WORKER-1" || request.ExpectedSourceStorageID != "SOURCE-1" ||
		request.ExpectedWorkspaceDriveID != "VOLUME-1" || request.ArchiveStorageID != "ARCHIVE-1" ||
		request.InputRevision != 7 || len(request.FolderIDs) != 1 || request.FolderIDs[0] != "recordings" ||
		request.SelectedBy.Type != vocabulary.ActorHuman || request.SelectedBy.ID != "operator-1" || request.Actor.Type != vocabulary.ActorArchivist {
		t.Fatalf("message input changed the fixed Turn selection: response=%d %s request=%+v", response.Code, response.Body.String(), request)
	}
	var cards []archivistcontrol.ActionCardProjection
	missing, err := New(Configuration{
		Archivists: fixtureArchivistRuntime{selectionErr: true, cards: &cards}, Catalog: fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{},
		Drives: fixtureDrives{}, ArchiveRuns: &fixtureArchiveRuns{}, ArchiveTranscriptions: &fixtureArchiveTranscriptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	missingResponse := mcpTestRequest(missing, http.MethodPost, "/mcp/v1/call", archiveToolCall("CALL-MISSING", ToolArchiveRunStart, `{"archive_run_id":"ARCHIVE-RUN-2","control_mode":"ARCHIVIST"}`), "secret")
	if missingResponse.Code != http.StatusAccepted || len(cards) != 1 || cards[0].CommandType != "archive_intake.selection.required" ||
		!strings.Contains(missingResponse.Body.String(), "ACTION-SELECT-CALL-MISSING") {
		t.Fatalf("missing selection did not end in a selection ActionCard: %d %s cards=%+v", missingResponse.Code, missingResponse.Body.String(), cards)
	}
}

type driveMCPState struct {
	runs           *fixtureArchiveRuns
	transcriptions *fixtureArchiveTranscriptions
}

func driveMCPFixture(t *testing.T) (*Handler, driveMCPState) {
	t.Helper()
	selection := archiveIntakeSelectionFixture(t)
	runs := &fixtureArchiveRuns{}
	transcriptions := &fixtureArchiveTranscriptions{}
	handler, err := New(Configuration{
		Archivists: fixtureArchivistRuntime{selection: selection}, Catalog: fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{},
		Drives: fixtureDrives{}, ArchiveRuns: runs, ArchiveTranscriptions: transcriptions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, driveMCPState{runs: runs, transcriptions: transcriptions}
}

func archiveIntakeSelectionFixture(t *testing.T) archivistruntime.DriveSelection {
	t.Helper()
	selection, err := archivistruntime.NewDriveSelection(archivistruntime.DriveSelection{
		ArchiveRunID: "ARCHIVE-RUN-1", ArchiveRunRevision: 1, WorkerID: "WORKER-1", WorkspaceDriveID: "VOLUME-1",
		IntakeSessionID: "INTAKE-1", SourceVolumeID: "VOLUME-1", SourceStorageID: "SOURCE-1",
		ArchiveStorageID: "ARCHIVE-1", CandidateRevision: 4, FolderDiscoveryRevision: 7,
		FolderIDs: []string{"recordings"}, SelectedBy: "operator-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

type fixtureArchivistRuntime struct {
	selection    archivistruntime.DriveSelection
	selectionErr bool
	cards        *[]archivistcontrol.ActionCardProjection
	controlMode  workflow.ControlMode
}

func (fixtureArchivistRuntime) Authenticate(hostID, credential string) error {
	if hostID != "HOST-1" || credential != "secret" {
		return errors.New("unauthorized")
	}
	return nil
}
func (fixtureArchivistRuntime) ValidateToolContext(context.Context, string, string, string, uint64) error {
	return nil
}
func (fixtureArchivistRuntime) RecordToolCall(context.Context, string, string, string, string, string, string) error {
	return nil
}
func (fixture fixtureArchivistRuntime) RecordActionCard(_ context.Context, _ string, card archivistcontrol.ActionCardProjection) error {
	if fixture.cards != nil {
		*fixture.cards = append(*fixture.cards, card)
	}
	return nil
}
func (fixture fixtureArchivistRuntime) TurnDriveSelection(context.Context, string, string) (archivistruntime.DriveSelection, error) {
	if fixture.selectionErr {
		return archivistruntime.DriveSelection{}, archivistcontrol.ErrInvalidRequest
	}
	return fixture.selection, nil
}

func (fixture fixtureArchivistRuntime) SessionControlMode(context.Context, string) (workflow.ControlMode, error) {
	if fixture.controlMode == "" {
		return workflow.ArchivistMode, nil
	}
	return fixture.controlMode, nil
}

func (fixture fixtureArchivistRuntime) BindArchiveRun(context.Context, string, string, string, uint64) error {
	return nil
}

type fixtureCatalog struct{}

func (fixtureCatalog) Search(context.Context, catalog.SearchQuery) ([]catalog.SearchResult, error) {
	return nil, nil
}

type fixtureArchiveWorkspace struct{}

func (fixtureArchiveWorkspace) Detail(context.Context, string) (archiveworkspace.JobView, error) {
	return archiveworkspace.JobView{}, archiveworkspace.ErrJobNotFound
}

type fixtureCommands struct{}

func (fixtureCommands) Execute(context.Context, command.Envelope) (command.Result, error) {
	return command.Result{}, nil
}

type fixtureDrives struct {
	state *holdReviewState
}

func (fixture fixtureDrives) Projection(context.Context) ([]archiveintakeworkspace.DriveView, error) {
	if fixture.state != nil {
		return []archiveintakeworkspace.DriveView{fixture.state.view}, nil
	}
	return []archiveintakeworkspace.DriveView{{
		WorkspaceDriveID: "VOLUME-1", ArchiveRunID: "ARCHIVE-RUN-1", WorkerID: "WORKER-1", State: "READY",
	}}, nil
}

func (fixture fixtureDrives) Override(_ context.Context, request archiveintakeworkspace.OverrideCommand) (organization.OrganizationDecision, error) {
	if fixture.state == nil {
		return organization.OrganizationDecision{}, organization.ErrInvalidDecision
	}
	latest, found := fixture.state.decisions[request.Folder]
	if !found || request.WorkspaceDriveID != fixture.state.view.WorkspaceDriveID {
		return organization.OrganizationDecision{}, organization.ErrInvalidDecision
	}
	history, err := organization.NewDecisionHistory(latest)
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	decision, err := history.RecordOverride(organization.OverrideRequest{
		DecisionID: request.DecisionID, ExpectedInputRevision: request.ExpectedInputRevision,
		Disposition: request.Disposition, TargetVideoBitRateKbps: request.TargetVideoBitRateKbps,
		DecidedByType: request.Actor.Type, DecidedBy: request.Actor.ID,
		Reason: request.Reason, Basis: request.Basis, DecidedAt: request.OccurredAt,
	})
	if err != nil {
		return organization.OrganizationDecision{}, err
	}
	fixture.state.decisions[request.Folder] = decision
	fixture.state.overrides = append(fixture.state.overrides, request)
	for index := range fixture.state.view.Folders {
		folder := &fixture.state.view.Folders[index]
		if folder.Folder == request.Folder {
			folder.DecisionID = decision.ID
			folder.DecisionSequence = decision.Sequence
			folder.Disposition = decision.Disposition
			folder.ProfileReference = decision.ProfileReference
			folder.HoldReasons = append([]vocabulary.OrganizationHoldReason(nil), decision.HoldReasons...)
		}
	}
	return decision, nil
}

type holdReviewState struct {
	view      archiveintakeworkspace.DriveView
	decisions map[string]organization.OrganizationDecision
	overrides []archiveintakeworkspace.OverrideCommand
}

func holdReviewMCPHandler(t *testing.T, state *holdReviewState, mode workflow.ControlMode) *Handler {
	t.Helper()
	handler, err := New(Configuration{
		Archivists: fixtureArchivistRuntime{selection: archiveIntakeSelectionFixture(t), controlMode: mode},
		Catalog:    fixtureCatalog{}, ArchiveWorkspace: fixtureArchiveWorkspace{}, Commands: fixtureCommands{},
		Drives: fixtureDrives{state: state}, ArchiveRuns: &fixtureArchiveRuns{}, ArchiveTranscriptions: &fixtureArchiveTranscriptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func holdReviewDriveState(t *testing.T) *holdReviewState {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	input := func(assetID, relativePath, checksum string) organization.Input {
		return organization.Input{
			OriginalAssetID: assetID, RelativePath: relativePath,
			Inspection: mediaanalysis.MediaInspection{
				ID: "INSPECTION-" + assetID, OriginalAssetID: assetID, OriginalChecksum: checksum,
				Containers: []string{"mov"}, DurationMillis: 600_000, SizeBytes: 4_000_000_000, BitRate: 50_192_000,
				VideoStreams: []mediaanalysis.VideoStream{{
					Index: 0, Codec: "h264", Width: 3840, Height: 2160,
					FrameRate: mediaanalysis.Rational{Numerator: 30, Denominator: 1}, BitRate: 50_000_000,
				}},
				AudioStreams:     []mediaanalysis.AudioStream{{Index: 1, Codec: "aac", SampleRateHz: 48_000, Channels: 2}},
				ProcessorVersion: "ffprobe-8.0", ObservedBy: "WORKER-1", InspectedAt: now,
			},
		}
	}
	inputs := []organization.Input{
		input("ASSET-A", "recordings/a.mov", strings.Repeat("a", 64)),
		input("ASSET-B", "recordings/b.mov", strings.Repeat("b", 64)),
	}
	hold := organization.OrganizationDecision{
		ID: "DECISION-HOLD", Sequence: 1, WorkspaceDriveID: "VOLUME-1", Folder: "recordings", InputRevision: 7,
		Disposition: vocabulary.OrganizationHold, Inputs: inputs,
		HoldReasons:   []vocabulary.OrganizationHoldReason{vocabulary.OrganizationHoldAmbiguousMultipleVideos},
		DecidedByType: vocabulary.ActorSystem, DecidedBy: "SYSTEM", DecidedAt: now,
	}
	outside := hold
	outside.ID, outside.Folder = "DECISION-OUTSIDE", "outside"
	ready := hold
	ready.ID, ready.Folder, ready.Disposition, ready.HoldReasons = "DECISION-READY", "ready", vocabulary.OrganizationAsIs, nil
	assets := make([]archiveintakeworkspace.ClassificationAssetView, 0, len(inputs))
	for _, item := range inputs {
		assets = append(assets, archiveintakeworkspace.ClassificationAssetView{
			OriginalAssetID: item.OriginalAssetID, RelativePath: item.RelativePath,
			Checksum: item.Inspection.OriginalChecksum, DurationMillis: item.Inspection.DurationMillis,
			SizeBytes: item.Inspection.SizeBytes, BitRate: item.Inspection.BitRate,
			Containers:   append([]string(nil), item.Inspection.Containers...),
			VideoStreams: append([]mediaanalysis.VideoStream(nil), item.Inspection.VideoStreams...),
			AudioStreams: append([]mediaanalysis.AudioStream(nil), item.Inspection.AudioStreams...),
		})
	}
	folderView := func(decision organization.OrganizationDecision) archiveintakeworkspace.FolderView {
		return archiveintakeworkspace.FolderView{
			Folder: decision.Folder, DecisionID: decision.ID, DecisionSequence: decision.Sequence,
			InputRevision: decision.InputRevision, Disposition: decision.Disposition,
			HoldReasons: append([]vocabulary.OrganizationHoldReason(nil), decision.HoldReasons...),
			VideoCount:  len(decision.Inputs), ClassificationAssets: append([]archiveintakeworkspace.ClassificationAssetView(nil), assets...),
		}
	}
	return &holdReviewState{
		view: archiveintakeworkspace.DriveView{
			WorkspaceDriveID: "VOLUME-1", ArchiveRunID: "ARCHIVE-RUN-1", ArchiveRunRevision: 1,
			WorkerID: "WORKER-1", InputRevision: 7, Folders: []archiveintakeworkspace.FolderView{
				folderView(hold), folderView(outside), folderView(ready),
			},
		},
		decisions: map[string]organization.OrganizationDecision{"recordings": hold, "outside": outside, "ready": ready},
	}
}

type fixtureArchiveRuns struct {
	calls   int
	request archiveorganization.StartRequest
}

func (fixture *fixtureArchiveRuns) Start(_ context.Context, request archiveorganization.StartRequest) (archiveorganization.Result, error) {
	fixture.calls++
	fixture.request = request
	return archiveorganization.Result{Receipt: organization.ArchiveOrganizationReceipt{
		ID: request.ArchiveRunID, Folders: []organization.CoordinatedFolder{{Folder: "recordings"}},
	}}, nil
}

type fixtureArchiveTranscriptions struct {
	calls   int
	request archivetranscription.StartRequest
}

func (fixture *fixtureArchiveTranscriptions) Start(_ context.Context, request archivetranscription.StartRequest) (archivetranscription.Result, error) {
	fixture.calls++
	fixture.request = request
	return archivetranscription.Result{ArchiveRunID: request.ArchiveRunID, JobIDs: []string{"STT-JOB-1"}, EligibleCount: 1}, nil
}

func archiveToolCall(callID, tool, input string) string {
	payload := map[string]any{
		"session_id": "SESSION-1", "turn_id": "TURN-1", "fencing_token": uint64(1),
		"tool_call_id": callID, "command_id": "COMMAND-" + callID,
		"idempotency_key": "IDEMPOTENCY-" + callID, "input_digest": "sha256:" + callID,
		"tool": tool,
	}
	var raw json.RawMessage = []byte(input)
	payload["input"] = raw
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func mcpTestRequest(handler http.Handler, method, target, body, credential string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Archivist-Host-ID", "HOST-1")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}
