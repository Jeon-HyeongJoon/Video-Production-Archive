package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type catalog struct {
	Requirements []requirement `json:"requirements"`
}

type requirement struct {
	ID       string `json:"id"`
	TestName string `json:"test_name"`
	TestFile string `json:"test_file"`
	Status   string `json:"status"`
	Level    string `json:"level"`
}

type goTestEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
}

type testResult struct {
	Package       string  `json:"package"`
	TestName      string  `json:"test_name"`
	TestCategory  string  `json:"test_category"`
	RequirementID string  `json:"requirement_id,omitempty"`
	Level         string  `json:"level,omitempty"`
	Result        string  `json:"result"`
	DurationSec   float64 `json:"duration_seconds"`
	FinishedAt    string  `json:"finished_at,omitempty"`
	LogRef        string  `json:"log_ref"`
}

type runMetadata struct {
	RunID      string
	Revision   string
	Repository string
}

type runRecord struct {
	RunID      string `json:"run_id"`
	Revision   string `json:"revision"`
	Repository string `json:"repository,omitempty"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	GoVersion  string `json:"go_version"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Outcome    string `json:"outcome"`
	TestCount  int    `json:"test_count"`
	ResultsRef string `json:"results_ref"`
	RawLogRef  string `json:"raw_log_ref"`
}

func main() {
	inputPath := flag.String("input", "", "path to go test -json output")
	catalogPath := flag.String("catalog", "requirements/catalog.json", "path to requirement catalog")
	outputDirectory := flag.String("output", "", "directory for run.json and results.jsonl")
	runID := flag.String("run-id", "", "CI run identifier")
	revision := flag.String("revision", "", "tested git revision")
	repository := flag.String("repository", "", "repository identifier")
	operational := flag.Bool("operational", false, "run the lightweight operational checks, or check their results in -input")
	flag.Parse()
	if *operational {
		if err := checkOperational(*catalogPath, *inputPath, operationalIDs); err != nil {
			fatal(err)
		}
		return
	}

	if *inputPath == "" || *outputDirectory == "" || *runID == "" || *revision == "" {
		fatal(errors.New("input, output, run-id and revision are required"))
	}
	if err := generate(*inputPath, *catalogPath, *outputDirectory, runMetadata{
		RunID: *runID, Revision: *revision, Repository: *repository,
	}); err != nil {
		fatal(err)
	}
}

func generate(inputPath, catalogPath, outputDirectory string, metadata runMetadata) error {
	catalogPayload, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("read requirement catalog: %w", err)
	}
	var entries catalog
	if err := json.Unmarshal(catalogPayload, &entries); err != nil {
		return fmt.Errorf("decode requirement catalog: %w", err)
	}

	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open Go test log: %w", err)
	}
	defer input.Close()

	run, results, err := normalize(input, entries.Requirements, metadata)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
		return fmt.Errorf("create evidence directory: %w", err)
	}
	if err := copyRawLog(inputPath, filepath.Join(outputDirectory, "go-test.jsonl")); err != nil {
		return err
	}
	if err := writeRun(filepath.Join(outputDirectory, "run.json"), run); err != nil {
		return err
	}
	if err := writeResults(filepath.Join(outputDirectory, "results.jsonl"), results); err != nil {
		return err
	}
	return nil
}

func copyRawLog(sourcePath, destinationPath string) error {
	sourceAbsolute, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve Go test log: %w", err)
	}
	destinationAbsolute, err := filepath.Abs(destinationPath)
	if err != nil {
		return fmt.Errorf("resolve evidence log: %w", err)
	}
	if sourceAbsolute == destinationAbsolute {
		return nil
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("reopen Go test log: %w", err)
	}
	defer source.Close()
	destination, err := os.Create(destinationPath)
	if err != nil {
		return fmt.Errorf("create evidence log: %w", err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return fmt.Errorf("copy evidence log: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close evidence log: %w", err)
	}
	return nil
}

func normalize(input io.Reader, requirements []requirement, metadata runMetadata) (runRecord, []testResult, error) {
	byTestName := make(map[string]requirement, len(requirements))
	for _, item := range requirements {
		if item.TestName != "" {
			byTestName[item.TestName] = item
		}
	}

	decoder := json.NewDecoder(input)
	resultsByKey := make(map[string]testResult)
	var startedAt time.Time
	var finishedAt time.Time
	outcome := "passed"
	for {
		var event goTestEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return runRecord{}, nil, fmt.Errorf("decode Go test event: %w", err)
		}
		if !event.Time.IsZero() && (startedAt.IsZero() || event.Time.Before(startedAt)) {
			startedAt = event.Time
		}
		if !event.Time.IsZero() && event.Time.After(finishedAt) {
			finishedAt = event.Time
		}
		if event.Action == "fail" {
			outcome = "failed"
		}
		if event.Test == "" || (event.Action != "pass" && event.Action != "fail" && event.Action != "skip") {
			continue
		}

		result := testResult{
			Package: event.Package, TestName: event.Test, TestCategory: "tooling",
			Result: event.Action, DurationSec: event.Elapsed, LogRef: "go-test.jsonl",
		}
		if !event.Time.IsZero() {
			result.FinishedAt = event.Time.UTC().Format(time.RFC3339Nano)
		}
		rootName := strings.SplitN(event.Test, "/", 2)[0]
		if linked, found := byTestName[rootName]; found {
			result.TestCategory = "requirement"
			result.RequirementID = linked.ID
			result.Level = linked.Level
		}
		resultsByKey[event.Package+"\x00"+event.Test] = result
	}

	results := make([]testResult, 0, len(resultsByKey))
	for _, result := range resultsByKey {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Package == results[j].Package {
			return results[i].TestName < results[j].TestName
		}
		return results[i].Package < results[j].Package
	})

	run := runRecord{
		RunID: metadata.RunID, Revision: metadata.Revision, Repository: metadata.Repository,
		OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), Outcome: outcome,
		TestCount: len(results), ResultsRef: "results.jsonl", RawLogRef: "go-test.jsonl",
	}
	if !startedAt.IsZero() {
		run.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !finishedAt.IsZero() {
		run.FinishedAt = finishedAt.UTC().Format(time.RFC3339Nano)
	}
	return run, results, nil
}

func writeRun(path string, run runRecord) error {
	payload, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run evidence: %w", err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write run evidence: %w", err)
	}
	return nil
}

func writeResults(path string, results []testResult) error {
	output, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create test results: %w", err)
	}
	writer := bufio.NewWriter(output)
	encoder := json.NewEncoder(writer)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			output.Close()
			return fmt.Errorf("encode test result: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		output.Close()
		return fmt.Errorf("flush test results: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close test results: %w", err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
