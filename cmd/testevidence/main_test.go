package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Requirement: TST-001
func TestCIEvidenceBundleRecordsEveryExecutedTestWithRequirementContext(t *testing.T) {
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "go-test.jsonl")
	catalogPath := filepath.Join(directory, "catalog.json")
	outputDirectory := filepath.Join(directory, "evidence")

	writeFixture(t, inputPath, `
{"Time":"2026-10-26T01:00:00Z","Action":"run","Package":"videoarchive/internal/core/pipeline","Test":"TestRequirementBehavior"}
{"Time":"2026-10-26T01:00:01Z","Action":"pass","Package":"videoarchive/internal/core/pipeline","Test":"TestRequirementBehavior","Elapsed":1}
{"Time":"2026-10-26T01:00:01Z","Action":"run","Package":"videoarchive/cmd/reqcheck","Test":"TestToolingBehavior"}
{"Time":"2026-10-26T01:00:02Z","Action":"skip","Package":"videoarchive/cmd/reqcheck","Test":"TestToolingBehavior","Elapsed":1}
{"Time":"2026-10-26T01:00:02Z","Action":"pass","Package":"videoarchive/cmd/reqcheck","Elapsed":2}
`)
	writeFixture(t, catalogPath, `{
  "requirements": [{
    "id": "PIP-999",
    "test_name": "TestRequirementBehavior",
    "level": "integration"
  }]
}`)

	err := generate(inputPath, catalogPath, outputDirectory, runMetadata{
		RunID: "123-1", Revision: "abc123", Repository: "office/archive",
	})
	if err != nil {
		t.Fatal(err)
	}

	runPayload, err := os.ReadFile(filepath.Join(outputDirectory, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var run runRecord
	if err := json.Unmarshal(runPayload, &run); err != nil {
		t.Fatal(err)
	}
	if run.RunID != "123-1" || run.Revision != "abc123" || run.Outcome != "passed" || run.TestCount != 2 ||
		run.ResultsRef != "results.jsonl" || run.RawLogRef != "go-test.jsonl" {
		t.Fatalf("incomplete CI run evidence: %+v", run)
	}
	if _, err := os.Stat(filepath.Join(outputDirectory, run.RawLogRef)); err != nil {
		t.Fatalf("raw Go test evidence was not copied: %v", err)
	}

	resultsFile, err := os.Open(filepath.Join(outputDirectory, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer resultsFile.Close()
	var results []testResult
	scanner := bufio.NewScanner(resultsFile)
	for scanner.Scan() {
		var result testResult
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d test evidence records, want 2", len(results))
	}
	if results[0].TestName != "TestToolingBehavior" || results[0].TestCategory != "tooling" || results[0].RequirementID != "" || results[0].Result != "skip" {
		t.Fatalf("tooling test evidence is invalid: %+v", results[0])
	}
	if results[1].TestName != "TestRequirementBehavior" || results[1].TestCategory != "requirement" ||
		results[1].RequirementID != "PIP-999" || results[1].Level != "integration" || results[1].Result != "pass" {
		t.Fatalf("requirement test evidence is invalid: %+v", results[1])
	}
}

func writeFixture(t *testing.T, path, payload string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Requirement: TST-002
func TestOperationalChecksRequireEverySelectedCatalogTestToPass(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	writeFixture(t, catalogPath, `{"requirements":[
		{"id":"A-001","status":"implemented","level":"integration","test_file":"internal/a/a_test.go","test_name":"TestA"},
		{"id":"B-001","status":"implemented","level":"integration","test_file":"internal/b/b_test.go","test_name":"TestB"}]}`)
	const first = `{"Action":"pass","Package":"videoarchive/internal/a","Test":"TestA"}` + "\n"
	for _, tc := range []struct {
		name     string
		second   string
		wantPass bool
	}{
		{"all passed", `{"Action":"pass","Package":"videoarchive/internal/b","Test":"TestB"}`, true},
		{"missing", "", false},
		{"skipped", `{"Action":"skip","Package":"videoarchive/internal/b","Test":"TestB"}`, false},
		{"failed", `{"Action":"fail","Package":"videoarchive/internal/b","Test":"TestB"}`, false},
		{"same name in another package", `{"Action":"pass","Package":"videoarchive/internal/a","Test":"TestB"}`, false},
		{"skip followed by pass", `{"Action":"skip","Package":"videoarchive/internal/b","Test":"TestB"}
{"Action":"pass","Package":"videoarchive/internal/b","Test":"TestB"}`, false},
		{"only child passed", `{"Action":"pass","Package":"videoarchive/internal/b","Test":"TestB/child"}`, false},
		{"skipped case in passing test", `{"Action":"skip","Package":"videoarchive/internal/b","Test":"TestB/child"}
{"Action":"pass","Package":"videoarchive/internal/b","Test":"TestB"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "test.jsonl")
			writeFixture(t, input, first+tc.second)
			err := checkOperational(catalogPath, input, []string{"A-001", "B-001"})
			if (err == nil) != tc.wantPass {
				t.Fatalf("pass=%t, error=%v", tc.wantPass, err)
			}
		})
	}
}
