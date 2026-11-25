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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"videoarchive/internal/adapter/processprogress"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrChunkPlan           = errors.New("speech audio chunk planning failed")
	ErrInvalidModel        = errors.New("whisper rejected the model file signature")
	ErrOutputRead          = errors.New("whisper output unavailable")
	ErrOutputInvalid       = errors.New("whisper output is invalid")
	ErrPublish             = errors.New("transcript publication failed")
	ErrInvalidRequest      = errors.New("invalid whisper.cpp transcription request")
	ErrTranscriptionFailed = errors.New("whisper.cpp transcription failed")
)

// OutputOpenFailure contains only observations safe to persist across the Worker boundary.
type OutputOpenFailure struct {
	WindowsPathUnits int
	ExtendedPath     bool
	TargetDirectory  bool
}

func (failure *OutputOpenFailure) Error() string {
	summary := "Whisper could not open the requested temporary JSON for writing."
	if failure.WindowsPathUnits > 0 {
		summary += fmt.Sprintf(" Windows path length %d UTF-16 units, extended path used: %t.", failure.WindowsPathUnits, failure.ExtendedPath)
	}
	if failure.TargetDirectory {
		summary += " The target was a directory when checked."
	}
	return summary
}

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
	ModelRoot             *archive.StorageRoot
	ModelRelativePath     string
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
	request.ArtifactPath = artifactPath
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
	chunks, err := transcriber.planRequestChunks(request)
	if err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrChunkPlan, err)
	}
	if request.InputRoot != nil {
		_ = execution.ReportProgress(ctx, execution.ProgressUpdate{
			InputIndex: request.InputIndex, InputCount: request.InputCount,
			Operation: execution.OperationTranscribeAudio, Status: execution.ProgressPlanned,
			ChunkCount: uint32(len(chunks)), SourceEndMillis: chunks[len(chunks)-1].LogicalEndMillis,
		})
	}
	checkpoints, err := newChunkCheckpoints(request, transcriber.processorVersion, profile, chunks)
	if err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
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
		if err := ctx.Err(); err != nil {
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
		}
		part, cached := checkpoints.load(index, chunk)
		if !cached {
			reportChunk(execution.ProgressStarted, float64(index)/float64(len(chunks)))
			raw, runErr := transcriber.runChunk(ctx, request, profile, chunk, index, len(chunks), func(percent uint32) {
				observedPercent = percent
				reportChunk(execution.ProgressProcessing, 0)
			})
			if runErr != nil {
				reportChunk(execution.ProgressFailed, 0)
				return transcription.Transcript{}, runErr
			}
			part, err = parseChunk(raw, chunk, chunks[len(chunks)-1].LogicalEndMillis, len(chunks) == 1)
			if err != nil {
				reportChunk(execution.ProgressFailed, 0)
				return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid, err)
			}
		}
		if (profile.Language != "auto" && part.Language != profile.Language) || (language != "" && language != part.Language) {
			reportChunk(execution.ProgressFailed, 0)
			if profile.Language == "auto" {
				checkpoints.clear()
			}
			return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, ErrOutputInvalid)
		}
		language = part.Language
		segments = append(segments, part.Segments...)
		if !cached {
			checkpoints.save(index, part)
		}
		reportChunk(execution.ProgressCompleted, float64(index+1)/float64(len(chunks)))
	}
	content, err := transcription.NewContent(language, numberSegments(segments))
	if err != nil {
		checkpoints.clear()
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
	if err := ctx.Err(); err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
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
	if request.OutputRoot != nil {
		// A shorter replacement input may now have a single chunk.
		_ = request.OutputRoot.RemoveAll(checkpointDirectory(artifactPath))
	}
	return transcript, nil
}

// Direct Adapter callers supply a resolved WAV path. Registered Worker inputs
// must identify the same file for measurement and for the subprocess.
func (transcriber *Transcriber) planRequestChunks(request Request) ([]speechChunk, error) {
	root, name := request.InputRoot, request.AudioArtifact.Artifact.Path
	if root == nil {
		var err error
		root, err = archive.OpenStorageRoot(filepath.Dir(request.ResolvedAudioPath))
		if err != nil {
			return nil, err
		}
		defer root.Close()
		name = filepath.Base(request.ResolvedAudioPath)
	} else {
		resolved, err := root.ExternalProcessPath(name)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(request.ResolvedAudioPath) {
			return nil, ErrInvalidRequest
		}
		// The digest used for retry must describe this attempt's actual WAV.
		size, checksum, err := root.FileSHA256(name)
		if err != nil {
			return nil, err
		}
		if size != request.AudioArtifact.Artifact.Size || checksum != request.AudioArtifact.Artifact.Checksum {
			return nil, ErrInvalidRequest
		}
	}
	input, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	chunks, err := planSpeechChunks(input, transcriber.chunkPolicy)
	return chunks, errors.Join(err, input.Close())
}

