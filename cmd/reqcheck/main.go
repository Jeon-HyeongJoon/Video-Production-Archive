package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type catalog struct {
	Version      int           `json:"version"`
	Requirements []requirement `json:"requirements"`
}

type requirement struct {
	ID        string `json:"id"`
	Design    string `json:"design"`
	Statement string `json:"statement"`
	Given     string `json:"given"`
	When      string `json:"when"`
	Then      string `json:"then"`
	TestFile  string `json:"test_file"`
	TestName  string `json:"test_name"`
	Status    string `json:"status"`
	Level     string `json:"level"`
}

type testRef struct {
	ID   string
	File string
	Name string
}

type semanticLock struct {
	Version      int               `json:"version"`
	Requirements map[string]string `json:"requirements"`
}

var requirementComment = regexp.MustCompile(`(?m)^Requirement:\s*([A-Z]{3}-[0-9]{3})\s*$`)

func main() {
	updateLock := flag.Bool("update-lock", false, "replace the reviewed requirement semantic fingerprint lock")
	flag.Parse()
	root, err := repositoryRoot()
	check(err)
	catalogPath := filepath.Join(root, "requirements", "catalog.json")
	payload, err := os.ReadFile(catalogPath)
	check(err)

	var entries catalog
	check(json.Unmarshal(payload, &entries))
	if entries.Version != 2 {
		check(fmt.Errorf("unsupported catalog version %d", entries.Version))
	}
	lockPath := filepath.Join(root, "requirements", "semantic-lock.json")
	if *updateLock {
		check(writeSemanticLock(lockPath, entries.Requirements))
		fmt.Printf("semantic lock updated: %d requirements\n", len(entries.Requirements))
		return
	}
	lockPayload, err := os.ReadFile(lockPath)
	check(err)
	var lock semanticLock
	check(json.Unmarshal(lockPayload, &lock))
	check(validateSemanticLock(entries.Requirements, lock))

	tests, err := annotatedTests(root)
	check(err)
	implemented, planned, err := validate(root, entries.Requirements, tests)
	check(err)
	fmt.Printf("traceability ok: %d requirements (%d implemented, %d planned), %d one-to-one tests\n", len(entries.Requirements), implemented, planned, len(tests))
}

func writeSemanticLock(path string, requirements []requirement) error {
	lock := semanticLock{Version: 1, Requirements: requirementFingerprints(requirements)}
	payload, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(path, payload, 0o644)
}

func validateSemanticLock(requirements []requirement, lock semanticLock) error {
	if lock.Version != 1 {
		return fmt.Errorf("unsupported semantic lock version %d", lock.Version)
	}
	current := requirementFingerprints(requirements)
	var problems []string
	for id, fingerprint := range current {
		locked, exists := lock.Requirements[id]
		if !exists {
			problems = append(problems, fmt.Sprintf("%s is missing from semantic lock", id))
		} else if locked != fingerprint {
			problems = append(problems, fmt.Sprintf("%s meaning changed without semantic lock review", id))
		}
	}
	for id := range lock.Requirements {
		if _, exists := current[id]; !exists {
			problems = append(problems, fmt.Sprintf("semantic lock contains removed requirement %s", id))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("semantic lock failed:\n- %s\nrun 'go run ./cmd/reqcheck -update-lock' only after reviewing the semantic change", strings.Join(problems, "\n- "))
	}
	return nil
}

func requirementFingerprints(requirements []requirement) map[string]string {
	fingerprints := make(map[string]string, len(requirements))
	for _, req := range requirements {
		meaning := struct {
			Version   int    `json:"version"`
			ID        string `json:"id"`
			Design    string `json:"design"`
			Statement string `json:"statement"`
			Given     string `json:"given"`
			When      string `json:"when"`
			Then      string `json:"then"`
			Status    string `json:"status"`
			Level     string `json:"level"`
		}{
			Version: 1, ID: req.ID, Design: req.Design, Statement: req.Statement,
			Given: req.Given, When: req.When, Then: req.Then, Status: req.Status, Level: req.Level,
		}
		payload, _ := json.Marshal(meaning)
		digest := sha256.Sum256(payload)
		fingerprints[req.ID] = fmt.Sprintf("sha256:%x", digest)
	}
	return fingerprints
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found")
		}
		directory = parent
	}
}

