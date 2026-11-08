//go:build windows

package controlruntime_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Requirement: LRH-020
func TestWindowsCodexHostRunsInAuthenticatedInteractiveTerminalSession(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_INTERACTIVE_ARCHIVIST_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_INTERACTIVE_ARCHIVIST_ACCEPTANCE=1 on the installed Windows host")
	}
	root := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_RUNTIME_ROOT")
	codex := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_CODEX_EXECUTABLE")
	script := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_INTERACTIVE_ARCHIVIST_SCRIPT")
	for _, path := range []string{codex, script} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("required acceptance file is unavailable: %s: %v", path, err)
		}
	}

	probe := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Mode", "Probe", "-Root", root, "-CodexExecutable", codex)
	probeOutput, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("probe interactive Codex login: %v: %s", err, probeOutput)
	}
	var login struct {
		UserSID          string `json:"user_sid"`
		CodexLoginResult int    `json:"codex_login_result"`
	}
	if err := json.Unmarshal(trimPowerShellOutput(probeOutput), &login); err != nil || login.UserSID == "" || login.CodexLoginResult != 0 {
		t.Fatalf("interactive Codex login is unavailable: decode=%v output=%s", err, probeOutput)
	}

	inspectScript := `$task=Get-ScheduledTask -TaskName 'Archive Archivist Host'; $interactive=(Get-CimInstance Win32_ComputerSystem).UserName; $interactiveSid=(New-Object System.Security.Principal.NTAccount($interactive)).Translate([System.Security.Principal.SecurityIdentifier]).Value; $process=Get-CimInstance Win32_Process | Where-Object {$_.Name -eq 'archivist-host.exe'} | Select-Object -First 1; if($null -eq $process){throw 'Archivist Host process is unavailable'}; $owner=Invoke-CimMethod -InputObject $process -MethodName GetOwner; $ownerSid=(New-Object System.Security.Principal.NTAccount($owner.Domain+'\'+$owner.User)).Translate([System.Security.Principal.SecurityIdentifier]).Value; [pscustomobject]@{logon_type=[string]$task.Principal.LogonType;task_user_sid=(New-Object System.Security.Principal.NTAccount($task.Principal.UserId)).Translate([System.Security.Principal.SecurityIdentifier]).Value;interactive_user_sid=$interactiveSid;process_owner_sid=$ownerSid;process_session_id=$process.SessionId;execute=$task.Actions[0].Execute} | ConvertTo-Json -Compress`
	inspect := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", inspectScript)
	inspectOutput, err := inspect.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect interactive Archivist Host: %v: %s", err, inspectOutput)
	}
	var actual struct {
		LogonType          string `json:"logon_type"`
		TaskUserSID        string `json:"task_user_sid"`
		InteractiveUserSID string `json:"interactive_user_sid"`
		ProcessOwnerSID    string `json:"process_owner_sid"`
		ProcessSessionID   uint32 `json:"process_session_id"`
		Execute            string `json:"execute"`
	}
	if err := json.Unmarshal(trimPowerShellOutput(inspectOutput), &actual); err != nil {
		t.Fatalf("decode interactive Host inspection: %v: %s", err, inspectOutput)
	}
	wantLauncher := filepath.Join(root, "runtime", "current", "run-archivist-host.cmd")
	if actual.LogonType != "Interactive" || actual.TaskUserSID != login.UserSID ||
		actual.InteractiveUserSID != login.UserSID || actual.ProcessOwnerSID != login.UserSID ||
		actual.ProcessSessionID == 0 || !sameWindowsPath(actual.Execute, wantLauncher) {
		t.Fatalf("Archivist Host does not use the authenticated interactive terminal session: %+v login=%+v", actual, login)
	}
}

func trimPowerShellOutput(output []byte) []byte {
	return bytes.TrimPrefix(bytes.TrimSpace(output), []byte{0xef, 0xbb, 0xbf})
}
