//go:build windows

package controlruntime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"videoarchive/internal/adapter/controlruntime"
	"videoarchive/internal/adapter/ossecret"
)

// Requirement: LRH-011
func TestWindowsArchivistHostResumesCollaborationTurnWithoutDuplicateEffects(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_ARCHIVIST_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_ARCHIVIST_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	hostExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_ARCHIVIST_HOST_PATH")
	cliExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_ARCHIVIST_FIXTURE_PATH")
	directory := t.TempDir()
	holdPath := filepath.Join(directory, "complete.turn")
	tracePath := filepath.Join(directory, "runtime-trace.log")
	t.Cleanup(func() {
		for _, processID := range tracedProcessIDs(tracePath) {
			if process, err := os.FindProcess(processID); err == nil {
				_ = process.Kill()
			}
		}
	})
	credentialReference := "dpapi://" + filepath.Join(directory, "runtime-service.dpapi")
	credential := "runtime-acceptance-secret"
	if err := ossecret.ProvisionDPAPI(credentialReference, credential); err != nil {
		t.Fatal(err)
	}

	configuration := runtimeTestConfiguration(t)
	configuration.ArchivistHosts[0].CredentialRef = credentialReference
	configuration.ArchivistTurnLeaseTTL = 2 * time.Second
	runtime, err := controlruntime.OpenWithDependencies(context.Background(), configuration, "windows-acceptance", controlruntime.Dependencies{
		Secrets: runtimeSecret(credential), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	controlServer := httptest.NewServer(runtime.Handler())
	defer controlServer.Close()
	mcpServer := httptest.NewServer(runtime.MCPHandler())
	defer mcpServer.Close()

	proxyAddress := freeLoopbackAddress(t)
	hostConfigurationPath := filepath.Join(directory, "archivist-host.yaml")
	hostConfiguration := fmt.Sprintf(`schema_version: 1
archivist_host:
  id: archivist-windows
  control_url: %s
  mcp_control_url: %s
  credential_ref: %s
  heartbeat_interval: 1s
  claim_interval: 250ms
  mcp_listen_address: %s
clis:
  - id: codex
    executable: %s
    fixed_arguments:
      - -scenario
      - collaboration-resume
      - -hold-file
      - %s
      - -trace-file
      - %s
workspace_roots:
  - id: video-project
    path: %s
`, yamlScalar(controlServer.URL), yamlScalar(mcpServer.URL), yamlScalar(credentialReference),
		yamlScalar(proxyAddress), yamlScalar(cliExecutable), yamlScalar(holdPath), yamlScalar(tracePath), yamlScalar(directory))
	if err := os.WriteFile(hostConfigurationPath, []byte(hostConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}

	firstHost := startWindowsAcceptanceProcess(t, hostExecutable, "-config", hostConfigurationPath)
	defer firstHost.Stop(t)
	waitForWindowsAcceptance(t, 10*time.Second, "Archivist Host ONLINE", func() bool {
		response := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/archivist-hosts", "", nil, "", "")
		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"state":"ONLINE"`)
	})

	cookie, csrfToken := runtimeSession(t, runtime.Handler())
	origin := "http://127.0.0.1:8080"
	humanJob := `{"job_id":"JOB-HUMAN-WINDOWS-1","command_id":"COMMAND-HUMAN-WINDOWS-1","idempotency_key":"windows-acceptance:human-job","reason":"Windows collaboration acceptance","template_name":"occasion-recording","target_title":"Human initiated work","control_mode":"MANUAL","expected_revision":0}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/jobs", humanJob, cookie, csrfToken, origin); response.Code != http.StatusCreated {
		t.Fatalf("human Command returned %d: %s", response.Code, response.Body.String())
	}
	session := `{"session_id":"SESSION-WINDOWS-1","archivist_host_id":"archivist-windows","backend":"EMBEDDED_CORE","cli_id":"codex","workspace_root_id":"video-project","job_id":"JOB-HUMAN-WINDOWS-1"}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions", session, cookie, csrfToken, origin); response.Code != http.StatusCreated {
		t.Fatalf("ArchivistSession create returned %d: %s", response.Code, response.Body.String())
	}
	turn := `{"turn_id":"TURN-WINDOWS-1","request_id":"REQUEST-WINDOWS-1","prompt":"Inspect the shared work and propose the next allowed action."}`
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/archivist-sessions/SESSION-WINDOWS-1/turns", turn, cookie, csrfToken, origin); response.Code != http.StatusCreated {
		t.Fatalf("ArchivistTurn create returned %d: %s", response.Code, response.Body.String())
	}

	waitForWindowsAcceptance(t, 15*time.Second, "first CLI tool calls", func() bool {
		tokens := completedTraceTokens(tracePath)
		return len(tokens) >= 1 && tokens[0] == 1
	})
	firstCLIPIDs := tracedProcessIDs(tracePath)
	if len(firstCLIPIDs) != 1 {
		t.Fatalf("first CLI process trace=%v want one PID", firstCLIPIDs)
	}
	cardID := "ACTION-CALL-ACTION-TURN-WINDOWS-1"
	waitForWindowsAcceptance(t, 10*time.Second, "PENDING ActionCard", func() bool {
		projection := readWindowsArchivistProjection(t, runtime.Handler())
		return projection.actionCardState(cardID) == "PENDING"
	})
	if response := runtimeRequest(runtime.Handler(), http.MethodPost, "/api/v1/action-cards/"+cardID+"/execute", `{}`, cookie, csrfToken, origin); response.Code != http.StatusOK {
		t.Fatalf("explicit ActionCard operation returned %d: %s", response.Code, response.Body.String())
	}
	waitForWindowsAcceptance(t, 10*time.Second, "completed ActionCard and created Job", func() bool {
		projection := readWindowsArchivistProjection(t, runtime.Handler())
		job := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/jobs/JOB-TURN-WINDOWS-1", "", nil, "", "")
		return projection.actionCardState(cardID) == "COMPLETED" && job.Code == http.StatusOK
	})

	firstHost.Stop(t)
	stopWindowsAcceptancePID(t, firstCLIPIDs[0])
	time.Sleep(3 * time.Second)
	secondHost := startWindowsAcceptanceProcess(t, hostExecutable, "-config", hostConfigurationPath)
	defer secondHost.Stop(t)
	waitForWindowsAcceptance(t, 15*time.Second, "resumed CLI tool calls", func() bool {
		tokens := completedTraceTokens(tracePath)
		return len(tokens) >= 2 && tokens[0] == 1 && tokens[1] > tokens[0]
	})
	projection := readWindowsArchivistProjection(t, runtime.Handler())
	if state := projection.actionCardState(cardID); state != "COMPLETED" {
		t.Fatalf("replayed ActionCard state=%q want=COMPLETED", state)
	}
	if err := os.WriteFile(holdPath, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForWindowsAcceptance(t, 15*time.Second, "structured ArchivistTurn completion", func() bool {
		projection := readWindowsArchivistProjection(t, runtime.Handler())
		turn := projection.turn("TURN-WINDOWS-1")
		return turn != nil && turn.State == "COMPLETED" && turn.FinalResponse != nil &&
			turn.FinalResponse.Summary == "Windows Archivist Host resumed and completed the collaboration turn" &&
			len(turn.FinalResponse.ResultIDs) == 1 && turn.FinalResponse.ResultIDs[0] == "JOB-TURN-WINDOWS-1"
	})
	secondHost.Stop(t)

	ledger := openLedger(t, configuration.DatabasePath)
	defer ledger.Close()
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM command_records WHERE target_id = ?`, 1, "JOB-HUMAN-WINDOWS-1")
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM command_records WHERE target_id = ?`, 1, "JOB-TURN-WINDOWS-1")
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM idempotency_receipts WHERE idempotency_key = ?`, 1, "windows-acceptance:human-job")
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM idempotency_receipts WHERE idempotency_key = ?`, 1, "fixture-action:TURN-WINDOWS-1")
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM archivist_tool_calls WHERE turn_id = ?`, 2, "TURN-WINDOWS-1")
	assertWindowsRowCount(t, ledger, `SELECT COUNT(*) FROM archivist_action_cards WHERE turn_id = ?`, 1, "TURN-WINDOWS-1")

	tokens := completedTraceTokens(tracePath)
	hostname, _ := os.Hostname()
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "LRH-011", "host": hostname, "archivist_host_executable": filepath.Base(hostExecutable),
		"cli_executable": filepath.Base(cliExecutable), "session_id": "SESSION-WINDOWS-1", "turn_id": "TURN-WINDOWS-1",
		"fencing_tokens": tokens[:2], "tool_call_count": 2, "action_card_count": 1,
		"human_command_count": 1, "archivist_command_count": 1, "final_response": "structured",
	})
	t.Logf("acceptance_evidence=%s", evidence)
}

