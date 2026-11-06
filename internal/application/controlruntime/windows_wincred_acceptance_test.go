//go:build windows

package controlruntime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/controlruntime"
)

func TestWindowsCredentialManagerAuthenticatesOnlyMatchingControlAndWorkerProcesses(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_WINCRED_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_WINCRED_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	mediaExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_MEDIA_PATH")
	ffmpegExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFMPEG_PATH")
	ffprobeExecutable := requireWindowsAcceptanceExecutable(t, "ARCHIVE_FFPROBE_PATH")
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 10) + "-" + strconv.Itoa(os.Getpid())
	correctTarget := "archive/acceptance/worker-correct-" + suffix
	wrongTarget := "archive/acceptance/worker-wrong-" + suffix
	correctSecret := "wincred-correct-" + suffix
	wrongSecret := "wincred-wrong-" + suffix
	provisionWindowsGenericCredential(t, correctTarget, correctSecret)
	provisionWindowsGenericCredential(t, wrongTarget, wrongSecret)

	configuration := runtimeTestConfiguration(t)
	configuration.Workers[0].CredentialRef = "wincred://" + correctTarget
	runtime, err := controlruntime.Open(context.Background(), configuration, "windows-wincred-acceptance")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := httptest.NewServer(runtime.Handler())
	defer server.Close()

	directory := t.TempDir()
	writeWorkerConfiguration := func(reference string) string {
		path := filepath.Join(directory, "worker-"+strings.TrimPrefix(reference, "wincred://")+".yaml")
		document := fmt.Sprintf(`schema_version: 1
worker:
  id: worker-main
  control_url: %s
  credential_ref: %s
  heartbeat_interval: 1s
  claim_interval: 1s
  storage_roots:
    - storage_id: scratch-main
      root: %s
  tools:
    ffmpeg:
      executable: %s
      version: ffmpeg-windows-wincred-acceptance
    ffprobe:
      executable: %s
      version: ffprobe-windows-wincred-acceptance
`, yamlScalar(server.URL), yamlScalar(reference), yamlScalar(directory), yamlScalar(ffmpegExecutable), yamlScalar(ffprobeExecutable))
		if strings.Contains(document, correctSecret) || strings.Contains(document, wrongSecret) {
			t.Fatal("Worker configuration contains a plaintext credential")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	wrongWorker := startWindowsAcceptanceProcess(t, mediaExecutable, "runner", "-config", writeWorkerConfiguration("wincred://"+wrongTarget))
	time.Sleep(2500 * time.Millisecond)
	status := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"OFFLINE"`) {
		wrongWorker.Stop(t)
		t.Fatalf("wrong Windows credential connected: %d %s", status.Code, status.Body.String())
	}
	wrongWorker.Stop(t)

	correctWorker := startWindowsAcceptanceProcess(t, mediaExecutable, "runner", "-config", writeWorkerConfiguration("wincred://"+correctTarget))
	defer correctWorker.Stop(t)
	waitForWindowsAcceptance(t, 15*time.Second, "Worker authenticated through Windows Credential Manager", func() bool {
		response := runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"state":"ONLINE"`)
	})
	status = runtimeRequest(runtime.Handler(), http.MethodGet, "/api/v1/workers", "", nil, "", "")
	body := status.Body.String()
	if strings.Contains(body, correctSecret) || strings.Contains(body, wrongSecret) ||
		strings.Contains(body, correctTarget) || strings.Contains(body, wrongTarget) || strings.Contains(body, "wincred://") {
		t.Fatalf("Worker response exposed Windows credential material: %s", body)
	}
	hostname, _ := os.Hostname()
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "WRK-007", "host": hostname, "wrong_credential_rejected": true,
		"matching_credential_online": true, "credential_material_absent_from_response": true,
	})
	t.Logf("acceptance_evidence=%s", evidence)
}

func provisionWindowsGenericCredential(t *testing.T, target, secret string) {
	t.Helper()
	command := exec.Command("cmdkey.exe", "/generic:"+target, "/user:archive-acceptance", "/pass:"+secret)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("provision Windows generic credential %s: %v: %s", target, err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("cmdkey.exe", "/delete:"+target).CombinedOutput(); err != nil {
			t.Errorf("delete Windows generic credential %s: %v: %s", target, err, output)
		}
	})
}
