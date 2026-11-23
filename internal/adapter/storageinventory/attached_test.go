package storageinventory

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
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
			Folders: []worker.MediaFolderObservation{
				{Folder: "first", VideoCount: 2, InputBytes: 200, Videos: []worker.MediaFileObservation{{RelativePath: "first/A001.mp4", FileName: "A001.mp4", SizeBytes: 120, ModifiedAt: time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)}}},
				{Folder: "second", VideoCount: 3, InputBytes: 300, Videos: []worker.MediaFileObservation{{RelativePath: "second/B001.mov", FileName: "B001.mov", SizeBytes: 180, ModifiedAt: time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)}}},
			},
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
	if observations[0].DiscoveryRevision != 77 || len(observations[0].Folders) != 2 || observations[0].Folders[1].VideoCount != 3 ||
		len(observations[0].Folders[1].Videos) != 1 || observations[0].Folders[1].Videos[0].RelativePath != "second/B001.mov" ||
		observations[0].Folders[1].Videos[0].FileName != "B001.mov" || observations[0].Folders[1].Videos[0].SizeBytes != 180 {
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

// Requirement: DEV-014
func TestDetachedSourceCancelsDiscoveryBeforeReusedMountCanPublishOldIdentity(t *testing.T) {
	root := t.TempDir()
	oldVolume := device.ConnectedVolume{
		PhysicalDeviceID: "physical-old", VolumeID: "volume-old", StorageID: "source-old",
		Root: root, CapacityBytes: 4_000, FreeBytes: 1_000, ObservedAt: time.Now(),
	}
	newVolume := device.ConnectedVolume{
		PhysicalDeviceID: "physical-new", VolumeID: "volume-new", StorageID: "source-new",
		Root: root, CapacityBytes: 8_000, FreeBytes: 2_000, ObservedAt: time.Now().Add(time.Second),
	}
	probe := NewAttachedProbe()
	started := make(chan string, 2)
	canceled := make(chan struct{})
	probe.scan = func(ctx context.Context, task attachedScanTask, publish func([]worker.MediaFolderObservation)) (worker.StorageObservation, error) {
		started <- task.storageID
		if task.storageID == oldVolume.StorageID {
			<-ctx.Done()
			// A slow filesystem call may return one last stale batch. The probe
			// must ignore it after detachment and must not bind it to a reused root.
			publish([]worker.MediaFolderObservation{{Folder: "stale", VideoCount: 99}})
			close(canceled)
			return worker.StorageObservation{}, ctx.Err()
		}
		return worker.StorageObservation{
			StorageID: task.storageID, Connected: true, AccessKind: vocabulary.StorageAccessAttachedVolume,
			DiscoveryState: vocabulary.FolderDiscoveryReady, CapacityBytes: task.volume.CapacityBytes,
			FreeBytes: task.volume.FreeBytes, DiscoveryRevision: 88,
			Folders: []worker.MediaFolderObservation{{Folder: "current", VideoCount: 1}},
		}, nil
	}

	observations, err := probe.Observe(context.Background(), []string{oldVolume.StorageID}, []device.ConnectedVolume{oldVolume})
	if err != nil || len(observations) != 1 || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning {
		t.Fatalf("old source discovery did not start: observations=%+v err=%v", observations, err)
	}
	select {
	case storageID := <-started:
		if storageID != oldVolume.StorageID {
			t.Fatalf("unexpected first scan: %s", storageID)
		}
	case <-time.After(time.Second):
		t.Fatal("old source discovery did not enter the scanner")
	}

	observations, err = probe.Observe(context.Background(), []string{oldVolume.StorageID}, nil)
	if err != nil || len(observations) != 0 {
		t.Fatalf("detached source remained observable: observations=%+v err=%v", observations, err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("detached source discovery was not canceled")
	}

	observations, err = probe.Observe(context.Background(), []string{newVolume.StorageID}, []device.ConnectedVolume{newVolume})
	if err != nil || len(observations) != 1 || observations[0].StorageID != newVolume.StorageID || observations[0].DiscoveryState != vocabulary.FolderDiscoveryScanning {
		t.Fatalf("new source did not receive an isolated scan: observations=%+v err=%v", observations, err)
	}
	observations = waitForDiscoveryState(t, probe, newVolume, vocabulary.FolderDiscoveryReady)
	if observations[0].StorageID != newVolume.StorageID || observations[0].DiscoveryRevision != 88 ||
		len(observations[0].Folders) != 1 || observations[0].Folders[0].Folder != "current" {
		t.Fatalf("reused mount published stale source identity: %+v", observations)
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

type deniedDirectory struct{ *archive.ReadOnlyStorageRoot }

func (root deniedDirectory) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "camera-B" {
		return nil, fs.ErrPermission
	}
	return root.ReadOnlyStorageRoot.ReadDir(name)
}

// Requirement: DEV-017
func TestIncompleteInventoryRevokesReadyRevisionAndIdentifiesUnreadableFolder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"camera-A", "camera-B", ".ignored"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "video.mov"), []byte("clip"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := archive.OpenReadOnlyStorageRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	base := worker.StorageObservation{StorageID: "source", Connected: true}
	good, err := observeRoot(context.Background(), base, root, 1000, 500, nil)
	if err != nil || len(good.Folders) != 2 {
		t.Fatalf("initial scan: %+v %v", good, err)
	}
	failed, err := observeRoot(context.Background(), base, deniedDirectory{root}, 1000, 500, nil)
	if err == nil {
		t.Fatal("incomplete source scan accepted")
	}
	task := attachedScanTask{storageID: "source", attempt: 2}
	probe := NewAttachedProbe()
	probe.states["source"] = &attachedScanState{attempt: 2, ready: true, scanning: true, observation: good}
	probe.finish(task, failed, false)
	result := probe.states["source"].observation
	if result.DiscoveryState != vocabulary.FolderDiscoveryFailed || result.DiscoveryRevision != 0 || len(result.Folders) != 0 || result.DiscoveryGap == nil || result.DiscoveryGap.Count != 1 || result.DiscoveryGap.FirstFolder != "camera-B" || result.DiscoveryGap.Cause != "DIRECTORY_PERMISSION_DENIED" {
		t.Fatalf("incomplete scan misreported: %+v", result)
	}
}
