package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Requirement: DSH-041
func TestTrayClickOpensConfiguredArchiveWorkspaceInChromeNewTab(t *testing.T) {
	chrome := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(chrome, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := trayConfiguration{
		DashboardURL:     "https://100.121.90.20:8443/work/jobs",
		ChromeExecutable: chrome,
	}
	if err := configuration.validate(); err != nil {
		t.Fatal(err)
	}

	var executable string
	var arguments []string
	launches := 0
	click := func() error {
		return configuration.openDashboard(func(name string, values ...string) error {
			launches++
			executable = name
			arguments = append([]string(nil), values...)
			return nil
		})
	}
	if err := click(); err != nil {
		t.Fatal(err)
	}
	if launches != 1 || executable != chrome || !reflect.DeepEqual(arguments, []string{"--new-tab", configuration.DashboardURL}) {
		t.Fatalf("unexpected Chrome invocation: launches=%d executable=%q arguments=%q", launches, executable, arguments)
	}
}
