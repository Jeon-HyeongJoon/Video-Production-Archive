package whispercpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/adapter/processprogress"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrChunkPlan           = errors.New("speech audio chunk planning failed")
	ErrOutputRead          = errors.New("whisper output unavailable")
	ErrOutputInvalid       = errors.New("whisper output is invalid")
	ErrPublish             = errors.New("transcript publication failed")
	ErrInvalidRequest      = errors.New("invalid whisper.cpp transcription request")
	ErrTranscriptionFailed = errors.New("whisper.cpp transcription failed")
)

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, executable string, arguments []string) error {
	return (OSRunner{}).RunStreams(ctx, executable, arguments, nil, nil)
}

func (OSRunner) RunStreams(ctx context.Context, executable string, arguments []string, stdout, stderr io.Writer) error {
	process := exec.CommandContext(ctx, executable, arguments...)
	process.Stdout, process.Stderr = stdout, stderr
	return process.Run()
}

type AtomicPublisher struct{}

func (AtomicPublisher) Publish(temporaryPath, publishedPath string) error {
	if filepath.Clean(filepath.Dir(temporaryPath)) != filepath.Clean(filepath.Dir(publishedPath)) {
		return ErrInvalidRequest
	}
	return os.Rename(temporaryPath, publishedPath)
}

type Request struct {
	InputIndex            uint32
	InputCount            uint32
	TranscriptID          string
	AudioArtifact         audio.AudioArtifact
	InputRoot             *archive.StorageRoot
	ResolvedAudioPath     string
	ResolvedModelPath     string
	TemporaryOutputPath   string
	PublishedOutputPath   string
	ArtifactPath          string
	TemporaryArtifactPath string
	OutputRoot            *archive.StorageRoot
	Profile               transcription.Profile
}

type Transcriber struct {
	executable       string
	processorVersion string
	workerID         string
	runner           execution.Runner
	publisher        execution.Publisher
	clock            func() time.Time
	chunkPolicy      chunkPolicy
}

func NewTranscriber(executable, processorVersion, workerID string, runner execution.Runner, publisher execution.Publisher) (*Transcriber, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" ||
		runner == nil || publisher == nil {
		return nil, ErrInvalidRequest
	}
	return &Transcriber{
		executable: strings.TrimSpace(executable), processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		runner: runner, publisher: publisher, clock: func() time.Time { return time.Now().UTC() }, chunkPolicy: defaultChunkPolicy,
	}, nil
}

