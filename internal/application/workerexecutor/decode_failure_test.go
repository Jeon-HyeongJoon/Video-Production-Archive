package workerexecutor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"videoarchive/internal/adapter/ffmpeg"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/workflow"
)

type decoderSample struct {
	stderr string
	exit   error
}

type processExit int

func (e processExit) Error() string { return "process exited" }
func (e processExit) ExitCode() int { return int(e) }

func (r decoderSample) Run(context.Context, string, []string) error { return r.exit }
func (r decoderSample) RunStreams(_ context.Context, _ string, _ []string, stdout, stderr io.Writer) error {
	io.WriteString(stdout, "out_time_us=1518917400\n")
	for i := 0; i < len(r.stderr); i += 13 {
		io.WriteString(stderr, r.stderr[i:min(i+13, len(r.stderr))])
	}
	return r.exit
}

// Requirement: WRK-022
func TestWorkerDistinguishesTerminalSourceDecodeFailureFromWarnings(t *testing.T) {
	t.Run("real decoder and persisted retry decision", testTerminalDecode)
	t.Run("healthy input with unwritable output", testRenderOutputFailure)
	t.Run("cancelled render preserves verified sibling", testCancelledRecording)
	const packet = "[vist#0:0/h264 @ 123] [dec:h264 @ 456] Error submitting packet to decoder: Invalid data found when processing input\n"
	const mismatch = "[aac @ 123] Sample rate index (8) in program config element does not match the sample rate index (3) configured by the container.\n"
	const filter = "[af#0:1 @ 123] Terminating thread with return code -22 (Invalid argument)\n"
	const decoder = "[vist#0:0/h264 @ 123] [dec:h264 @ 456] Terminating thread with return code -1094995529 (Invalid data found when processing input)\n"
	const rate = "[vist#0:0/h264 @ 123] [dec:h264 @ 456] Decode error rate 0.833333 exceeds maximum 0.666667\n"
	for _, tc := range []struct {
		name, lines string
		code        int
		input       bool
	}{
		{"damaged recording", packet + mismatch + filter, -22, true}, {"fatal decoder", packet + decoder, 1, true},
		{"output failure after warnings", packet + mismatch + "[out#0/mp4] No space left on device\n", 1, false},
		{"filter configuration", filter, -22, false}, {"recoverable warning", packet + mismatch + filter, 0, false},
		{"exit 69 alone", "", 69, false}, {"packet warning with exit 69", packet, 69, false},
		{"rate exceeded in another input", packet + strings.ReplaceAll(rate, "vist#0", "vist#1"), 69, false},
		{"decoder rate exceeded", packet + rate, 69, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var exit error
			if tc.code != 0 {
				exit = processExit(tc.code)
			}
			err := ffmpeg.WithProgress(decoderSample{tc.lines, exit}, execution.OperationRenderRecording, 2, 5, 1755887).Run(context.Background(), "ffmpeg", nil)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			result := operationFailureResult(execution.StepExecutionSpec{Operation: execution.OperationRenderRecording}, err)
			if tc.input {
				if result.FailureClass != workflow.FailureInput || result.CauseCode != "SOURCE_DECODE_FAILED" || !strings.Contains(result.ErrorSummary, "5개 중 2번째") || !strings.Contains(result.ErrorSummary, "1518초") {
					t.Fatal(result)
				}
			} else if result.FailureClass != workflow.FailureTransient || result.CauseCode == "SOURCE_DECODE_FAILED" {
				t.Fatal(result)
			}
		})
	}
	sequence := strings.ReplaceAll(packet+rate, "vist#0:0", "vist#1:0")
	err := ffmpeg.WithProgress(decoderSample{sequence, processExit(69)}, execution.OperationRenderSourceSequence, 1, 1, 3000).Run(
		context.Background(), "ffmpeg", []string{"-i", "first", "-i", "second", "-i", "third"})
	var failure *ffmpeg.DecodeFailure
	if !errors.As(err, &failure) || failure.InputIndex != 2 || failure.InputCount != 3 || failure.OutputMillis != 1518917 {
		t.Fatalf("decoder input does not match sequence order: %v", err)
	}
}
