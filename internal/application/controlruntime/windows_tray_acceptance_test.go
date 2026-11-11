//go:build windows

package controlruntime_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Requirement: CFG-016
func TestWindowsTrayStartsAtInteractiveLogonWithoutOwningControl(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_TRAY_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_TRAY_ACCEPTANCE=1 on the installed Windows host")
	}
	root := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_RUNTIME_ROOT")
	dashboardURL := strings.TrimSpace(os.Getenv("ARCHIVE_WINDOWS_TRAY_URL"))
	if dashboardURL == "" {
		t.Fatal("ARCHIVE_WINDOWS_TRAY_URL is required")
	}
	trayExecutable := filepath.Join(root, "runtime", "current", "archive-tray.exe")
	requireWindowsRuntimeFile(t, trayExecutable)

	snapshot := readWindowsTraySnapshot(t)
	if !strings.EqualFold(snapshot.Task.Execute, trayExecutable) {
		t.Fatalf("Tray task uses a noncanonical executable: %s", snapshot.Task.Execute)
	}
	if snapshot.Task.TriggerClass != "MSFT_TaskLogonTrigger" || snapshot.Task.LogonType != "Interactive" ||
		snapshot.Task.RunLevel != "Limited" {
		t.Fatalf("Tray task is not an interactive limited AtLogOn task: %+v", snapshot.Task)
	}
	if snapshot.Task.UserSID == "" || snapshot.Task.UserSID != snapshot.ConsoleUserSID {
		t.Fatalf("Tray task belongs to the wrong interactive user: %+v", snapshot)
	}
	if !strings.Contains(snapshot.Task.Arguments, `--dashboard-url "`+dashboardURL+`"`) ||
		!strings.Contains(snapshot.Task.Arguments, `--chrome "`) {
		t.Fatalf("Tray task arguments do not pin the dashboard URL and Chrome executable: %s", snapshot.Task.Arguments)
	}
	if snapshot.Tray.ProcessID == 0 || snapshot.Tray.SessionID == 0 || snapshot.Tray.OwnerSID != snapshot.ConsoleUserSID ||
		!strings.EqualFold(snapshot.Tray.Executable, trayExecutable) {
		t.Fatalf("Tray is not running in the interactive user session: %+v", snapshot.Tray)
	}
	if snapshot.Control.ProcessID == 0 {
		t.Fatal("Archive Control must be running before Tray independence is checked")
	}

	stopWindowsTrayTask(t)
	trayStopped := true
	defer func() {
		if trayStopped {
			startWindowsTrayTask(t, trayExecutable)
		}
	}()
	afterStop := waitForWindowsTrayStop(t)
	if afterStop.Tray.ProcessID != 0 {
		t.Fatalf("Tray process remained after stopping its task: %+v", afterStop.Tray)
	}
	if afterStop.Control.ProcessID != snapshot.Control.ProcessID {
		t.Fatalf("stopping Tray changed Archive Control: before=%d after=%d", snapshot.Control.ProcessID, afterStop.Control.ProcessID)
	}

	startWindowsTrayTask(t, trayExecutable)
	trayStopped = false
	afterRestart := readWindowsTraySnapshot(t)
	if afterRestart.Tray.ProcessID == 0 || afterRestart.Tray.SessionID == 0 ||
		afterRestart.Tray.OwnerSID != snapshot.ConsoleUserSID {
		t.Fatalf("Tray did not return to the interactive user session: %+v", afterRestart.Tray)
	}
}

func waitForWindowsTrayStop(t *testing.T) windowsTrayProcessSnapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := readWindowsTrayProcessSnapshot(t)
		if snapshot.Tray.ProcessID == 0 {
			return snapshot
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Archive Tray process did not stop")
	return windowsTrayProcessSnapshot{}
}

type windowsTrayTaskSnapshot struct {
	Execute      string `json:"execute"`
	Arguments    string `json:"arguments"`
	TriggerClass string `json:"trigger_class"`
	LogonType    string `json:"logon_type"`
	RunLevel     string `json:"run_level"`
	UserSID      string `json:"user_sid"`
}

type windowsTrayProcess struct {
	ProcessID  uint32 `json:"process_id"`
	SessionID  uint32 `json:"session_id"`
	Executable string `json:"executable"`
	OwnerSID   string `json:"owner_sid"`
}

type windowsControlProcess struct {
	ProcessID uint32 `json:"process_id"`
}

type windowsTraySnapshot struct {
	Task           windowsTrayTaskSnapshot `json:"task"`
	Tray           windowsTrayProcess      `json:"tray"`
	Control        windowsControlProcess   `json:"control"`
	ConsoleUserSID string                  `json:"console_user_sid"`
}

