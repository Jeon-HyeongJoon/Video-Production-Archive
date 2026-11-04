package mediafolder

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"videoarchive/internal/core/archive"
)

func TestStorageRootScannerReturnsSupportedRegularVideosWithoutFollowingLinks(t *testing.T) {
	directory := t.TempDir()
	for _, relative := range []string{"event-a/C0002.MP4", "event-a/C0001.MP4", "event-a/notes.txt", "nested/event-b/final.mov", ".hidden/hidden.mp4"} {
		fullPath := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "escape.mp4"), []byte("escape"), 0o600); err != nil {
		t.Fatal(err)
	}
	createDirectoryLink(t, external, filepath.Join(directory, "linked"))
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	scanner, err := NewScanner(root)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := scanner.Discover()
	if err != nil {
		t.Fatal(err)
	}
	want := []Folder{
		{RelativePath: "event-a", VideoPaths: []string{"event-a/C0001.MP4", "event-a/C0002.MP4"}},
		{RelativePath: "nested/event-b", VideoPaths: []string{"nested/event-b/final.mov"}},
	}
	if !reflect.DeepEqual(folders, want) {
		t.Fatalf("folders=%+v want=%+v", folders, want)
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