func (transcriber *Transcriber) runChunk(ctx context.Context, request Request, profile transcription.Profile, chunk speechChunk, index, count int, onPercent func(uint32)) ([]byte, error) {
	suffix := ""
	if count > 1 {
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
	arguments := []string{"-m", whisperPathArgument(request.ResolvedModelPath), "-f", whisperPathArgument(request.ResolvedAudioPath)}
	if count > 1 {
		arguments = append(arguments,
			"-ot", strconv.FormatInt(chunk.ProcessStartMillis, 10),
			"-d", strconv.FormatInt(chunk.ProcessEndMillis-chunk.ProcessStartMillis, 10),
		)
	}
	// whisper appends .json to -of before opening the file.
	outputArgument := strings.TrimSuffix(whisperPathArgument(rawPath), ".json")
	arguments = append(arguments, "-oj", "-of", outputArgument, "-l", profile.Language, "-t", strconv.Itoa(profile.ThreadCount), "-np")
	var runErr error
	var observedPercent uint32
	if stream, ok := transcriber.runner.(execution.StreamRunner); ok && request.InputRoot != nil {
		// Only numeric whisper progress reaches the reporter. Limit live updates to ten per chunk.
		openFailureLine := "open: failed to open '" + outputArgument + ".json' for writing"
		var observedFailure error
		lines := processprogress.Lines{MaxLineBytes: max(256, len(openFailureLine)), OnLine: func(line string) {
			switch line {
			case openFailureLine:
				failure := &OutputOpenFailure{}
				if runtime.GOOS == "windows" {
					failure.WindowsPathUnits = len(utf16.Encode([]rune(rawPath)))
					failure.ExtendedPath = strings.HasPrefix(outputArgument, `\\?\`)
				}
				observedFailure = failure
				return
			case "whisper_model_load: invalid model data (bad magic)":
				observedFailure = ErrInvalidModel
				return
			}
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
				onPercent(observedPercent)
			}
		}}
		arguments[len(arguments)-1] = "-pp"
		runErr = stream.RunStreams(ctx, transcriber.executable, arguments, nil, &lines)
		lines.Flush()
		if runErr != nil && observedFailure != nil {
			runErr = errors.Join(runErr, observedFailure)
		}
	} else {
		runErr = transcriber.runner.Run(ctx, transcriber.executable, arguments)
	}
	if runErr != nil {
		// The engine never opened this target. Its existing contents may
		// belong to another owner, including a conflicting directory.
		var openFailure *OutputOpenFailure
		if errors.As(runErr, &openFailure) {
			if request.OutputRoot != nil {
				info, statErr := request.OutputRoot.Stat(rawArtifactPath)
				openFailure.TargetDirectory = statErr == nil && info.IsDir()
			}
		} else {
			cleanupRaw()
		}
		return nil, errors.Join(ErrTranscriptionFailed, runErr)
	}
	var err error
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
		return nil, errors.Join(ErrTranscriptionFailed, ErrOutputRead, err)
	}
	return rawPayload, nil
}

// The Windows whisper.cpp CLI requires extended-length names beyond MAX_PATH.
// Only its argument spelling changes. StorageRoot retains the original paths.
func whisperPathArgument(name string) string {
	if runtime.GOOS != "windows" || len(name) < 260 || !filepath.IsAbs(name) {
		return name
	}
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, `\\?\`) || strings.HasPrefix(clean, `\\.\`) {
		return clean
	}
	if server, ok := strings.CutPrefix(clean, `\\`); ok {
		return `\\?\UNC\` + server
	}
	return `\\?\` + clean
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

// whisper.cpp runs on the original WAV with -ot/-d. Its JSON offsets are
// absolute even when the first returned cue predates the requested window.
func parseChunk(payload []byte, chunk speechChunk, duration int64, single bool) (transcription.Content, error) {
	var output whisperOutput
	if !utf8.Valid(payload) {
		return transcription.Content{}, ErrOutputInvalid
	}
	if err := json.Unmarshal(payload, &output); err != nil {
		return transcription.Content{}, err
	}
	language := strings.ToLower(strings.TrimSpace(output.Result.Language))
	if language == "" || output.Transcription == nil {
		return transcription.Content{}, transcription.ErrInvalidContent
	}
	segments := make([]transcription.Segment, 0, len(output.Transcription))
	for _, raw := range output.Transcription {
		start, end := raw.Offsets.From, raw.Offsets.To
		if start >= duration || end <= 0 {
			continue
		}
		start, end = max(start, int64(0)), min(end, duration)
		if !single {
			if end <= start || strings.TrimSpace(raw.Text) == "" {
				continue
			}
			// Clamp against the measured outer bounds before assigning overlap
			// ownership. An overlong final cue can otherwise lose its owner.
			midpoint := start + (end-start)/2
			if midpoint < chunk.LogicalStartMillis || midpoint >= chunk.LogicalEndMillis {
				continue
			}
			start, end = max(start, chunk.LogicalStartMillis), min(end, chunk.LogicalEndMillis)
		}
		segments = append(segments, transcription.Segment{StartMillis: start, EndMillis: end, Text: raw.Text})
	}
	if !single {
		segments = mergeChunkOverlaps(segments)
		if len(segments) == 0 {
			return transcription.Content{Language: language}, nil
		}
	}
	return transcription.NewContent(language, numberSegments(segments))
}

func numberSegments(segments []transcription.Segment) []transcription.Segment {
	for index := range segments {
		segments[index].Sequence = uint32(index + 1)
	}
	return segments
}

func mergeChunkOverlaps(segments []transcription.Segment) []transcription.Segment {
	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].StartMillis == segments[j].StartMillis {
			return segments[i].EndMillis < segments[j].EndMillis
		}
		return segments[i].StartMillis < segments[j].StartMillis
	})
	merged := segments[:0]
	var previousEnd int64
	for _, segment := range segments {
		if segment.EndMillis <= previousEnd {
			continue
		}
		segment.StartMillis = max(segment.StartMillis, previousEnd)
		merged = append(merged, segment)
		previousEnd = segment.EndMillis
	}
	return merged
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
