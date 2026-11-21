package processlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
)

// Requirement: WRK-019
func TestProcessRunnerCapturesBoundedSanitizedFailureArtifactAndRetention(t *testing.T) {
	temporary := t.TempDir()
	root, err := archive.OpenStorageRoot(temporary)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(path.Join(directory, "PLOG-OLD.log"), []byte(strings.Repeat("o", 1500)), 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(root, Policy{MaxFileBytes: 1024, TailBytes: 1024, Retention: time.Hour, MaxTotalBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	contextWithBoundary := execution.WithProcessContext(context.Background(), execution.ProcessContext{
		JobID: "JOB-PROCESS-LOG", StepID: "render", Operation: execution.OperationRenderRecording, FencingToken: 7,
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = runner.Run(contextWithBoundary, executable, []string{"-test.run=^TestProcessLogHelper$", "--", "process-log-fail"})
	var failure *execution.ProcessFailure
	if !errors.As(err, &failure) {
		t.Fatalf("expected process failure, got %v", err)
	}
	artifact := failure.Artifact
	if artifact.Validate() != nil || artifact.State != execution.ProcessLogAvailable || artifact.CapturedBytes != 1024 ||
		artifact.TotalBytes <= artifact.CapturedBytes || !artifact.Truncated || artifact.Digest == "" ||
		artifact.Reference != "evidence://process-log/"+artifact.ID {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}
	if strings.Contains(strings.ToLower(artifact.SanitizedTail), "test1234") || strings.Contains(artifact.SanitizedTail, `C:\Users`) ||
		strings.Contains(artifact.SanitizedTail, "/Users/") || !strings.Contains(artifact.SanitizedTail, "fatal codec mismatch") {
		t.Fatalf("tail was not useful and sanitized: %q", artifact.SanitizedTail)
	}
	if _, err := root.Open(path.Join(directory, "PLOG-OLD.log")); err == nil {
		t.Fatal("retention budget did not remove the oldest log")
	}
	info, err := os.Stat(path.Join(temporary, directory, artifact.ID+".log"))
	if err != nil || info.Size() != artifact.CapturedBytes {
		t.Fatalf("bounded raw log missing: info=%v err=%v", info, err)
	}

	successContext := execution.WithProcessContext(context.Background(), execution.ProcessContext{
		JobID: "JOB-PROCESS-LOG", StepID: "render", Operation: execution.OperationRenderRecording, FencingToken: 8,
	})
	if err := runner.Run(successContext, executable, []string{"-test.run=^TestProcessLogHelper$", "--", "process-log-success"}); err != nil {
		t.Fatal(err)
	}
	successID := processLogID(execution.ProcessContext{JobID: "JOB-PROCESS-LOG", StepID: "render", Operation: execution.OperationRenderRecording, FencingToken: 8})
	if _, err := os.Stat(path.Join(temporary, directory, successID+".log")); !os.IsNotExist(err) {
		t.Fatalf("successful process log was retained: %v", err)
	}

	probeContext := execution.WithProcessContext(context.Background(), execution.ProcessContext{
		JobID: "JOB-PROCESS-LOG", StepID: "inspect", Operation: execution.OperationInspectMedia, FencingToken: 9,
	})
	if _, err := runner.Output(probeContext, executable, []string{"-test.run=^TestProcessLogHelper$", "--", "process-log-fail"}); !errors.As(err, &failure) || failure.Artifact.State != execution.ProcessLogAvailable {
		t.Fatalf("ffprobe-style failure did not retain evidence: %v", err)
	}
	probeSuccessContext := execution.WithProcessContext(context.Background(), execution.ProcessContext{
		JobID: "JOB-PROCESS-LOG", StepID: "inspect", Operation: execution.OperationInspectMedia, FencingToken: 10,
	})
	output, err := runner.Output(probeSuccessContext, executable, []string{"-test.run=^TestProcessLogHelper$", "--", "process-log-output-success"})
	if err != nil || !strings.HasPrefix(string(output), `{"ok":true}`) {
		t.Fatalf("ffprobe-style success output mismatch: output=%q err=%v", output, err)
	}
}

func TestProcessLogHelper(t *testing.T) {
	mode := ""
	for _, argument := range os.Args {
		if strings.HasPrefix(argument, "process-log-") {
			mode = argument
		}
	}
	if mode == "" {
		return
	}
	if mode == "process-log-output-success" {
		fmt.Fprint(os.Stdout, `{"ok":true}`)
		return
	}
	fmt.Fprintln(os.Stderr, `token=test1234 C:\Users\sshuser\secret.mov /Users/hj/private.mov`)
	fmt.Fprintln(os.Stderr, strings.Repeat("x", 2200))
	fmt.Fprintln(os.Stderr, "fatal codec mismatch")
	if mode == "process-log-fail" {
		os.Exit(22)
	}
}