func (transcriber *Transcriber) Transcribe(ctx context.Context, request Request) (transcription.Transcript, error) {
	profile, err := transcription.NewProfile(request.Profile)
	if err != nil {
		return transcription.Transcript{}, err
	}
	artifactPath, pathOK := normalizeArtifactPath(request.ArtifactPath)
	audioArtifact := request.AudioArtifact
	if strings.TrimSpace(request.TranscriptID) == "" || audioArtifact.Validate() != nil ||
		strings.TrimSpace(request.ResolvedAudioPath) == "" || strings.TrimSpace(request.ResolvedModelPath) == "" ||
		strings.TrimSpace(request.TemporaryOutputPath) == "" || strings.TrimSpace(request.PublishedOutputPath) == "" || !pathOK ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) {
		return transcription.Transcript{}, ErrInvalidRequest
	}
	if request.OutputRoot != nil {
		temporaryArtifactPath, temporaryOK := normalizeArtifactPath(request.TemporaryArtifactPath)
		temporaryResolved, temporaryErr := request.OutputRoot.ExternalProcessPath(temporaryArtifactPath)
		publishedResolved, publishedErr := request.OutputRoot.ExternalProcessPath(artifactPath)
		if !temporaryOK || temporaryArtifactPath == artifactPath || path.Dir(temporaryArtifactPath) != path.Dir(artifactPath) ||
			temporaryErr != nil || publishedErr != nil || filepath.Clean(temporaryResolved) != filepath.Clean(request.TemporaryOutputPath) ||
			filepath.Clean(publishedResolved) != filepath.Clean(request.PublishedOutputPath) {
			return transcription.Transcript{}, ErrInvalidRequest
		}
		if err := request.OutputRoot.MkdirAll(path.Dir(artifactPath), 0o755); err != nil {
			return transcription.Transcript{}, err
		}
	} else if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return transcription.Transcript{}, err
	}

	defer func() {
		if request.OutputRoot != nil {
			_ = request.OutputRoot.Remove(request.TemporaryArtifactPath)
		} else {
			_ = os.Remove(request.TemporaryOutputPath)
		}
	}()
	chunks := []speechChunk{{}}
	if request.InputRoot != nil {
		input, openErr := request.InputRoot.Open(audioArtifact.Artifact.Path)
		if openErr != nil {
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrChunkPlan, openErr)
		}
		chunks, err = planSpeechChunks(input, transcriber.chunkPolicy)
		closeErr := input.Close()
		if err != nil || closeErr != nil {
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrChunkPlan, err, closeErr)
		}
	}
	if request.InputRoot != nil {
		_ = execution.ReportProgress(ctx, execution.ProgressUpdate{
			InputIndex: request.InputIndex, InputCount: request.InputCount,
			Operation: execution.OperationTranscribeAudio, Status: execution.ProgressPlanned,
			ChunkCount: uint32(len(chunks)), SourceEndMillis: chunks[len(chunks)-1].LogicalEndMillis,
		})
	}
	var language string
	segments := make([]transcription.Segment, 0)
	for index, chunk := range chunks {
		var observedPercent uint32
		reportChunk := func(status execution.ProgressStatus, progress float64) {
			if request.InputRoot == nil {
				return
			}
			var percent uint32
			if status == execution.ProgressProcessing || status == execution.ProgressFailed {
				percent = observedPercent
				progress = (float64(index) + float64(percent)/100) / float64(len(chunks))
			}
			_ = execution.ReportProgress(ctx, execution.ProgressUpdate{
				ProcessPercent: percent, InputIndex: request.InputIndex, InputCount: request.InputCount,
				Operation: execution.OperationTranscribeAudio, Status: status,
				ChunkIndex: uint32(index + 1), ChunkCount: uint32(len(chunks)),
				SourceStartMillis: chunk.LogicalStartMillis, SourceEndMillis: chunk.LogicalEndMillis,
				Progress: progress,
			})
		}
		suffix := ""
		if len(chunks) > 1 {
			suffix = "." + fmt.Sprintf("%04d", index+1)
		}
		rawPrefix := request.TemporaryOutputPath + ".whisper" + suffix
		rawPath := rawPrefix + ".json"
		rawArtifactPath := request.TemporaryArtifactPath + ".whisper" + suffix + ".json"
		cleanupRaw := func() {
			if request.OutputRoot != nil {
				_ = request.OutputRoot.Remove(rawArtifactPath)
			} else {
				_ = os.Remove(rawPath)
			}
		}
		reportChunk(execution.ProgressStarted, float64(index)/float64(len(chunks)))
		arguments := []string{"-m", request.ResolvedModelPath, "-f", request.ResolvedAudioPath}
		if len(chunks) > 1 {
			arguments = append(arguments,
				"-ot", strconv.FormatInt(chunk.ProcessStartMillis, 10),
				"-d", strconv.FormatInt(chunk.ProcessEndMillis-chunk.ProcessStartMillis, 10),
			)
		}
		arguments = append(arguments, "-oj", "-of", rawPrefix, "-l", profile.Language, "-t", strconv.Itoa(profile.ThreadCount), "-np")
		var runErr error
		if stream, ok := transcriber.runner.(execution.StreamRunner); ok && request.InputRoot != nil {
			// Only numeric whisper progress reaches the reporter. Limit live updates to ten per chunk.
			lines := processprogress.Lines{OnLine: func(line string) {
				value, found := strings.CutPrefix(strings.TrimSpace(line), "whisper_print_progress_callback: progress =")
				if !found {
					return
				}
				value = strings.TrimSpace(value)
				if !strings.HasSuffix(value, "%") {
					return
				}
				n, err := strconv.ParseUint(strings.TrimSuffix(value, "%"), 10, 32)
				if err == nil && n <= 100 && uint32(n)/10 > observedPercent/10 {
					observedPercent = uint32(n)
					reportChunk(execution.ProgressProcessing, 0)
				}
			}}
			arguments[len(arguments)-1] = "-pp"
			runErr = stream.RunStreams(ctx, transcriber.executable, arguments, nil, &lines)
			lines.Flush()
		} else {
			runErr = transcriber.runner.Run(ctx, transcriber.executable, arguments)
		}
		if runErr != nil {
			cleanupRaw()
			reportChunk(execution.ProgressFailed, float64(index)/float64(len(chunks)))
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, runErr)
		}
		var rawPayload []byte
		if request.OutputRoot != nil {
			rawPayload, err = request.OutputRoot.ReadFile(rawArtifactPath)
			if errors.Is(err, os.ErrNotExist) {
				// A reachable output directory with a missing JSON is a tool failure.
				if _, dirErr := request.OutputRoot.ReadDir(path.Dir(rawArtifactPath)); dirErr == nil {
					err = ErrOutputRead
				}
			}
		} else {
			rawPayload, err = os.ReadFile(rawPath)
		}
		cleanupRaw()
		if err != nil {
			reportChunk(execution.ProgressFailed, float64(index)/float64(len(chunks)))
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputRead, err)
		}
		decoded, decodeErr := decodeWhisperJSON(rawPayload)
		if decodeErr != nil {
			reportChunk(execution.ProgressFailed, float64(index)/float64(len(chunks)))
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid, decodeErr)
		}
		if language == "" {
			language = strings.ToLower(strings.TrimSpace(decoded.Result.Language))
		} else if language != strings.ToLower(strings.TrimSpace(decoded.Result.Language)) {
			reportChunk(execution.ProgressFailed, float64(index)/float64(len(chunks)))
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid)
		}
		if len(chunks) == 1 {
			parsed, parseErr := parseWhisperJSON(rawPayload)
			if parseErr != nil {
				reportChunk(execution.ProgressFailed, float64(index)/float64(len(chunks)))
				return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid, parseErr)
			}
			segments = append(segments, parsed.Segments...)
		} else {
			segments = append(segments, ownedChunkSegments(decoded, chunk, false)...)
		}
		reportChunk(execution.ProgressCompleted, float64(index+1)/float64(len(chunks)))
	}
	content, err := mergeWhisperSegments(language, segments)
	if err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid, err)
	}
	document := transcriptDocument{SchemaVersion: 1, ProfileReference: profile.Reference(), Content: content}
	payload, err := json.Marshal(document)
	if err != nil {
		return transcription.Transcript{}, err
	}
	digest := sha256.Sum256(payload)
	artifact := archive.Artifact{
		ID: strings.TrimSpace(request.TranscriptID), Path: artifactPath, Size: int64(len(payload)), Checksum: hex.EncodeToString(digest[:]),
		Origin: archive.Provenance{
			OriginalAssetID: audioArtifact.Artifact.Origin.OriginalAssetID, InputArtifactIDs: []string{audioArtifact.Artifact.ID}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: transcriber.processorVersion, ProfileVersion: profile.Reference(), CreatedBy: transcriber.workerID, CreatedAt: transcriber.clock(),
		},
	}
	transcript, err := transcription.NewTranscript(artifact, profile, content)
	if err != nil {
		return transcription.Transcript{}, err
	}
	if request.OutputRoot != nil {
		if err := request.OutputRoot.WriteFile(request.TemporaryArtifactPath, payload, 0o600); err != nil {
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrPublish, err)
		}
	} else if err := os.WriteFile(request.TemporaryOutputPath, payload, 0o600); err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrPublish, err)
	}
	if err := transcriber.publisher.Publish(request.TemporaryOutputPath, request.PublishedOutputPath); err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrPublish, err)
	}
	return transcript, nil
}