func annotatedTests(root string) ([]testRef, error) {
	var tests []testRef
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(function.Name.Name, "Test") || function.Doc == nil {
				continue
			}
			match := requirementComment.FindStringSubmatch(strings.TrimSpace(function.Doc.Text()))
			if len(match) == 2 {
				tests = append(tests, testRef{ID: match[1], File: filepath.ToSlash(relative), Name: function.Name.Name})
			}
		}
		return nil
	})
	return tests, err
}

func validate(root string, requirements []requirement, tests []testRef) (int, int, error) {
	byID := make(map[string]requirement, len(requirements))
	byTest := make(map[string]string, len(requirements))
	var problems []string
	implemented := 0
	planned := 0
	for _, req := range requirements {
		if req.ID == "" || req.Design == "" || req.Statement == "" || req.Given == "" || req.When == "" || req.Then == "" ||
			req.Status == "" || req.Level == "" {
			problems = append(problems, fmt.Sprintf("%s has an incomplete catalog entry", req.ID))
		}
		if req.Level != "unit" && req.Level != "integration" && req.Level != "acceptance" {
			problems = append(problems, fmt.Sprintf("%s has unknown test level %q", req.ID, req.Level))
		}
		switch req.Status {
		case "implemented":
			implemented++
			if req.TestFile == "" || req.TestName == "" {
				problems = append(problems, fmt.Sprintf("%s is implemented but has no test reference", req.ID))
			}
		case "planned":
			planned++
			if req.TestFile != "" || req.TestName != "" {
				problems = append(problems, fmt.Sprintf("%s is planned and must not claim a test", req.ID))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s has unknown status %q", req.ID, req.Status))
		}
		if _, duplicate := byID[req.ID]; duplicate {
			problems = append(problems, fmt.Sprintf("duplicate requirement %s", req.ID))
		}
		byID[req.ID] = req
		if req.Status == "implemented" && req.TestFile != "" && req.TestName != "" {
			key := filepath.ToSlash(req.TestFile) + "::" + req.TestName
			if previous, duplicate := byTest[key]; duplicate {
				problems = append(problems, fmt.Sprintf("requirements %s and %s share %s", previous, req.ID, key))
			}
			byTest[key] = req.ID
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(req.TestFile))); err != nil {
				problems = append(problems, fmt.Sprintf("%s points to missing file %s", req.ID, req.TestFile))
			}
		}
	}

	seen := make(map[string]int, len(tests))
	for _, test := range tests {
		seen[test.ID]++
		req, exists := byID[test.ID]
		if !exists {
			problems = append(problems, fmt.Sprintf("%s::%s references unknown requirement %s", test.File, test.Name, test.ID))
			continue
		}
		if req.Status != "implemented" {
			problems = append(problems, fmt.Sprintf("%s::%s references planned requirement %s", test.File, test.Name, test.ID))
			continue
		}
		if req.TestFile != test.File || req.TestName != test.Name {
			problems = append(problems, fmt.Sprintf("%s catalog/test mismatch: catalog=%s::%s actual=%s::%s", req.ID, req.TestFile, req.TestName, test.File, test.Name))
		}
	}
	for id, req := range byID {
		if req.Status == "implemented" && seen[id] != 1 {
			problems = append(problems, fmt.Sprintf("%s has %d annotated tests; want exactly 1", id, seen[id]))
		} else if req.Status == "planned" && seen[id] != 0 {
			problems = append(problems, fmt.Sprintf("%s is planned but has %d annotated tests", id, seen[id]))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return implemented, planned, fmt.Errorf("traceability failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return implemented, planned, nil
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
