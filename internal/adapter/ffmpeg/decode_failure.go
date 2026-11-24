package ffmpeg

import (
	"fmt"
	"regexp"
	"strings"
)

// DecodeFailure contains only numeric observations and never exports tool output.
type DecodeFailure struct {
	InputIndex, InputCount uint32
	OutputMillis           int64
}

func (e *DecodeFailure) Error() string {
	return fmt.Sprintf("원본 %d개 중 %d번째 디코딩 실패 · 마지막 출력 위치 %d초", e.InputCount, e.InputIndex, e.OutputMillis/1000)
}

type decoderDiagnostics struct {
	badPacket, audioMismatch, fatalDecoder, fatalAudioFilter bool
}

var decoderEnd = regexp.MustCompile(`\[dec:[^]]+\]\s+Terminating thread with return code -?\d+ \(Invalid data found when processing input\)`)
var audioFilterEnd = regexp.MustCompile(`^\[af#[^]]+\]\s+Terminating thread with return code -22 \(Invalid argument\)`)

func (d *decoderDiagnostics) observe(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") {
		return
	}
	switch {
	case strings.Contains(line, "[dec:") && strings.Contains(line, "Error submitting packet to decoder: Invalid data found when processing input"):
		d.badPacket = true
	case strings.HasPrefix(line, "[aac @") && strings.Contains(line, "Sample rate index") && strings.Contains(line, "does not match the sample rate index"):
		d.audioMismatch = true
	case strings.Contains(line, "Terminating thread"):
		d.fatalDecoder = d.fatalDecoder || decoderEnd.MatchString(line)
		d.fatalAudioFilter = d.fatalAudioFilter || audioFilterEnd.MatchString(line)
	}
}

func (d decoderDiagnostics) rejectedInput() bool {
	return d.badPacket && (d.fatalDecoder || d.audioMismatch && d.fatalAudioFilter)
}