type windowsArchivistProjection struct {
	Data []struct {
		Turns []struct {
			TurnID        string `json:"turn_id"`
			State         string `json:"state"`
			FinalResponse *struct {
				Summary   string   `json:"summary"`
				ResultIDs []string `json:"result_ids"`
			} `json:"final_response"`
		} `json:"turns"`
		ActionCards []struct {
			ActionCardID string `json:"action_card_id"`
			State        string `json:"state"`
		} `json:"action_cards"`
	} `json:"data"`
}

func (projection windowsArchivistProjection) actionCardState(cardID string) string {
	for _, session := range projection.Data {
		for _, card := range session.ActionCards {
			if card.ActionCardID == cardID {
				return card.State
			}
		}
	}
	return ""
}

func (projection windowsArchivistProjection) turn(turnID string) *struct {
	TurnID        string `json:"turn_id"`
	State         string `json:"state"`
	FinalResponse *struct {
		Summary   string   `json:"summary"`
		ResultIDs []string `json:"result_ids"`
	} `json:"final_response"`
} {
	for sessionIndex := range projection.Data {
		for turnIndex := range projection.Data[sessionIndex].Turns {
			turn := &projection.Data[sessionIndex].Turns[turnIndex]
			if turn.TurnID == turnID {
				return turn
			}
		}
	}
	return nil
}