type transcriptDocument struct {
	SchemaVersion    uint32                `json:"schema_version"`
	ProfileReference string                `json:"profile_reference"`
	Content          transcription.Content `json:"content"`
}

type whisperOutput struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

func parseWhisperJSON(payload []byte) (transcription.Content, error) {
	output, err := decodeWhisperJSON(payload)
	if err != nil {
		return transcription.Content{}, err
	}
	segments := make([]transcription.Segment, 0, len(output.Transcription))
	for index, segment := range output.Transcription {
		segments = append(segments, transcription.Segment{
			Sequence: uint32(index + 1), StartMillis: segment.Offsets.From, EndMillis: segment.Offsets.To, Text: segment.Text,
		})
	}
	return transcription.NewContent(output.Result.Language, segments)
}

func decodeWhisperJSON(payload []byte) (whisperOutput, error) {
	var output whisperOutput
	if err := json.Unmarshal(payload, &output); err != nil {
		return whisperOutput{}, err
	}
	if strings.TrimSpace(output.Result.Language) == "" {
		return whisperOutput{}, transcription.ErrInvalidContent
	}
	return output, nil
}

func ownedChunkSegments(output whisperOutput, chunk speechChunk, whole bool) []transcription.Segment {
	segments := make([]transcription.Segment, 0, len(output.Transcription))
	relativeOffsets := !whole && chunk.ProcessStartMillis > 0 && len(output.Transcription) > 0 &&
		output.Transcription[0].Offsets.From+1000 < chunk.ProcessStartMillis
	for _, raw := range output.Transcription {
		start, end := raw.Offsets.From, raw.Offsets.To
		if relativeOffsets {
			start += chunk.ProcessStartMillis
			end += chunk.ProcessStartMillis
		}
		text := strings.TrimSpace(raw.Text)
		if text == "" || end <= start {
			continue
		}
		if !whole {
			midpoint := start + (end-start)/2
			if midpoint < chunk.LogicalStartMillis || midpoint >= chunk.LogicalEndMillis {
				continue
			}
			start = maxInt64(start, chunk.LogicalStartMillis)
			end = minInt64(end, chunk.LogicalEndMillis)
			if end <= start {
				continue
			}
		}
		segments = append(segments, transcription.Segment{StartMillis: start, EndMillis: end, Text: text})
	}
	return segments
}

func mergeWhisperSegments(language string, segments []transcription.Segment) (transcription.Content, error) {
	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].StartMillis == segments[j].StartMillis {
			return segments[i].EndMillis < segments[j].EndMillis
		}
		return segments[i].StartMillis < segments[j].StartMillis
	})
	merged := make([]transcription.Segment, 0, len(segments))
	var previousEnd int64
	for _, segment := range segments {
		if segment.EndMillis <= previousEnd {
			continue
		}
		if segment.StartMillis < previousEnd {
			segment.StartMillis = previousEnd
		}
		if segment.EndMillis <= segment.StartMillis || strings.TrimSpace(segment.Text) == "" {
			continue
		}
		segment.Sequence = uint32(len(merged) + 1)
		merged = append(merged, segment)
		previousEnd = segment.EndMillis
	}
	return transcription.NewContent(language, merged)
}

func normalizeArtifactPath(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "", false
	}
	return clean, clean == normalized
}
