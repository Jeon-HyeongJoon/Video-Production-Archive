package controlruntime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/sqlitecommand"
	"videoarchive/internal/adapter/storageinventory"
	"videoarchive/internal/application/archivistloop"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/archivistruntime"
)

func incidentCall(t *testing.T, rt *controlruntime.Runtime, token uint64, id, tool, input string) *httptest.ResponseRecorder {
	t.Helper()
	call := archivistloop.NewToolEnvelope("archive-archivist", archivistloop.RuntimeContext{SessionID: "SESSION-INCIDENT", TurnID: "TURN-INCIDENT", FencingToken: token}, id, tool, json.RawMessage(input))
	return sendIncidentCall(t, rt, call)
}

func sendIncidentCall(t *testing.T, rt *controlruntime.Runtime, call archivistloop.ToolEnvelope) *httptest.ResponseRecorder {
	t.Helper()
	wire, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	return mcpRequest(rt.MCPHandler(), http.MethodPost, "/mcp/v1/call", string(wire), "runtime-service-secret")
}

// Requirement: MCP-019
func TestResumedArchivistJobCallReturnsOriginalReceipt(t *testing.T) {
	cfg := runtimeTestConfiguration(t)
	now := time.Now().UTC()
	open := func() *controlruntime.Runtime {
		rt, err := controlruntime.OpenWithDependencies(context.Background(), cfg, "incident", controlruntime.Dependencies{Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	const tool = "archive.jobs.create.v1"
	const input = `{"job_id":"JOB-INCIDENT","template_name":"occasion-recording","target_title":"가상 작업","reason":"재개 시험"}`
	rt := open()
	setupRunningArchivistTurn(t, rt, "SESSION-INCIDENT", "TURN-INCIDENT", "REQUEST-INCIDENT")
	first := incidentCall(t, rt, 1, "create", tool, input)
	proposal := incidentCall(t, rt, 1, "select", "archive.run.start.v1", `{"archive_run_id":"RUN-UNSELECTED"}`)
	rt.Close()
	if first.Code != 200 || proposal.Code != 202 {
		t.Fatalf("job=%d %s card=%d %s", first.Code, first.Body, proposal.Code, proposal.Body)
	}
	now = now.Add(cfg.ArchivistTurnLeaseTTL + time.Second)
	rt = open()
	defer rt.Close()
	for _, path := range []string{"/archivist/v1/heartbeat", "/archivist/v1/turns/claim"} {
		r := archivistHostRequest(rt.Handler(), http.MethodPost, path, "", "runtime-service-secret")
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body)
		}
	}
	if stale := incidentCall(t, rt, 1, "create", tool, input); stale.Code != 409 {
		t.Fatalf("old lease: %d %s", stale.Code, stale.Body)
	}
	start := archivistHostRequest(rt.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-INCIDENT/events", `{"fencing_token":2,"cursor":3,"request_id":"REQUEST-INCIDENT","kind":"TURN_STARTED"}`, "runtime-service-secret")
	if start.Code != 200 {
		t.Fatalf("resume: %d %s", start.Code, start.Body)
	}
	repeat := incidentCall(t, rt, 2, "create", tool, input)
	if repeat.Code != 200 || first.Body.String() != repeat.Body.String() {
		t.Fatalf("first=%s resumed=%d %s", first.Body, repeat.Code, repeat.Body)
	}
	changed := archivistloop.NewToolEnvelope("archive-archivist", archivistloop.RuntimeContext{SessionID: "SESSION-INCIDENT", TurnID: "TURN-INCIDENT", FencingToken: 2}, "create", tool, json.RawMessage(input))
	changed.Input = json.RawMessage(strings.Replace(input, "가상 작업", "변경된 작업", 1))
	if mismatch := sendIncidentCall(t, rt, changed); mismatch.Code != http.StatusConflict {
		t.Fatalf("changed replay: %d %s", mismatch.Code, mismatch.Body)
	}
	var card struct {
		Data struct {
			ID string `json:"action_card_id"`
		}
	}
	if err := json.Unmarshal(proposal.Body.Bytes(), &card); err != nil || card.Data.ID == "" {
		t.Fatal("no proposed card", err)
	}
	cookie, token := runtimeSession(t, rt.Handler())
	r := runtimeRequest(rt.Handler(), http.MethodPost, "/api/v1/action-cards/"+card.Data.ID+"/reject", `{"reason":"선택 보류"}`, cookie, token, "http://127.0.0.1:8080")
	if r.Code != 200 {
		t.Fatalf("reject: %d %s", r.Code, r.Body)
	}
	replayedCard := incidentCall(t, rt, 2, "select", "archive.run.start.v1", `{"archive_run_id":"RUN-UNSELECTED"}`)
	if replayedCard.Code != 202 || !strings.Contains(replayedCard.Body.String(), `"state":"REJECTED"`) {
		t.Fatalf("decision on replay: %d %s", replayedCard.Code, replayedCard.Body)
	}
	db := openLedger(t, cfg.DatabasePath)
	defer db.Close()
	for _, query := range []string{
		`SELECT COUNT(*) FROM command_records WHERE target_id='JOB-INCIDENT' AND json_extract(payload,'$.accepted')=1`,
		`SELECT COUNT(*) FROM archivist_tool_calls WHERE turn_id='TURN-INCIDENT' AND json_extract(payload,'$.State')='COMPLETED'`,
		`SELECT COUNT(*) FROM idempotency_receipts WHERE json_extract(payload,'$.Result.TargetID')='JOB-INCIDENT'`,
		`SELECT COUNT(*) FROM archivist_action_cards WHERE turn_id='TURN-INCIDENT'`,
	} {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: count=%d err=%v", query, n, err)
		}
	}
}

// Requirement: MCP-018
func TestBoundArchiveRunDiagnosisCitesOutcomeAndRecordedAttempt(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "source"), filepath.Join(tmp, "archive")
	const folder = "370105-행사_촬영-본당"
	for _, dir := range []string{filepath.Join(src, folder), dst} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"A 녹화.mov", "B 녹화.mov", "C 녹화.mov"} {
		if err := os.WriteFile(filepath.Join(src, folder, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := registeredMediaConfiguration(t, tmp, src, dst, exe)
	host := runtimeTestConfiguration(t)
	cfg.ArchivistHosts, cfg.RuntimeHeartbeatTTL, cfg.ArchivistTurnLeaseTTL = host.ArchivistHosts, host.RuntimeHeartbeatTTL, host.ArchivistTurnLeaseTTL
	now := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	rt, err := controlruntime.OpenWithDependencies(context.Background(), cfg, "incident", controlruntime.Dependencies{Secrets: runtimeSecret("runtime-service-secret"), Now: func() time.Time { return now }, ArchiveIntakeAnalyzers: driveAnalyzerFactoryFixture{now: now}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	beat := func(seq int) {
		t.Helper()
		r := workerHeartbeatRequest(rt.Handler(), fmt.Sprintf(`{"worker_id":"worker-main","snapshot_id":%d,"observed_at":"2026-11-23T03:00:00Z","idle_stable_millis":60000,"available_memory_bytes":34359738368,"available_vram_bytes":8589934592,"scratch_free_bytes":536870912000,"accepting_work":true,"accessible_storage_ids":["source-main","archive-main"]}`, seq), "runtime-service-secret")
		if r.Code != 200 {
			t.Fatal(r.Body)
		}
	}
	beat(1)
	root, err := archive.OpenReadOnlyStorageRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := storageinventory.ScanSource(context.Background(), root)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	cookie, token := runtimeSession(t, rt.Handler())
	start := runtimeRequest(rt.Handler(), http.MethodPost, "/api/v1/archive-intake/runs", fmt.Sprintf(`{"archive_run_id":"RUN-INCIDENT","idempotency_key":"start-incident","session_id":"INTAKE-INCIDENT","source_volume_id":"source-volume","expected_candidate_revision":1,"archive_storage_id":"archive-main","input_revision":%d,"folder_ids":[%q]}`, inventory.DiscoveryRevision, folder), cookie, token, "http://127.0.0.1:8080")
	if start.Code != 201 {
		t.Fatal(start.Body)
	}
	beat(2)
	claim := workerProtocolRequest(rt.Handler(), "/worker/v1/steps/claim", "", "runtime-service-secret")
	var assignment struct{ Data workerclient.Assignment }
	if err := json.Unmarshal(claim.Body.Bytes(), &assignment); err != nil || assignment.Data.StepID != "render" {
		t.Fatalf("claim=%s err=%v", claim.Body, err)
	}
	a := assignment.Data
	worker := func(endpoint, fields string) {
		t.Helper()
		body := fmt.Sprintf(`{"job_id":%q,"step_id":%q,"fencing_token":%d,%s}`, a.JobID, a.StepID, a.FencingToken, fields)
		r := workerProtocolRequest(rt.Handler(), "/worker/v1/steps/"+endpoint, body, "runtime-service-secret")
		if r.Code != 200 {
			t.Fatalf("%s: %s", endpoint, r.Body)
		}
	}
	worker("progress", `"report_sequence":1,"update":{"operation":"RENDER_RECORDING","status":"OUTPUT_POSITION","input_index":2,"input_count":3,"output_millis":1518,"progress":0.02}`)
	worker("result", `"success":false,"failure_class":"INPUT","error_class":"SOURCE_DECODE_FAILED","cause_code":"SOURCE_DECODE_FAILED","error_summary":"두 번째 원본 디코딩 실패","process_exit_code":69,"log_reference":"evidence://process-log/PLOG-INCIDENT","process_log":{"id":"PLOG-INCIDENT","reference":"evidence://process-log/PLOG-INCIDENT","state":"AVAILABLE","digest":"sha256:incident","captured_bytes":80,"total_bytes":80,"sanitized_tail":"decoder rejected invalid input packet"}`)
	work := runtimeRequest(rt.Handler(), http.MethodGet, "/api/v1/work/RUN-INCIDENT", "", nil, "", "")
	var truth struct {
		Data struct {
			Summary struct {
				State       string
				ResultCount *int `json:"result_count"`
				Revision    uint64
			}
		}
	}
	if err := json.Unmarshal(work.Body.Bytes(), &truth); err != nil || truth.Data.Summary.State != "FAILED" || truth.Data.Summary.ResultCount == nil || *truth.Data.Summary.ResultCount != 0 {
		t.Fatalf("work truth %s err=%v", work.Body, err)
	}
	store, err := sqlitecommand.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Load("RUN-INCIDENT")
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	const question = "이 작업은 몇 건 보관됐고 어느 입력에서 멈췄나요? 마지막 출력 위치와 원인 근거만 알려 주세요."
	turn := fmt.Sprintf(`{"turn_id":"TURN-INCIDENT","request_id":"REQUEST-INCIDENT","prompt":%q,"work_reference":{"kind":"ARCHIVE_RUN","work_id":"RUN-INCIDENT","revision":%d}}`, question, run.Revision)
	startArchivistTestTurn(t, rt, "SESSION-INCIDENT", "TURN-INCIDENT", "REQUEST-INCIDENT", turn)
	started := time.Now()
	d := incidentCall(t, rt, 1, "diagnostic", "archive.work.diagnostics.get.v1", `{}`)
	var diagnostic struct {
		Data struct {
			workdiagnostics.Snapshot
			WorkState   string `json:"work_state"`
			ResultCount *int   `json:"result_count"`
		}
	}
	if err := json.Unmarshal(d.Body.Bytes(), &diagnostic); d.Code != 200 || err != nil || len(diagnostic.Data.Jobs) != 1 {
		t.Fatalf("diag=%s err=%v", d.Body, err)
	}
	job := diagnostic.Data.Jobs[0]
	if len(job.FailureEvidence) != 1 || len(job.ProgressEvidence) != 1 {
		t.Fatalf("evidence=%+v", job)
	}
	failure, position := job.FailureEvidence[0], job.ProgressEvidence[0]
	detail := incidentCall(t, rt, 1, "detail", "archive.work.failure_evidence.get.v1", fmt.Sprintf(`{"evidence_id":%q}`, failure.EvidenceID))
	var log struct{ Data workdiagnostics.FailureDetail }
	if err := json.Unmarshal(detail.Body.Bytes(), &log); err != nil || detail.Code != 200 {
		t.Fatal(detail.Body, err)
	}
	// Render a deterministic Korean answer using only the two actual MCP receipts.
	state, count := diagnostic.Data.WorkState, "미확인"
	if diagnostic.Data.ResultCount != nil {
		count = fmt.Sprint(*diagnostic.Data.ResultCount)
	}
	outcomeID := ""
	for _, fact := range diagnostic.Data.Facts {
		if fact.Code == "ARCHIVE_RUN_OUTCOME" {
			outcomeID = fact.EvidenceID
		}
	}
	answer := fmt.Sprintf("%s %s, 보관 %s건 (%s). 원본 %d개 중 %d번째, 마지막 출력 %dms (%s). %s, 시도 %d, 로그 %s (%s).", diagnostic.Data.WorkID, state, count, outcomeID, position.Update.InputCount, position.Update.InputIndex, position.Update.OutputMillis, position.EvidenceID, log.Data.Summary, log.Data.WorkflowAttempt, log.Data.ProcessLog.State, log.Data.EvidenceID)
	response, err := archivistruntime.NewFinalResponse(answer, []string{diagnostic.Data.WorkID, position.EvidenceID, log.Data.EvidenceID})
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(map[string]any{"fencing_token": 1, "cursor": 3, "request_id": "REQUEST-INCIDENT", "kind": "TURN_COMPLETED", "final_response": response})
	if err != nil {
		t.Fatal(err)
	}
	completed := archivistHostRequest(rt.Handler(), http.MethodPost, "/archivist/v1/turns/TURN-INCIDENT/events", string(event), "runtime-service-secret")
	if completed.Code != 200 {
		t.Fatal(completed.Body)
	}
	db := openLedger(t, cfg.DatabasePath)
	defer db.Close()
	var calls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM archivist_tool_calls WHERE turn_id='TURN-INCIDENT' AND json_extract(payload,'$.State')='COMPLETED'`).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	got := runtimeRequest(rt.Handler(), http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if !strings.Contains(got.Body.String(), answer) {
		t.Fatalf("durable answer missing: %s", got.Body)
	}
	checks := []bool{diagnostic.Data.WorkID == "RUN-INCIDENT", state == truth.Data.Summary.State, count == fmt.Sprint(*truth.Data.Summary.ResultCount), strings.HasPrefix(outcomeID, "EVD-DIAGNOSTIC-"), job.JobID == a.JobID && log.Data.JobID == a.JobID, position.Update.InputIndex == 2 && position.Update.InputCount == 3, position.Update.OutputMillis == 1518 && position.FencingToken == failure.FencingToken, log.Data.CauseCode == "SOURCE_DECODE_FAILED" && log.Data.WorkflowAttempt == 1, log.Data.EvidenceID == failure.EvidenceID && log.Data.ProcessExitCode != nil && *log.Data.ProcessExitCode == 69, log.Data.ProcessLog.Reference == "evidence://process-log/PLOG-INCIDENT" && log.Data.ProcessLog.SanitizedTail == "decoder rejected invalid input packet", len(diagnostic.Data.RecoveryActions) == 0}
	matched := 0
	for _, ok := range checks {
		if ok {
			matched++
		}
	}
	t.Logf("evidence_agreement=%d/%d tool_calls=%d diagnostic_to_persisted_answer=%s answer=%s", matched, len(checks), calls, time.Since(started), answer)
	if matched != len(checks) {
		t.Fatalf("answer/receipt mismatch: %v", checks)
	}
}
