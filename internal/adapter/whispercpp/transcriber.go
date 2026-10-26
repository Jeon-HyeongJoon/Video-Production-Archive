package whispercpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRequest      = errors.New("invalid whisper.cpp transcription request")
	ErrTranscriptionFailed = errors.New("whisper.cpp transcription failed")
)

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, executable string, arguments []string) error {
	process := exec.CommandContext(ctx, executable, arguments...)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
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
	TranscriptID        string
	AudioArtifact       archive.Artifact
	ResolvedAudioPath   string
	ResolvedModelPath   string
	TemporaryOutputPath string
	PublishedOutputPath string
	ArtifactPath        string
	Profile             transcription.Profile
}

type Transcriber struct {
	executable       string
	processorVersion string
	workerID         string
	runner           execution.Runner
	publisher        execution.Publisher
	clock            func() time.Time
}

func NewTranscriber(executable, processorVersion, workerID string, runner execution.Runner, publisher execution.Publisher) (*Transcriber, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(processorVersion) == "" || strings.TrimSpace(workerID) == "" ||
		runner == nil || publisher == nil {
		return nil, ErrInvalidRequest
	}
	return &Transcriber{
		executable: strings.TrimSpace(executable), processorVersion: strings.TrimSpace(processorVersion), workerID: strings.TrimSpace(workerID),
		runner: runner, publisher: publisher, clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (transcriber *Transcriber) Transcribe(ctx context.Context, request Request) (transcription.Transcript, error) {
	profile, err := transcription.NewProfile(request.Profile)
	if err != nil {
		return transcription.Transcript{}, err
	}
	artifactPath, pathOK := normalizeArtifactPath(request.ArtifactPath)
	audio := request.AudioArtifact
	if strings.TrimSpace(request.TranscriptID) == "" || audio.ValidateDerived() != nil ||
		audio.Origin.ProcessingKind != vocabulary.ProcessingAudioExtract ||
		strings.TrimSpace(request.ResolvedAudioPath) == "" || strings.TrimSpace(request.ResolvedModelPath) == "" ||
		strings.TrimSpace(request.TemporaryOutputPath) == "" || strings.TrimSpace(request.PublishedOutputPath) == "" || !pathOK ||
		filepath.Clean(filepath.Dir(request.TemporaryOutputPath)) != filepath.Clean(filepath.Dir(request.PublishedOutputPath)) {
		return transcription.Transcript{}, ErrInvalidRequest
	}
	if err := os.MkdirAll(filepath.Dir(request.PublishedOutputPath), 0o755); err != nil {
		return transcription.Transcript{}, err
	}

	rawPrefix := request.TemporaryOutputPath + ".whisper"
	rawPath := rawPrefix + ".json"
	defer os.Remove(rawPath)
	defer os.Remove(request.TemporaryOutputPath)
	arguments := []string{
		"-m", request.ResolvedModelPath,
		"-f", request.ResolvedAudioPath,
		"-oj", "-of", rawPrefix,
		"-l", profile.Language,
		"-t", strconv.Itoa(profile.ThreadCount),
		"-np",
	}
	if err := transcriber.runner.Run(ctx, transcriber.executable, arguments); err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
	}
	rawPayload, err := os.ReadFile(rawPath)
	if err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
	}
	content, err := parseWhisperJSON(rawPayload)
	if err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
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
			OriginalAssetID: audio.Origin.OriginalAssetID, InputArtifactIDs: []string{audio.ID}, ProcessingKind: vocabulary.ProcessingSTT,
			ProcessorVersion: transcriber.processorVersion, ProfileVersion: profile.Reference(), CreatedBy: transcriber.workerID, CreatedAt: transcriber.clock(),
		},
	}
	transcript, err := transcription.NewTranscript(artifact, profile, content)
	if err != nil {
		return transcription.Transcript{}, err
	}
	if err := os.WriteFile(request.TemporaryOutputPath, payload, 0o600); err != nil {
		return transcription.Transcript{}, err
	}
	if err := transcriber.publisher.Publish(request.TemporaryOutputPath, request.PublishedOutputPath); err != nil {
		return transcription.Transcript{}, errors.Join(ErrTranscriptionFailed, err)
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
	var output whisperOutput
	if err := json.Unmarshal(payload, &output); err != nil {
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

func normalizeArtifactPath(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "", false
	}
	return clean, clean == normalized
}
