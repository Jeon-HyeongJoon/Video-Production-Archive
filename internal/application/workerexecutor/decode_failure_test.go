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
	const packet = "[vist#0:0/h264 @ 123] [dec:h264 @ 456] Error submitting packet to decoder: Invalid data found when processing input\n"
	const mismatch = "[aac @ 123] Sample rate index (8) in program config element does not match the sample rate index (3) configured by the container.\n"
	const filter = "[af#0:1 @ 123] Terminating thread with return code -22 (Invalid argument)\n"
	const decoder = "[vist#0:0/h264 @ 123] [dec:h264 @ 456] Terminating thread with return code -1094995529 (Invalid data found when processing input)\n"
	for _, tc := range []struct {
		name, lines    string
		success, input bool
	}{
		{"damaged recording", packet + mismatch + filter, false, true}, {"fatal decoder", packet + decoder, false, true},
		{"output failure after warnings", packet + mismatch + "[out#0/mp4] No space left on device\n", false, false},
		{"filter configuration", filter, false, false}, {"recoverable warning", packet + mismatch + filter, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var exit error
			if !tc.success {
				exit = errors.New("process exited")
			}
			err := ffmpeg.WithProgress(decoderSample{tc.lines, exit}, execution.OperationRenderRecording, 2, 5, 1755887).Run(context.Background(), "ffmpeg", nil)
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			result := operationFailureResult(execution.OperationRenderRecording, err)
			if tc.input {
				if result.FailureClass != workflow.FailureInput || result.CauseCode != "SOURCE_DECODE_FAILED" || !strings.Contains(result.ErrorSummary, "5개 중 2번째") || !strings.Contains(result.ErrorSummary, "1518초") {
					t.Fatal(result)
				}
			} else if result.FailureClass != workflow.FailureTransient || result.CauseCode == "SOURCE_DECODE_FAILED" {
				t.Fatal(result)
			}
		})
	}
}
