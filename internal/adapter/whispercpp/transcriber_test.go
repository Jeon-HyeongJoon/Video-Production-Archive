package whispercpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const audioChecksum = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Requirement: TRN-002
func TestWhisperCPPAdapterExecutesArgumentArrayAndPublishesVerifiedTranscript(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments.txt")
	executablePath := filepath.Join(directory, "whisper-fixture")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WHISPER_ARGUMENTS\"\nprevious=''\noutput_prefix=''\nfor argument in \"$@\"; do\n  if [ \"$previous\" = '-of' ]; then output_prefix=\"$argument\"; fi\n  previous=\"$argument\"\ndone\nprintf '%s' \"$WHISPER_JSON\" > \"${output_prefix}.json\"\n"
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rawJSON := `{"systeminfo":"private-host","params":{"model":"/secret/models/private.bin"},"result":{"language":"ko"},"transcription":[{"timestamps":{"from":"00:00:00,000","to":"00:00:01,250"},"offsets":{"from":0,"to":1250},"text":" 첫 문장 "},{"timestamps":{"from":"00:00:01,400","to":"00:00:02,800"},"offsets":{"from":1400,"to":2800},"text":"두 번째 문장"}]}`
	t.Setenv("WHISPER_ARGUMENTS", argumentsPath)
	t.Setenv("WHISPER_JSON", rawJSON)

	transcriber, err := NewTranscriber(executablePath, "whisper.cpp-b6250", "WORKER-1", OSRunner{}, AtomicPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	profile := transcription.Profile{
		ID: "event-ko", Version: 1, ModelID: "ggml-large-v3-turbo-q5_0", Language: "ko", ThreadCount: 4,
	}
	audioProfile := audio.Profile{
		ID: "speech-pcm-16k", Version: 1, Container: audio.ContainerWAV, Codec: audio.CodecPCM16LE,
		SampleRateHz: audio.SpeechRateHz, Channels: audio.SpeechChannels, ThreadCount: 2,
	}
	audioArtifact := archive.Artifact{
		ID: "AUDIO-1", Path: "audio/event.wav", Size: 1_024, Checksum: audioChecksum,
		Origin: archive.Provenance{
			OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingAudioExtract,
			ProcessorVersion: "ffmpeg-8.0", ProfileVersion: audioProfile.Reference(), CreatedBy: "WORKER-1", CreatedAt: time.Now().UTC(),
		},
	}
	audioInput, err := audio.NewAudioArtifact(audioArtifact, audioProfile)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(directory, "audio", "event & keep.wav")
	modelPath := filepath.Join(directory, "models", "large;keep.bin")
	temporaryPath := filepath.Join(directory, "output", "event.json.partial")
	publishedPath := filepath.Join(directory, "output", "event.json")
	transcript, err := transcriber.Transcribe(context.Background(), Request{
		TranscriptID: "TRANSCRIPT-1", AudioArtifact: audioInput, ResolvedAudioPath: inputPath, ResolvedModelPath: modelPath,
		TemporaryOutputPath: temporaryPath, PublishedOutputPath: publishedPath, ArtifactPath: "transcript/event.json", Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}

	rawPrefix := temporaryPath + ".whisper"
	wantArguments := []string{"-m", modelPath, "-f", inputPath, "-oj", "-of", rawPrefix, "-l", "ko", "-t", "4", "-np"}
	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("whisper.cpp arguments=%q want=%q", gotArguments, wantArguments)
	}
	if _, err := os.Stat(rawPrefix + ".json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raw whisper output remained after normalization: %v", err)
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary transcript remained after publish: %v", err)
	}
	published, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(published), "private-host") || strings.Contains(string(published), "/secret/models") {
		t.Fatalf("raw host or model path leaked into transcript: %s", published)
	}
	var document transcriptDocument
	if err := json.Unmarshal(published, &document); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(published)
	artifact := transcript.Artifact
	if document.SchemaVersion != 1 || document.ProfileReference != "event-ko@v1" || document.Content.Language != "ko" ||
		len(document.Content.Segments) != 2 || document.Content.Segments[0].Text != "첫 문장" ||
		artifact.ID != "TRANSCRIPT-1" || artifact.Path != "transcript/event.json" || artifact.Size != int64(len(published)) ||
		artifact.Checksum != hex.EncodeToString(digest[:]) || artifact.Origin.OriginalAssetID != "ASSET-1" ||
		!reflect.DeepEqual(artifact.Origin.InputArtifactIDs, []string{"AUDIO-1"}) || artifact.Origin.ProcessingKind != vocabulary.ProcessingSTT ||
		artifact.Origin.ProfileVersion != "event-ko@v1" || artifact.Origin.ProcessorVersion != "whisper.cpp-b6250" || artifact.Origin.CreatedAt.IsZero() {
		t.Fatalf("transcript output or provenance is incomplete: document=%+v transcript=%+v", document, transcript)
	}

	t.Setenv("WHISPER_JSON", `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":0,"to":1500},"text":"first"},{"offsets":{"from":1400,"to":2000},"text":"overlap"}]}`)
	invalidTemporaryPath := filepath.Join(directory, "output", "invalid.json.partial")
	invalidPublishedPath := filepath.Join(directory, "output", "invalid.json")
	_, err = transcriber.Transcribe(context.Background(), Request{
		TranscriptID: "TRANSCRIPT-2", AudioArtifact: audioInput, ResolvedAudioPath: inputPath, ResolvedModelPath: modelPath,
		TemporaryOutputPath: invalidTemporaryPath, PublishedOutputPath: invalidPublishedPath, ArtifactPath: "transcript/invalid.json", Profile: profile,
	})
	if !errors.Is(err, ErrTranscriptionFailed) {
		t.Fatalf("invalid whisper output was not rejected: %v", err)
	}
	if _, err := os.Stat(invalidPublishedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid transcript was published: %v", err)
	}
}
