//go:build windows

package archive

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Requirement: ARC-014
func TestWindowsStorageRootPublishesVerifiedOutputWithWritableSyncHandle(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}

	directory := t.TempDir()
	root, err := OpenStorageRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll("outputs", 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified Windows output")
	if err := root.WriteFile("outputs/result.partial", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("outputs/result.partial", "outputs/result.txt"); err != nil {
		t.Fatal(err)
	}
	stored, err := root.ReadFile("outputs/result.txt")
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("published output=%q error=%v", stored, err)
	}
	if exists, err := root.Exists("outputs/result.partial"); err != nil || exists {
		t.Fatalf("temporary output remains: exists=%v error=%v", exists, err)
	}
}

// Requirement: ARC-008
func TestWindowsStorageRootRejectsJunctionOutsideRegisteredRoot(t *testing.T) {
	if os.Getenv("ARCHIVE_WINDOWS_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}

	base := t.TempDir()
	registered := filepath.Join(base, "registered")
	external := filepath.Join(base, "external")
	if err := os.MkdirAll(registered, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	externalPath := filepath.Join(external, "outside.txt")
	want := []byte("outside remains unchanged")
	if err := os.WriteFile(externalPath, want, 0o644); err != nil {
		t.Fatal(err)
	}

	junction := filepath.Join(registered, "outside-link")
	output, err := exec.Command("cmd.exe", "/d", "/s", "/c", "mklink", "/J", junction, external).CombinedOutput()
	if err != nil {
		t.Fatalf("create Windows junction: %v: %s", err, output)
	}

	root, err := OpenStorageRoot(registered)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, openErr := root.Open(filepath.Join("outside-link", "outside.txt"))
	if file != nil {
		_ = file.Close()
		t.Fatal("StorageRoot returned a file handle through a junction")
	}
	if !errors.Is(openErr, ErrUnsafeStoragePath) {
		t.Fatalf("junction access error=%v want=%v", openErr, ErrUnsafeStoragePath)
	}
	got, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("external file changed: got=%q want=%q", got, want)
	}
}
