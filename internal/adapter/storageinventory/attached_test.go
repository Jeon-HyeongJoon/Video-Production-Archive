package storageinventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"videoarchive/internal/core/device"
)

// Requirement: ORG-001
func TestSelectedAttachedSourceIsScannedAsynchronouslyWithoutFollowingHiddenOrLinkedPaths(t *testing.T) {
	selectedRoot := t.TempDir()
	unselectedRoot := t.TempDir()
	for relative, payload := range map[string]string{
		"event-a/C0001.MP4": "first",
		"event-a/C0002.mov": "second",
		"event-a/notes.txt": "ignored",
	} {
		fullPath := filepath.Join(selectedRoot, relative)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(unselectedRoot, "private.mp4"), []byte("unselected"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(externalRoot, "escape.mp4"), []byte("escape"), 0o600); err != nil {
		t.Fatal(err)
	}
	createDirectoryLink(t, externalRoot, filepath.Join(selectedRoot, "linked"))
	if err := os.MkdirAll(filepath.Join(selectedRoot, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(selectedRoot, ".hidden", "hidden.mp4"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	volumes := []device.ConnectedVolume{
		{StorageID: "source-selected", Root: selectedRoot, CapacityBytes: 2_000, FreeBytes: 500, ObservedAt: time.Now()},
		{StorageID: "source-unselected", Root: unselectedRoot, CapacityBytes: 3_000, FreeBytes: 700, ObservedAt: time.Now()},
	}
	probe := NewAttachedProbe()
	observations, err := probe.Observe(context.Background(), []string{"source-selected"}, volumes)
	if err != nil || len(observations) != 0 {
		t.Fatalf("selected source scan blocked the inventory call: observations=%+v err=%v", observations, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(observations) == 0 && time.Now().Before(deadline) {
		var err error
		observations, err = probe.Observe(context.Background(), []string{"source-selected"}, volumes)
		if err != nil {
			t.Fatal(err)
		}
		if len(observations) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(observations) != 1 || observations[0].StorageID != "source-selected" || !observations[0].Connected ||
		observations[0].CapacityBytes != 2_000 || observations[0].FreeBytes != 500 || observations[0].DiscoveryRevision == 0 ||
		len(observations[0].Folders) != 1 || observations[0].Folders[0].Folder != "event-a" ||
		observations[0].Folders[0].VideoCount != 2 || observations[0].Folders[0].InputBytes != uint64(len("first")+len("second")) {
		t.Fatalf("selected source was not isolated and inventoried: %+v", observations)
	}
}

func createDirectoryLink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		output, err := exec.Command("cmd.exe", "/d", "/s", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Fatalf("create Windows junction: %v: %s", err, output)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
