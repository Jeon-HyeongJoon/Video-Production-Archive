package whispercpp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sort"
)

const speechAnalysisWindowMillis int64 = 20

type chunkPolicy struct {
	MinimumMillis int64
	TargetMillis  int64
	MaximumMillis int64
	SilenceMillis int64
	ContextMillis int64
}

var defaultChunkPolicy = chunkPolicy{
	MinimumMillis: 8 * 60 * 1000,
	TargetMillis:  12 * 60 * 1000,
	MaximumMillis: 15 * 60 * 1000,
	SilenceMillis: 600,
	ContextMillis: 1500,
}

type speechChunk struct {
	LogicalStartMillis int64
	LogicalEndMillis   int64
	ProcessStartMillis int64
	ProcessEndMillis   int64
}

type chunkBoundary struct {
	millis   int64
	fallback bool
}

func (policy chunkPolicy) valid() bool {
	return policy.MinimumMillis > 0 && policy.MinimumMillis < policy.TargetMillis &&
		policy.TargetMillis < policy.MaximumMillis && policy.SilenceMillis >= speechAnalysisWindowMillis &&
		policy.ContextMillis >= 0 && policy.ContextMillis < policy.MinimumMillis
}

func planSpeechChunks(file *os.File, policy chunkPolicy) ([]speechChunk, error) {
	if file == nil || !policy.valid() {
		return nil, ErrInvalidRequest
	}
	levels, durationMillis, err := readPCM16MonoWindows(file)
	if err != nil || durationMillis <= 0 {
		return nil, errors.Join(ErrInvalidRequest, err)
	}
	if durationMillis <= policy.MaximumMillis {
		return []speechChunk{{LogicalEndMillis: durationMillis, ProcessEndMillis: durationMillis}}, nil
	}

	quietThreshold := adaptiveSilenceThreshold(levels)
	silences := silenceMidpoints(levels, quietThreshold, policy.SilenceMillis)
	boundaries := []chunkBoundary{{millis: 0}}
	for start := int64(0); durationMillis-start > policy.MaximumMillis; {
		minimum := start + policy.MinimumMillis
		target := start + policy.TargetMillis
		maximum := start + policy.MaximumMillis
		if latest := durationMillis - policy.MinimumMillis; maximum > latest {
			maximum = latest
		}
		boundary, found := nearestBoundary(silences, minimum, target, maximum)
		fallback := false
		if !found {
			boundary = quietestBoundary(levels, minimum, target, maximum)
			fallback = true
		}
		if boundary <= start || boundary >= durationMillis {
			return nil, ErrInvalidRequest
		}
		boundaries = append(boundaries, chunkBoundary{millis: boundary, fallback: fallback})
		start = boundary
	}
	boundaries = append(boundaries, chunkBoundary{millis: durationMillis})

	chunks := make([]speechChunk, 0, len(boundaries)-1)
	for index := 0; index+1 < len(boundaries); index++ {
		left, right := boundaries[index], boundaries[index+1]
		processStart, processEnd := left.millis, right.millis
		if left.fallback {
			processStart = maxInt64(0, processStart-policy.ContextMillis)
		}
		if right.fallback {
			processEnd = minInt64(durationMillis, processEnd+policy.ContextMillis)
		}
		chunks = append(chunks, speechChunk{
			LogicalStartMillis: left.millis,
			LogicalEndMillis:   right.millis,
			ProcessStartMillis: processStart,
			ProcessEndMillis:   processEnd,
		})
	}
	return chunks, nil
}

