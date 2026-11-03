package processruntime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"videoarchive/internal/core/archivistruntime"
)

func TestRunnerStreamsStructuredProgressBeforeFinalResponse(t *testing.T) {
	plan := archivistruntime.LaunchPlan{
		Executable:       os.Args[0],
		Arguments:        []string{"-test.run=TestProcessRuntimeHelper", "--"},
		WorkingDirectory: t.TempDir(),
	}
	started := 0
	var progress []string
	response, err := New().Run(
		context.Background(),
		plan,
		"archive this drive",
		"http://127.0.0.1:1",
		map[string]string{"ARCHIVE_RUNTIME_TEST_MODE": "valid"},
		func() error { started++; return nil },
		func(update archivistruntime.ProgressUpdate) error {
			progress = append(progress, update.Summary)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || strings.Join(progress, ",") != "폴더 조회,분류 완료" {
		t.Fatalf("unexpected callbacks: started=%d progress=%v", started, progress)
	}
	if response.Summary != "아카이브 실행 시작" || len(response.ResultIDs) != 1 || response.ResultIDs[0] != "ARCHIVE-RUN-1" {
		t.Fatalf("unexpected final response: %+v", response)
	}
}

func TestRunnerRejectsUnrestrictedRuntimeEventFields(t *testing.T) {
	plan := archivistruntime.LaunchPlan{
		Executable:       os.Args[0],
		Arguments:        []string{"-test.run=TestProcessRuntimeHelper", "--"},
		WorkingDirectory: t.TempDir(),
	}
	_, err := New().Run(
		context.Background(),
		plan,
		"archive this drive",
		"http://127.0.0.1:1",
		map[string]string{"ARCHIVE_RUNTIME_TEST_MODE": "raw-field"},
		func() error { return nil },
		func(archivistruntime.ProgressUpdate) error { return nil },
	)
	if err == nil || err.Error() != "runtime event is invalid" {
		t.Fatalf("expected raw field rejection, got %v", err)
	}
}

func TestProcessRuntimeHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Getenv("ARCHIVE_RUNTIME_TEST_MODE") == "" {
		return
	}
	switch os.Getenv("ARCHIVE_RUNTIME_TEST_MODE") {
	case "valid":
		fmt.Fprintln(os.Stderr, "credential=must-not-be-forwarded")
		fmt.Println(`{"type":"progress","summary":"폴더 조회"}`)
		fmt.Println(`{"type":"progress","summary":"분류 완료"}`)
		fmt.Println(`{"type":"final","summary":"아카이브 실행 시작","result_ids":["ARCHIVE-RUN-1"]}`)
	case "raw-field":
		fmt.Println(`{"type":"progress","summary":"폴더 조회","stdout":"secret"}`)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
