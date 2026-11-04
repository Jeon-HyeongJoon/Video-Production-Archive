package storageinventory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"videoarchive/internal/core/device"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
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
	if err != nil || len(observations) != 1 || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning || observations[0].DiscoveryRevision != 0 {
		t.Fatalf("selected source scan blocked the inventory call: observations=%+v err=%v", observations, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for observations[0].DiscoveryState != vocabulary.FolderDiscoveryReady && time.Now().Before(deadline) {
		var err error
		observations, err = probe.Observe(context.Background(), []string{"source-selected"}, volumes)
		if err != nil {
			t.Fatal(err)
		}
		if observations[0].DiscoveryState != vocabulary.FolderDiscoveryReady {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(observations) != 1 || observations[0].StorageID != "source-selected" || !observations[0].Connected ||
		observations[0].DiscoveryState != vocabulary.FolderDiscoveryReady ||
		observations[0].CapacityBytes != 2_000 || observations[0].FreeBytes != 500 || observations[0].DiscoveryRevision == 0 ||
		len(observations[0].Folders) != 1 || observations[0].Folders[0].Folder != "event-a" ||
		observations[0].Folders[0].VideoCount != 2 || observations[0].Folders[0].InputBytes != uint64(len("first")+len("second")) {
		t.Fatalf("selected source was not isolated and inventoried: %+v", observations)
	}
}

// Requirement: DEV-011
func TestSelectedAttachedSourceReportsIncrementalDiscoveryStateWithoutBlocking(t *testing.T) {
	volume := device.ConnectedVolume{StorageID: "source-large", Root: t.TempDir(), CapacityBytes: 4_000, FreeBytes: 1_000, ObservedAt: time.Now()}
	probe := NewAttachedProbe()
	partialPublished := make(chan struct{})
	release := make(chan struct{})
	probe.scan = func(_ context.Context, task attachedScanTask, publish func([]worker.MediaFolderObservation)) (worker.StorageObservation, error) {
		publish([]worker.MediaFolderObservation{{Folder: "first", VideoCount: 2}})
		close(partialPublished)
		<-release
		return worker.StorageObservation{
			StorageID: task.storageID, Connected: true, AccessKind: vocabulary.StorageAccessAttachedVolume,
			DiscoveryState: vocabulary.FolderDiscoveryReady, CapacityBytes: task.volume.CapacityBytes,
			FreeBytes: task.volume.FreeBytes, DiscoveryRevision: 77,
			Folders: []worker.MediaFolderObservation{{Folder: "first", VideoCount: 2, InputBytes: 200}, {Folder: "second", VideoCount: 3, InputBytes: 300}},
		}, nil
	}

	observations, err := probe.Observe(context.Background(), []string{volume.StorageID}, []device.ConnectedVolume{volume})
	if err != nil || len(observations) != 1 || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning || observations[0].DiscoveryRevision != 0 {
		t.Fatalf("initial discovery did not return immediately as SCANNING: observations=%+v err=%v", observations, err)
	}
	select {
	case <-partialPublished:
	case <-time.After(time.Second):
		t.Fatal("partial discovery was not published")
	}
	observations, err = probe.Observe(context.Background(), []string{volume.StorageID}, []device.ConnectedVolume{volume})
	if err != nil || len(observations) != 1 || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning ||
		observations[0].DiscoveryRevision != 0 || len(observations[0].Folders) != 1 || observations[0].Folders[0].VideoCount != 2 {
		t.Fatalf("partial folders were not reported without a runnable revision: observations=%+v err=%v", observations, err)
	}
	close(release)
	observations = waitForDiscoveryState(t, probe, volume, vocabulary.FolderDiscoveryReady)
	if observations[0].DiscoveryRevision != 77 || len(observations[0].Folders) != 2 || observations[0].Folders[1].VideoCount != 3 {
		t.Fatalf("completed discovery did not replace partial results: %+v", observations)
	}

	failed := NewAttachedProbe()
	failed.scan = func(context.Context, attachedScanTask, func([]worker.MediaFolderObservation)) (worker.StorageObservation, error) {
		return worker.StorageObservation{}, errors.New("unreadable source")
	}
	observations, err = failed.Observe(context.Background(), []string{volume.StorageID}, []device.ConnectedVolume{volume})
	if err != nil || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning {
		t.Fatalf("failed discovery did not begin as SCANNING: observations=%+v err=%v", observations, err)
	}
	observations = waitForDiscoveryState(t, failed, volume, vocabulary.FolderDiscoveryFailed)
	if observations[0].DiscoveryRevision != 0 || len(observations[0].Folders) != 0 {
		t.Fatalf("failed discovery exposed a revision or partial scope: %+v", observations)
	}
}

func waitForDiscoveryState(t *testing.T, probe *AttachedProbe, volume device.ConnectedVolume, want vocabulary.FolderDiscoveryState) []worker.StorageObservation {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		observations, err := probe.Observe(context.Background(), []string{volume.StorageID}, []device.ConnectedVolume{volume})
		if err != nil {
			t.Fatal(err)
		}
		if len(observations) == 1 && observations[0].DiscoveryState == want {
			return observations
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("discovery did not reach %s", want)
	return nil
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