func readPCM16MonoWindows(file *os.File) ([]float64, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	var riff [12]byte
	if _, err := io.ReadFull(file, riff[:]); err != nil || string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, 0, ErrInvalidRequest
	}
	var formatFound bool
	var audioFormat, channels, blockAlign, bitsPerSample uint16
	var sampleRate uint32
	var dataOffset int64
	var dataSize int64
	for {
		var header [8]byte
		if _, err := io.ReadFull(file, header[:]); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		} else if err != nil {
			return nil, 0, err
		}
		size := int64(binary.LittleEndian.Uint32(header[4:8]))
		payloadOffset, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		switch string(header[0:4]) {
		case "fmt ":
			if size < 16 {
				return nil, 0, ErrInvalidRequest
			}
			var payload [16]byte
			if _, err := io.ReadFull(file, payload[:]); err != nil {
				return nil, 0, err
			}
			audioFormat = binary.LittleEndian.Uint16(payload[0:2])
			channels = binary.LittleEndian.Uint16(payload[2:4])
			sampleRate = binary.LittleEndian.Uint32(payload[4:8])
			blockAlign = binary.LittleEndian.Uint16(payload[12:14])
			bitsPerSample = binary.LittleEndian.Uint16(payload[14:16])
			formatFound = true
		case "data":
			dataOffset, dataSize = payloadOffset, size
		}
		next := payloadOffset + size
		if size%2 != 0 {
			next++
		}
		if _, err := file.Seek(next, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}
	if !formatFound || audioFormat != 1 || channels != 1 || sampleRate != 16_000 || blockAlign != 2 || bitsPerSample != 16 || dataOffset <= 0 || dataSize <= 0 {
		return nil, 0, ErrInvalidRequest
	}

	windowSamples := int(sampleRate) * int(speechAnalysisWindowMillis) / 1000
	windowBytes := windowSamples * int(blockAlign)
	reader := bufio.NewReaderSize(io.NewSectionReader(file, dataOffset, dataSize), 1<<20)
	buffer := make([]byte, windowBytes)
	levels := make([]float64, 0, dataSize/int64(windowBytes)+1)
	var samples int64
	for {
		count, err := io.ReadFull(reader, buffer)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		count -= count % int(blockAlign)
		if count > 0 {
			var sumSquares float64
			for offset := 0; offset < count; offset += 2 {
				sample := float64(int16(binary.LittleEndian.Uint16(buffer[offset : offset+2])))
				sumSquares += sample * sample
			}
			countSamples := count / 2
			levels = append(levels, math.Sqrt(sumSquares/float64(countSamples)))
			samples += int64(countSamples)
		}
		if err != nil {
			break
		}
	}
	if len(levels) == 0 || samples == 0 {
		return nil, 0, ErrInvalidRequest
	}
	return levels, samples * 1000 / int64(sampleRate), nil
}

func adaptiveSilenceThreshold(levels []float64) float64 {
	ordered := append([]float64(nil), levels...)
	sort.Float64s(ordered)
	noiseFloor := ordered[len(ordered)/5]
	return math.Max(250, math.Min(800, noiseFloor*1.8))
}

func silenceMidpoints(levels []float64, threshold float64, requiredMillis int64) []int64 {
	requiredWindows := int((requiredMillis + speechAnalysisWindowMillis - 1) / speechAnalysisWindowMillis)
	result := make([]int64, 0)
	for start := 0; start < len(levels); {
		if levels[start] > threshold {
			start++
			continue
		}
		end := start + 1
		for end < len(levels) && levels[end] <= threshold {
			end++
		}
		if end-start >= requiredWindows {
			result = append(result, int64(start+end)*speechAnalysisWindowMillis/2)
		}
		start = end
	}
	return result
}

func nearestBoundary(candidates []int64, minimum, target, maximum int64) (int64, bool) {
	var selected int64
	var distance int64
	found := false
	for _, candidate := range candidates {
		if candidate < minimum || candidate > maximum {
			continue
		}
		candidateDistance := absInt64(candidate - target)
		if !found || candidateDistance < distance {
			selected, distance, found = candidate, candidateDistance, true
		}
	}
	return selected, found
}

func quietestBoundary(levels []float64, minimum, target, maximum int64) int64 {
	minimumIndex := int(minimum / speechAnalysisWindowMillis)
	maximumIndex := int(maximum / speechAnalysisWindowMillis)
	if maximumIndex >= len(levels) {
		maximumIndex = len(levels) - 1
	}
	selected := minimumIndex
	selectedLevel := math.Inf(1)
	selectedDistance := int64(0)
	for index := minimumIndex; index <= maximumIndex; index++ {
		millis := int64(index)*speechAnalysisWindowMillis + speechAnalysisWindowMillis/2
		distance := absInt64(millis - target)
		if levels[index] < selectedLevel || (levels[index] == selectedLevel && distance < selectedDistance) {
			selected, selectedLevel, selectedDistance = index, levels[index], distance
		}
	}
	return int64(selected)*speechAnalysisWindowMillis + speechAnalysisWindowMillis/2
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
