package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCodexLoginProbeUsesConfiguredCLIAccount(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "fake-codex")
	if runtime.GOOS == "windows" {
		fixture += ".exe"
	}
	build := exec.Command("go", "build", "-o", fixture, "../../internal/adapter/codexcli/testdata/fakecodex")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Codex fixture: %v: %s", err, output)
	}
	if !codexLoginAvailable(context.Background(), fixture) {
		t.Fatal("logged-in Codex fixture was reported unavailable")
	}
	t.Setenv("ARCHIVE_CODEX_FIXTURE_LOGIN_MISSING", "1")
	if codexLoginAvailable(context.Background(), fixture) {
		t.Fatal("missing Codex login was reported available")
	}
}
