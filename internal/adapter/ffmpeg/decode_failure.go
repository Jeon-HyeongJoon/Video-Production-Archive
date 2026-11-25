package ffmpeg

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DecodeFailure contains only numeric observations and never exports tool output.
type DecodeFailure struct {
	InputIndex, InputCount uint32
	OutputMillis           int64
}

func (e *DecodeFailure) Error() string {
	return fmt.Sprintf("원본 %d개 중 %d번째 디코딩 실패 · 마지막 출력 위치 %d초 (%dms)", e.InputCount, e.InputIndex, e.OutputMillis/1000, e.OutputMillis)
}

type decoderDiagnostics struct {
	inputs                          []decoderInput
	audioMismatch, fatalAudioFilter bool
}

type decoderInput struct{ invalidData, fatalDecoder, excessiveRate, rateTermination bool }

var inputPrefix = regexp.MustCompile(`^\[[va]ist#(\d+):\d+[^]]*\]\s*`)
var excessiveRate = regexp.MustCompile(`^(?:\[dec:[^]]+\]\s+)?Decode error rate [0-9.]+ exceeds maximum [0-9.]+$`)
var decoderEnd = regexp.MustCompile(`\[dec:[^]]+\]\s+Terminating thread with return code -?\d+ \(Invalid data found when processing input\)`)
var audioFilterEnd = regexp.MustCompile(`^\[af#[^]]+\]\s+Terminating thread with return code -22 \(Invalid argument\)`)

func (d *decoderDiagnostics) observe(line string) {
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "[aac @") && strings.Contains(line, "Sample rate index") && strings.Contains(line, "does not match the sample rate index"):
		d.audioMismatch = true
		return
	case strings.HasPrefix(line, "[af#"):
		d.fatalAudioFilter = d.fatalAudioFilter || audioFilterEnd.MatchString(line)
		return
	}
	prefix := inputPrefix.FindStringSubmatch(line)
	index := 0
	message := line
	if prefix != nil {
		parsed, err := strconv.Atoi(prefix[1])
		if err != nil || parsed >= len(d.inputs) {
			return
		}
		index, message = parsed, line[len(prefix[0]):]
	} else if len(d.inputs) != 1 || !strings.HasPrefix(line, "[dec:") {
		return
	}
	input := &d.inputs[index]
	switch {
	case strings.HasSuffix(message, "Error submitting packet to decoder: Invalid data found when processing input"),
		strings.HasSuffix(message, "Decoding error: Invalid data found when processing input"):
		input.invalidData = true
	case strings.Contains(message, "Decode error rate") && excessiveRate.MatchString(message):
		input.excessiveRate = true
	case strings.HasPrefix(message, "[dec:") && strings.Contains(message, "] Terminating thread with return code -1145393733 ("):
		input.rateTermination = true
	case strings.Contains(message, "Terminating thread") && decoderEnd.MatchString(message):
		input.fatalDecoder = true
	}
}

func (d decoderDiagnostics) rejectedInput(err error) (int, bool) {
	var exit interface{ ExitCode() int }
	rateExit := errors.As(err, &exit) && exit.ExitCode() == 69
	for index, input := range d.inputs {
		fatalRate := input.excessiveRate && (rateExit || input.rateTermination)
		if input.invalidData && (input.fatalDecoder || fatalRate || len(d.inputs) == 1 && d.audioMismatch && d.fatalAudioFilter) {
			return index, true
		}
	}
	return 0, false
}