type windowsTrayProcessSnapshot struct {
	Tray    windowsTrayProcess    `json:"tray"`
	Control windowsControlProcess `json:"control"`
}

func readWindowsTraySnapshot(t *testing.T) windowsTraySnapshot {
	t.Helper()
	script := `$ErrorActionPreference='Stop'; $task=Get-ScheduledTask -TaskName 'Archive Tray'; $console=(Get-CimInstance Win32_ComputerSystem).UserName; $consoleSid=(New-Object System.Security.Principal.NTAccount($console)).Translate([System.Security.Principal.SecurityIdentifier]).Value; $taskSid=(New-Object System.Security.Principal.NTAccount($task.Principal.UserId)).Translate([System.Security.Principal.SecurityIdentifier]).Value; $tray=Get-CimInstance Win32_Process -Filter "Name='archive-tray.exe'" | Select-Object -First 1; $trayRecord=$null; if($null -ne $tray){$owner=Invoke-CimMethod -InputObject $tray -MethodName GetOwner; $ownerSid=(New-Object System.Security.Principal.NTAccount($owner.Domain+'\'+$owner.User)).Translate([System.Security.Principal.SecurityIdentifier]).Value; $trayRecord=[pscustomobject]@{process_id=$tray.ProcessId;session_id=$tray.SessionId;executable=$tray.ExecutablePath;owner_sid=$ownerSid}}; $control=Get-CimInstance Win32_Process -Filter "Name='archive-control.exe'" | Select-Object -First 1; [pscustomobject]@{task=[pscustomobject]@{execute=$task.Actions[0].Execute;arguments=$task.Actions[0].Arguments;trigger_class=$task.Triggers[0].CimClass.CimClassName;logon_type=[string]$task.Principal.LogonType;run_level=[string]$task.Principal.RunLevel;user_sid=$taskSid};tray=$trayRecord;control=[pscustomobject]@{process_id=if($null -eq $control){0}else{$control.ProcessId}};console_user_sid=$consoleSid} | ConvertTo-Json -Depth 6 -Compress`
	var snapshot windowsTraySnapshot
	runWindowsTrayPowerShellJSON(t, script, &snapshot)
	return snapshot
}

func readWindowsTrayProcessSnapshot(t *testing.T) windowsTrayProcessSnapshot {
	t.Helper()
	script := `$ErrorActionPreference='Stop'; $tray=Get-CimInstance Win32_Process -Filter "Name='archive-tray.exe'" | Select-Object -First 1; $trayRecord=$null; if($null -ne $tray){$owner=Invoke-CimMethod -InputObject $tray -MethodName GetOwner; $ownerSid=(New-Object System.Security.Principal.NTAccount($owner.Domain+'\'+$owner.User)).Translate([System.Security.Principal.SecurityIdentifier]).Value; $trayRecord=[pscustomobject]@{process_id=$tray.ProcessId;session_id=$tray.SessionId;executable=$tray.ExecutablePath;owner_sid=$ownerSid}}; $control=Get-CimInstance Win32_Process -Filter "Name='archive-control.exe'" | Select-Object -First 1; [pscustomobject]@{tray=$trayRecord;control=[pscustomobject]@{process_id=if($null -eq $control){0}else{$control.ProcessId}}} | ConvertTo-Json -Depth 5 -Compress`
	var snapshot windowsTrayProcessSnapshot
	runWindowsTrayPowerShellJSON(t, script, &snapshot)
	return snapshot
}

func stopWindowsTrayTask(t *testing.T) {
	t.Helper()
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `Stop-ScheduledTask -TaskName 'Archive Tray' -ErrorAction Stop`)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stop Archive Tray task: %v: %s", err, output)
	}
}

func startWindowsTrayTask(t *testing.T, trayExecutable string) {
	t.Helper()
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `Start-ScheduledTask -TaskName 'Archive Tray' -ErrorAction Stop`)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start Archive Tray task: %v: %s", err, output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := readWindowsTrayProcessSnapshot(t)
		if snapshot.Tray.ProcessID != 0 && strings.EqualFold(snapshot.Tray.Executable, trayExecutable) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Archive Tray process did not start")
}

func runWindowsTrayPowerShellJSON(t *testing.T, script string, target any) {
	t.Helper()
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Archive Tray: %v: %s", err, output)
	}
	output = bytes.TrimPrefix(bytes.TrimSpace(output), []byte{0xef, 0xbb, 0xbf})
	if err := json.Unmarshal(output, target); err != nil {
		t.Fatalf("decode Archive Tray snapshot: %v: %s", err, output)
	}
}
