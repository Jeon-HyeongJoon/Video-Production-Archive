package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Use requirement IDs so renaming or moving a test follows the reviewed catalog.
var operationalIDs = []string{
	"INT-001", "ORG-002", "ORG-019", "ORG-021", "ORG-023", "ORG-024", "ORG-025", "ORG-026", "ORG-027", "ORG-028", "ORG-029",
	"APP-006", "APP-011", "RND-005", "RND-006", "RND-007", "QCT-005",
	"APP-008", "AUD-002", "TRN-002", "TRN-006", "TRN-008", "TRN-009", "TRN-010", "SUB-001", "SUB-002",
	"ARC-011", "ARC-016", "ARC-017", "ARC-018", "ARC-021", "ARC-022", "ARC-023", "ARC-024", "ARC-025", "DSH-046", "DSH-054", "DSH-055",
	"DSH-056", "MCP-011", "MCP-014", "MCP-015", "MCP-017", "MCP-018", "MCP-019", "OBS-003", "WRK-018", "WRK-022",
}

func checkOperational(catalogPath, inputPath string, ids []string) error {
	payload, err := os.ReadFile(catalogPath)
	if err != nil {
		return err
	}
	var entries catalog
	if err := json.Unmarshal(payload, &entries); err != nil {
		return err
	}
	var selected []requirement
	var patterns, packages []string
	for _, id := range ids {
		index := slices.IndexFunc(entries.Requirements, func(r requirement) bool { return r.ID == id })
		if index == -1 {
			return fmt.Errorf("missing operational requirement %s", id)
		}
		r := entries.Requirements[index]
		if r.Status != "implemented" || r.TestFile == "" || r.TestName == "" || (r.Level != "unit" && r.Level != "integration") {
			return fmt.Errorf("%s needs a portable, catalogued test", id)
		}
		selected = append(selected, r)
		patterns = append(patterns, regexp.QuoteMeta(r.TestName))
		packages = append(packages, "./"+path.Dir(r.TestFile))
	}
	var input io.Reader
	if inputPath != "" {
		file, err := os.Open(inputPath)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	} else {
		slices.Sort(packages)
		args := []string{"test", "-json", "-count=1", "-run", "^(" + strings.Join(patterns, "|") + ")$"}
		cmd := exec.Command("go", append(args, slices.Compact(packages)...)...)
		cmd.Stderr = os.Stderr
		log, err := cmd.Output()
		if err != nil {
			os.Stderr.Write(log)
			return fmt.Errorf("operational go test: %w", err)
		}
		input = bytes.NewReader(log)
	}
	module, err := exec.Command("go", "list", "-m", "-f", "{{.Path}}").Output()
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}
	if err := requireOperationalPasses(input, strings.TrimSpace(string(module)), selected); err != nil {
		return err
	}
	fmt.Printf("operational checks passed: %d requirements\n", len(selected))
	return nil
}

func requireOperationalPasses(input io.Reader, module string, selected []requirement) error {
	type testKey struct{ pkg, name string }
	required := make(map[testKey]string, len(selected))
	passed := make(map[testKey]bool)
	for _, r := range selected {
		required[testKey{path.Join(module, path.Dir(r.TestFile)), r.TestName}] = r.ID
	}

	var problems []error
	decoder := json.NewDecoder(input)
	for {
		var event goTestEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode Go test event: %w", err)
		}
		root, _, child := strings.Cut(event.Test, "/")
		key := testKey{event.Package, root}
		id, tracked := required[key]
		if !tracked {
			continue
		}
		switch event.Action {
		case "pass":
			if !child {
				passed[key] = true
			}
		case "skip", "fail":
			problems = append(problems, fmt.Errorf("%s %s/%s: %s", id, event.Package, event.Test, event.Action))
		}
	}
	for _, r := range selected {
		key := testKey{path.Join(module, path.Dir(r.TestFile)), r.TestName}
		if !passed[key] {
			problems = append(problems, fmt.Errorf("%s %s/%s: missing pass", r.ID, key.pkg, key.name))
		}
	}
	return errors.Join(problems...)
}