func readWindowsArchivistProjection(t *testing.T, handler http.Handler) windowsArchivistProjection {
	t.Helper()
	response := runtimeRequest(handler, http.MethodGet, "/api/v1/archivist-sessions", "", nil, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("Archivist projection returned %d: %s", response.Code, response.Body.String())
	}
	var projection windowsArchivistProjection
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatalf("decode Archivist projection: %v: %s", err, response.Body.String())
	}
	return projection
}

func requireWindowsAcceptanceExecutable(t *testing.T, environmentName string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(environmentName))
	if path == "" || !filepath.IsAbs(path) {
		t.Fatalf("%s must contain an absolute executable path", environmentName)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", environmentName, err)
	}
	return path
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func yamlScalar(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

type windowsAcceptanceProcess struct {
	command *exec.Cmd
	done    chan error
	once    sync.Once
}

func startWindowsAcceptanceProcess(t *testing.T, executable string, arguments ...string) *windowsAcceptanceProcess {
	t.Helper()
	command := exec.Command(executable, arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(executable), err)
	}
	process := &windowsAcceptanceProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	return process
}

func (process *windowsAcceptanceProcess) Stop(t *testing.T) {
	t.Helper()
	process.once.Do(func() {
		killErr := process.command.Process.Kill()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("terminate Archivist Host %d: %v", process.command.Process.Pid, killErr)
		}
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			output, err := exec.Command("taskkill.exe", "/PID", strconv.Itoa(process.command.Process.Pid), "/F").CombinedOutput()
			t.Errorf("Archivist Host %d did not stop: taskkill=%v %s", process.command.Process.Pid, err, output)
		}
	})
}

func waitForWindowsAcceptance(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func completedTraceTokens(path string) []uint64 {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result []uint64
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "TOOLS_DONE" {
			continue
		}
		token, err := strconv.ParseUint(fields[2], 10, 64)
		if err == nil {
			result = append(result, token)
		}
	}
	return result
}

func tracedProcessIDs(path string) []int {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result []int
	seen := map[int]bool{}
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "START" {
			continue
		}
		processID, err := strconv.Atoi(fields[3])
		if err == nil && processID > 0 && !seen[processID] {
			seen[processID] = true
			result = append(result, processID)
		}
	}
	return result
}

func stopWindowsAcceptancePID(t *testing.T, processID int) {
	t.Helper()
	process, err := os.FindProcess(processID)
	if err != nil {
		t.Fatalf("find CLI process %d: %v", processID, err)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate CLI process %d: %v", processID, err)
	}
}

func assertWindowsRowCount(t *testing.T, ledger interface {
	QueryRow(string, ...any) *sql.Row
}, query string, want int, arguments ...any) {
	t.Helper()
	var count int
	if err := ledger.QueryRow(query, arguments...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("row count=%d want=%d query=%s", count, want, query)
	}
}
