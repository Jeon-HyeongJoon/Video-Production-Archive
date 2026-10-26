package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: AUD-002
func TestFFmpegAdapterExtractsVerifiedSpeechAudioWithoutShell(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments.txt")
	executablePath := filepath.Join(directory, "ffmpeg-audio-fixture")
	payload := []byte("verified-pcm-wave")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FFMPEG_AUDIO_ARGUMENTS\"\nfor argument in \"$@\"; do output=\"$argument\"; done\nprintf '%s' \"$FFMPEG_AUDIO_PAYLOAD\" > \"$output\"\n"
	if err := os.WriteFile(executablePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_AUDIO_ARGUMENTS", argumentsPath)
	t.Setenv("FFMPEG_AUDIO_PAYLOAD", string(payload))

	extractor, err := NewAudioExtractor(executablePath, "ffmpeg-8.0", "WORKER-1", OSRunner{}, payloadVerifier{expected: payload}, AtomicPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	profile := audio.Profile{
		ID: "speech-pcm-16k", Version: 1, Container: audio.ContainerWAV, Codec: audio.CodecPCM16LE,
		SampleRateHz: audio.SpeechRateHz, Channels: audio.SpeechChannels, ThreadCount: 2,
	}
	inputPath := filepath.Join(directory, "camera", "event & keep.mov")
	temporaryPath := filepath.Join(directory, "output", "event.wav.partial")
	publishedPath := filepath.Join(directory, "output", "event.wav")
	result, err := extractor.Extract(context.Background(), AudioRequest{
		AudioArtifactID: "AUDIO-1", OriginalAssetID: "ASSET-1", ResolvedInputPath: inputPath,
		TemporaryOutputPath: temporaryPath, PublishedOutputPath: publishedPath, ArtifactPath: "audio/event.wav", Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantArguments := []string{
		"-hide_banner", "-nostdin", "-y", "-i", inputPath,
		"-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-threads", "2", "-f", "wav", temporaryPath,
	}
	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("ffmpeg audio arguments=%q want=%q", gotArguments, wantArguments)
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary audio remained after publish: %v", err)
	}
	published, err := os.ReadFile(publishedPath)
	if err != nil || !reflect.DeepEqual(published, payload) {
		t.Fatalf("verified audio was not published: payload=%q err=%v", published, err)
	}
	digest := sha256.Sum256(payload)
	artifact := result.Artifact
	if result.ProfileID != "speech-pcm-16k" || result.ProfileVersion != 1 || result.Container != audio.ContainerWAV ||
		result.Codec != audio.CodecPCM16LE || result.SampleRateHz != 16_000 || result.Channels != 1 ||
		artifact.ID != "AUDIO-1" || artifact.Path != "audio/event.wav" || artifact.Size != int64(len(payload)) ||
		artifact.Checksum != hex.EncodeToString(digest[:]) || artifact.Origin.OriginalAssetID != "ASSET-1" ||
		artifact.Origin.ProcessingKind != vocabulary.ProcessingAudioExtract || artifact.Origin.ProfileVersion != "speech-pcm-16k@v1" ||
		artifact.Origin.ProcessorVersion != "ffmpeg-8.0" || artifact.Origin.CreatedAt.IsZero() {
		t.Fatalf("audio artifact provenance is incomplete: %+v", result)
	}
}
