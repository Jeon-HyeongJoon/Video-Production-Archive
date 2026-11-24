package ffmpeg

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/adapter/processprogress"
	"videoarchive/internal/core/execution"
)

// WithProgress observes the encoder's output clock, including its last position
// on failure. The verifier runs separately and determines whether publication succeeded.
func WithProgress(runner execution.Runner, operation execution.Operation, index, count int, durationMillis int64) execution.Runner {
	stream, ok := runner.(execution.StreamRunner)
	if !ok {
		return runner
	}
	return &progressRunner{stream, operation, uint32(index), uint32(count), durationMillis}
}

type progressRunner struct {
	stream         execution.StreamRunner
	operation      execution.Operation
	index, count   uint32
	durationMillis int64
}

func (r *progressRunner) Run(ctx context.Context, executable string, args []string) error {
	var latest, reported int64
	var lastReport time.Time
	report := func() {
		if latest <= reported {
			return
		}
		fraction := float64(0)
		if r.durationMillis > 0 {
			fraction = min(1, float64(latest)/float64(r.durationMillis))
		}
		_ = execution.ReportProgress(ctx, execution.ProgressUpdate{Operation: r.operation, Status: execution.ProgressPosition,
			InputIndex: r.index, InputCount: r.count, OutputMillis: latest, Progress: fraction})
		reported, lastReport = latest, time.Now()
	}
	lines := processprogress.Lines{OnLine: func(line string) {
		value, ok := strings.CutPrefix(line, "out_time_us=")
		if !ok {
			return
		}
		micros, err := strconv.ParseInt(value, 10, 64)
		if err != nil || micros/1000 <= latest {
			return
		}
		latest = micros / 1000
		if time.Since(lastReport) >= 30*time.Second {
			report()
		}
	}}
	args = append([]string{"-nostats", "-progress", "pipe:1"}, args...)
	var diagnostics decoderDiagnostics
	stderr := processprogress.Lines{OnLine: diagnostics.observe, MaxLineBytes: 1024}
	err := r.stream.RunStreams(ctx, executable, args, &lines, &stderr)
	stderr.Flush()
	lines.Flush()
	report()
	if err != nil && ctx.Err() == nil && diagnostics.rejectedInput() {
		return errors.Join(err, &DecodeFailure{r.index, r.count, latest})
	}
	return err
}
