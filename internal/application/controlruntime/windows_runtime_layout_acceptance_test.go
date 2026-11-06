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

	"videoarchive/internal/adapter/archivisthostconfig"
	"videoarchive/internal/adapter/bootconfig"
	"videoarchive/internal/adapter/workerconfig"
)

// Requirement: CFG-014
func TestWindowsRuntimeUsesSingleRootAndRetainsLegacyEvidence(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_RUNTIME_LAYOUT_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_RUNTIME_LAYOUT_ACCEPTANCE=1 on the installed Windows host")
	}
	root := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_RUNTIME_ROOT")
	evidence := requireWindowsRuntimePath(t, "ARCHIVE_WINDOWS_RUNTIME_EVIDENCE")

	for _, relative := range []string{
		"runtime/current", "runtime/releases", "config", "data", "logs", "scratch", "evidence", "legacy",
	} {
		requireWindowsRuntimeDirectory(t, filepath.Join(root, filepath.FromSlash(relative)))
	}
	for _, name := range []string{"archive-control.exe", "archive-media.exe", "archivist-host.exe"} {
		requireWindowsRuntimeFile(t, filepath.Join(root, "runtime", "current", name))
	}

	control, err := bootconfig.Load(filepath.Join(root, "config", "control.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !sameWindowsPath(control.DatabasePath, filepath.Join(root, "data", "control.db")) {
		t.Fatalf("database is outside the standard data directory: %s", control.DatabasePath)
	}
	controlScratch := ""
	for _, topology := range control.Storages {
		if topology.StorageID == "scratch-windows" {
			controlScratch = topology.Root
		}
	}
	if !sameWindowsPath(controlScratch, filepath.Join(root, "scratch")) {
		t.Fatalf("control scratch root is not canonical: %s", controlScratch)
	}

	worker, err := workerconfig.Load(filepath.Join(root, "config", "worker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workerScratch := ""
	for _, storageRoot := range worker.StorageRoots {
		if storageRoot.StorageID == "scratch-windows" {
			workerScratch = storageRoot.Root
		}
	}
	if !sameWindowsPath(workerScratch, filepath.Join(root, "scratch")) {
		t.Fatalf("worker scratch root is not canonical: %s", workerScratch)
	}

	host, err := archivisthostconfig.Load(filepath.Join(root, "config", "archivist-host.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for id, workspaceRoot := range host.LaunchPolicy.WorkspaceRoots {
		if !sameWindowsPath(workspaceRoot, filepath.Join(root, "workspace")) {
			t.Fatalf("Archivist workspace %s is outside the canonical workspace: %s", id, workspaceRoot)
		}
	}
	for id, cli := range host.LaunchPolicy.CLIs {
		requireWindowsRuntimeDescendant(t, root, cli.Executable, "Archivist CLI "+id)
	}

	snapshot := readWindowsRuntimeSnapshot(t)
	wantActions := map[string]string{
		"Archive Control":        filepath.Join(root, "runtime", "current", "run-control.cmd"),
		"Archive Worker":         filepath.Join(root, "runtime", "current", "run-worker.cmd"),
		"Archive Archivist Host": filepath.Join(root, "runtime", "current", "run-archivist-host.cmd"),
	}
	if len(snapshot.Tasks) != len(wantActions) {
		t.Fatalf("unexpected scheduled task count: %d", len(snapshot.Tasks))
	}
	for _, task := range snapshot.Tasks {
		want, ok := wantActions[task.Name]
		if !ok || !sameWindowsPath(task.Execute, want) {
			t.Fatalf("scheduled task uses a noncanonical action: %s -> %s", task.Name, task.Execute)
		}
	}
	if len(snapshot.Processes) != 3 {
		t.Fatalf("unexpected runtime process count: %d", len(snapshot.Processes))
	}
	for _, process := range snapshot.Processes {
		requireWindowsRuntimeDescendant(t, filepath.Join(root, "runtime", "current"), process.Executable, process.Name)
	}

	for _, obsolete := range []string{
		`C:\Video-Production-Archive`, filepath.Join(root, "deploy"), filepath.Join(root, "bin"), filepath.Join(root, "config", "data"),
	} {
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Fatalf("obsolete active path still exists: %s", obsolete)
		}
	}

	var summary struct {
		Health                       string   `json:"health"`
		WorkerStates                 []string `json:"worker_states"`
		ArchivistHostStates          []string `json:"archivist_host_states"`
		LegacyManifestCount          int      `json:"legacy_manifest_count"`
		LegacyValidManifestCount     int      `json:"legacy_valid_manifest_count"`
		LegacyInvalidManifestCount   int      `json:"legacy_invalid_manifest_count"`
		RelatedProfileFilesRemaining int      `json:"related_profile_files_remaining"`
		DeletedItems                 int      `json:"deleted_items"`
	}
	readWindowsRuntimeJSON(t, filepath.Join(evidence, "acceptance-summary.json"), &summary)
	if summary.Health != "ok" || !allWindowsRuntimeStates(summary.WorkerStates, "ONLINE") ||
		!allWindowsRuntimeStates(summary.ArchivistHostStates, "ONLINE") {
		t.Fatalf("runtime was not healthy in acceptance evidence: %+v", summary)
	}
	if summary.LegacyManifestCount == 0 ||
		summary.LegacyValidManifestCount+summary.LegacyInvalidManifestCount != summary.LegacyManifestCount {
		t.Fatalf("legacy ArchivePackage verification is incomplete: %+v", summary)
	}
	if summary.RelatedProfileFilesRemaining != 0 || summary.DeletedItems != 0 {
		t.Fatalf("legacy cleanup was destructive or incomplete: %+v", summary)
	}

	var moves struct {
		Recoverable  bool `json:"recoverable"`
		DeletedItems int  `json:"deleted_items"`
		Moves        []struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
		} `json:"moves"`
	}
	readWindowsRuntimeJSON(t, filepath.Join(evidence, "legacy-moves.json"), &moves)
	if !moves.Recoverable || moves.DeletedItems != 0 || len(moves.Moves) == 0 {
		t.Fatalf("legacy move evidence is not recoverable: %+v", moves)
	}
	for _, move := range moves.Moves {
		requireWindowsRuntimeDescendant(t, filepath.Join(root, "legacy"), move.Destination, "legacy destination")
		if _, err := os.Stat(move.Destination); err != nil {
			t.Fatalf("legacy destination is unavailable: %s: %v", move.Destination, err)
		}
	}

	manifestCount := 0
	err = filepath.Walk(filepath.Join(root, "legacy"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !info.IsDir() && strings.EqualFold(info.Name(), "manifest-sha256.txt") {
			manifestCount++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifestCount < summary.LegacyManifestCount {
		t.Fatalf("verified legacy manifests are missing: got %d, want at least %d", manifestCount, summary.LegacyManifestCount)
	}
}

type windowsRuntimeSnapshot struct {
	Tasks []struct {
		Name    string `json:"name"`
		Execute string `json:"execute"`
	} `json:"tasks"`
	Processes []struct {
		Name       string `json:"name"`
		Executable string `json:"executable"`
	} `json:"processes"`
}

func readWindowsRuntimeSnapshot(t *testing.T) windowsRuntimeSnapshot {
	t.Helper()
	script := `$ProgressPreference='SilentlyContinue'; $names=@('Archive Control','Archive Worker','Archive Archivist Host'); $tasks=@(Get-ScheduledTask -TaskName $names | ForEach-Object {[pscustomobject]@{name=$_.TaskName;execute=$_.Actions[0].Execute}}); $processes=@(Get-CimInstance Win32_Process | Where-Object {$_.Name -in @('archive-control.exe','archive-media.exe','archivist-host.exe')} | ForEach-Object {[pscustomobject]@{name=$_.Name;executable=$_.ExecutablePath}}); [pscustomobject]@{tasks=$tasks;processes=$processes} | ConvertTo-Json -Depth 5 -Compress`
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Windows runtime: %v: %s", err, output)
	}
	output = bytes.TrimPrefix(bytes.TrimSpace(output), []byte{0xef, 0xbb, 0xbf})
	var snapshot windowsRuntimeSnapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		t.Fatalf("decode Windows runtime snapshot: %v: %s", err, output)
	}
	return snapshot
}

func requireWindowsRuntimePath(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", name)
	}
	return filepath.Clean(value)
}

func requireWindowsRuntimeDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("required runtime directory is unavailable: %s: %v", path, err)
	}
}

func requireWindowsRuntimeFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("required runtime file is unavailable: %s: %v", path, err)
	}
}

func requireWindowsRuntimeDescendant(t *testing.T, root, path, label string) {
	t.Helper()
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("%s is outside %s: %s", label, root, path)
	}
}

func sameWindowsPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func allWindowsRuntimeStates(states []string, want string) bool {
	if len(states) == 0 {
		return false
	}
	for _, state := range states {
		if state != want {
			return false
		}
	}
	return true
}

func readWindowsRuntimeJSON(t *testing.T, path string, target any) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
