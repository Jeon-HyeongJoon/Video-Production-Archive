package whispercpp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/transcription"
	"videoarchive/internal/core/vocabulary"
)

const audioChecksum = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const whisperFixtureProcess = "ARCHIVE_WHISPER_FIXTURE_PROCESS"

func TestMain(m *testing.M) {
	if os.Getenv(whisperFixtureProcess) == "1" {
		arguments := os.Args[1:]
		if err := os.WriteFile(os.Getenv("WHISPER_FIXTURE_ARGUMENTS"), []byte(strings.Join(arguments, "\n")+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		outputPrefix := ""
		for index := 0; index+1 < len(arguments); index++ {
			if arguments[index] == "-of" {
				outputPrefix = arguments[index+1]
				break
			}
		}
		if outputPrefix == "" {
			fmt.Fprintln(os.Stderr, "whisper fixture received no -of argument")
			os.Exit(2)
		}
		payload := os.Getenv("WHISPER_FIXTURE_JSON")
		if table := os.Getenv("WHISPER_FIXTURE_BY_OFFSET"); table != "" {
			var responses map[string]string
			if err := json.Unmarshal([]byte(table), &responses); err != nil {
				panic(err)
			}
			for i, arg := range arguments {
				if arg == "-ot" {
					payload = responses[arguments[i+1]]
				}
			}
		}
		if err := os.WriteFile(outputPrefix+".json", []byte(payload), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type chunkFixtureRunner struct {
	calls  [][]string
	failAt int
}

func (runner *chunkFixtureRunner) Run(_ context.Context, _ string, arguments []string) error {
	runner.calls = append(runner.calls, append([]string(nil), arguments...))
	if runner.failAt == len(runner.calls) {
		return context.Canceled
	}
	value := func(flag string) string {
		for index := 0; index+1 < len(arguments); index++ {
			if arguments[index] == flag {
				return arguments[index+1]
			}
		}
		return ""
	}
	prefix := value("-of")
	duration, _ := strconv.ParseInt(value("-d"), 10, 64)
	if prefix == "" || duration <= 200 {
		return errors.New("invalid chunk fixture arguments")
	}
	start, _ := strconv.ParseInt(value("-ot"), 10, 64)
	payload := fmt.Sprintf(`{"result":{"language":"ko"},"transcription":[{"offsets":{"from":%d,"to":%d},"text":"청크"}]}`, start+100, start+minInt64(duration-100, 900))
	return os.WriteFile(prefix+".json", []byte(payload), 0o600)
}

func fixturePCM16WAV(t *testing.T, durationMillis int64, silent func(int64) bool) []byte {
	t.Helper()
	const sampleRate = 16_000
	sampleCount := durationMillis * sampleRate / 1000
	dataSize := sampleCount * 2
	var payload bytes.Buffer
	payload.WriteString("RIFF")
	_ = binary.Write(&payload, binary.LittleEndian, uint32(36+dataSize))
	payload.WriteString("WAVEfmt ")
	_ = binary.Write(&payload, binary.LittleEndian, uint32(16))
	_ = binary.Write(&payload, binary.LittleEndian, uint16(1))
	_ = binary.Write(&payload, binary.LittleEndian, uint16(1))
	_ = binary.Write(&payload, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&payload, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&payload, binary.LittleEndian, uint16(2))
	_ = binary.Write(&payload, binary.LittleEndian, uint16(16))
	payload.WriteString("data")
	_ = binary.Write(&payload, binary.LittleEndian, uint32(dataSize))
	for sample := int64(0); sample < sampleCount; sample++ {
		millis := sample * 1000 / sampleRate
		level := int16(3000)
		if silent(millis) {
			level = 0
		}
		_ = binary.Write(&payload, binary.LittleEndian, level)
	}
	return payload.Bytes()
}

func whisperFixtureExecutable(t *testing.T, argumentsPath string) string {
	t.Helper()
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(whisperFixtureProcess, "1")
	t.Setenv("WHISPER_FIXTURE_ARGUMENTS", argumentsPath)
	return executablePath
}

// Requirement: TRN-002
func TestWhisperCPPAdapterExecutesArgumentArrayAndPublishesVerifiedTranscript(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments.txt")
	executablePath := whisperFixtureExecutable(t, argumentsPath)
	rawJSON := `{"systeminfo":"private-host","params":{"model":"/secret/models/private.bin"},"result":{"language":"ko"},"transcription":[{"timestamps":{"from":"00:00:00,000","to":"00:00:01,250"},"offsets":{"from":0,"to":1250},"text":" 첫 문장 "},{"timestamps":{"from":"00:00:01,400","to":"00:00:02,800"},"offsets":{"from":1400,"to":2800},"text":"두 번째 문장"}]}`
	t.Setenv("WHISPER_FIXTURE_JSON", rawJSON)

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
	createInput := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, fixturePCM16WAV(t, 3000, func(int64) bool { return false }), 0600); err != nil {
			t.Fatal(err)
		}
	}
	createInput(inputPath)
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

	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if len(gotArguments) != 12 {
		t.Fatalf("unexpected arguments: %q", gotArguments)
	}
	rawPrefix := gotArguments[6]
	if filepath.Dir(rawPrefix) != filepath.Dir(temporaryPath) || rawPrefix+".json" == publishedPath {
		t.Fatalf("unexpected raw output target: %q", rawPrefix)
	}
	wantArguments := []string{"-m", modelPath, "-f", inputPath, "-oj", "-of", rawPrefix, "-l", "ko", "-t", "4", "-np"}
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

	t.Setenv("WHISPER_FIXTURE_JSON", `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":0,"to":1500},"text":"first"},{"offsets":{"from":1400,"to":2000},"text":"overlap"}]}`)
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

	t.Run("single and chunked engines keep absolute cues within measured PCM", func(t *testing.T) {
		for _, duration := range []int64{6278, 10000} {
			t.Run(strconv.FormatInt(duration, 10), func(t *testing.T) {
				root, err := archive.OpenStorageRoot(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				wav := fixturePCM16WAV(t, duration, func(ms int64) bool {
					return (ms >= 2500 && ms < 3100) || (ms >= 5700 && ms < 6300)
				})
				if duration == 6278 {
					// The WAV header advertises more samples than the file contains.
					binary.LittleEndian.PutUint32(wav[40:], uint32(len(wav)-44+32000))
				}
				if err := root.WriteFile("speech.wav", wav, 0600); err != nil {
					t.Fatal(err)
				}
				input := audioInput
				input.Artifact.Path, input.Artifact.Size, input.Artifact.Checksum = "speech.wav", int64(len(wav)), fmt.Sprintf("%x", sha256.Sum256(wav))
				engine := *transcriber
				first := `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":0,"to":500},"text":"시작"}]}`
				last := fmt.Sprintf(`{"result":{"language":"ko"},"transcription":[{"offsets":{"from":%d,"to":%d},"text":"마지막"},{"offsets":{"from":%d,"to":%d},"text":"밖"}]}`, duration-50, duration+900, duration+901, duration+1000)
				want := []transcription.Segment{{Sequence: 1, StartMillis: duration - 50, EndMillis: duration, Text: "마지막"}}
				if duration == 6278 {
					t.Setenv("WHISPER_FIXTURE_JSON", last)
				} else {
					engine.chunkPolicy = chunkPolicy{MinimumMillis: 2000, TargetMillis: 3000, MaximumMillis: 4000, SilenceMillis: 400, ContextMillis: 500}
					middle := `{"result":{"language":"ko"},"transcription":[{"offsets":{"from":100,"to":900},"text":"범위 밖"},{"offsets":{"from":3300,"to":3480},"text":"가운데"}]}`
					table, err := json.Marshal(map[string]string{"0": first, "2800": middle, "6000": last})
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("WHISPER_FIXTURE_BY_OFFSET", string(table))
					want = []transcription.Segment{
						{Sequence: 1, StartMillis: 0, EndMillis: 500, Text: "시작"},
						{Sequence: 2, StartMillis: 3300, EndMillis: 3480, Text: "가운데"},
						{Sequence: 3, StartMillis: 9950, EndMillis: 10000, Text: "마지막"},
					}
				}
				inputPath, _ := root.ExternalProcessPath(input.Artifact.Path)
				tmp, _ := root.ExternalProcessPath("out/transcript.partial")
				final, _ := root.ExternalProcessPath("out/transcript.json")
				got, err := engine.Transcribe(context.Background(), Request{
					TranscriptID: "TRANSCRIPT-BOUNDS", AudioArtifact: input, InputRoot: root,
					ResolvedAudioPath: inputPath, ResolvedModelPath: modelPath, Profile: profile,
					OutputRoot: root, ArtifactPath: "out/transcript.json", TemporaryArtifactPath: "out/transcript.partial",
					TemporaryOutputPath: tmp, PublishedOutputPath: final,
				})
				if err != nil || !reflect.DeepEqual(got.Content.Segments, want) {
					t.Fatalf("published cues = %+v, want %+v, err = %v", got.Content.Segments, want, err)
				}
			})
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("long Windows file arguments retain the resolved paths", func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), strings.Repeat("a", 90), strings.Repeat("b", 90), strings.Repeat("c", 80))
			model, audio := filepath.Join(directory, "model ;.bin"), filepath.Join(directory, "audio &.wav")
			output := filepath.Join(directory, "transcript.json")
			createInput(audio)
			t.Setenv("WHISPER_FIXTURE_JSON", rawJSON)
			_, err := transcriber.Transcribe(context.Background(), Request{
				TranscriptID: "TRANSCRIPT-LONG", AudioArtifact: audioInput, ResolvedAudioPath: audio, ResolvedModelPath: model,
				TemporaryOutputPath: output + ".partial", PublishedOutputPath: output, ArtifactPath: "transcript/event.json", Profile: profile,
			})
			if err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(argumentsPath)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(args)), "\n")
			want := map[string]string{"-m": model, "-f": audio, "-of": ""}
			for i := 0; i+1 < len(fields); i++ {
				if name, ok := want[fields[i]]; ok {
					if fields[i] == "-of" {
						name = filepath.Join(directory, filepath.Base(fields[i+1]))
					}
					extended := `\\?\` + name
					if server, unc := strings.CutPrefix(name, `\\`); unc {
						extended = `\\?\UNC\` + server
					}
					if fields[i+1] != extended {
						t.Fatalf("%s does not identify the extended path to %s", fields[i], name)
					}
					delete(want, fields[i])
				}
			}
			if len(want) != 0 {
				t.Fatalf("missing long path arguments: %v", want)
			}
			if _, err := os.Stat(output); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("long audio uses silence-aware bounded chunks and global timecodes", func(t *testing.T) {
		rootDirectory := t.TempDir()
		root, err := archive.OpenStorageRoot(rootDirectory)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		wave := fixturePCM16WAV(t, 10_000, func(millis int64) bool {
			return (millis >= 2_500 && millis < 3_100) || (millis >= 5_700 && millis < 6_300)
		})
		if err := root.MkdirAll("audio", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile("audio/event.wav", wave, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(wave)
		chunkAudio, err := audio.NewAudioArtifact(archive.Artifact{
			ID: "AUDIO-CHUNKED", Path: "audio/event.wav", Size: int64(len(wave)), Checksum: hex.EncodeToString(sum[:]),
			Origin: audioArtifact.Origin,
		}, audioProfile)
		if err != nil {
			t.Fatal(err)
		}
		runner := &chunkFixtureRunner{}
		chunkTranscriber, err := NewTranscriber("whisper-cli", "whisper.cpp-b6250", "WORKER-1", runner, AtomicPublisher{})
		if err != nil {
			t.Fatal(err)
		}
		chunkTranscriber.chunkPolicy = chunkPolicy{MinimumMillis: 2_000, TargetMillis: 3_000, MaximumMillis: 4_000, SilenceMillis: 400, ContextMillis: 500}
		temporaryRelative := "staging/chunked.json.partial"
		publishedRelative := "staging/chunked.json"
		temporaryResolved, _ := root.ExternalProcessPath(temporaryRelative)
		publishedResolved, _ := root.ExternalProcessPath(publishedRelative)
		audioResolved, _ := root.ExternalProcessPath("audio/event.wav")
		var progress []execution.ProgressUpdate
		ctx := execution.WithProgressReporter(context.Background(), func(_ context.Context, update execution.ProgressUpdate) error {
			progress = append(progress, update)
			return nil
		})
		result, err := chunkTranscriber.Transcribe(ctx, Request{
			TranscriptID: "TRANSCRIPT-CHUNKED", AudioArtifact: chunkAudio, InputRoot: root,
			ResolvedAudioPath: audioResolved, ResolvedModelPath: modelPath,
			TemporaryOutputPath: temporaryResolved, PublishedOutputPath: publishedResolved,
			ArtifactPath: publishedRelative, TemporaryArtifactPath: temporaryRelative, OutputRoot: root, Profile: profile,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.calls) != 3 || len(result.Content.Segments) != 3 || len(progress) != 7 ||
			progress[0].Status != execution.ProgressPlanned || progress[0].ChunkCount != 3 ||
			progress[len(progress)-1].Status != execution.ProgressCompleted || progress[len(progress)-1].Progress != 1 ||
			result.Content.Segments[0].StartMillis != 100 || result.Content.Segments[1].StartMillis < 2_800 ||
			result.Content.Segments[2].StartMillis < 6_000 {
			t.Fatalf("chunked transcription evidence is incomplete: calls=%d progress=%+v content=%+v", len(runner.calls), progress, result.Content)
		}
		for _, call := range runner.calls {
			if !containsArgument(call, "-ot") || !containsArgument(call, "-d") {
				t.Fatalf("bounded chunk arguments missing: %q", call)
			}
		}

		failureRunner := &chunkFixtureRunner{failAt: 2}
		failureTranscriber, err := NewTranscriber("whisper-cli", "whisper.cpp-b6250", "WORKER-1", failureRunner, AtomicPublisher{})
		if err != nil {
			t.Fatal(err)
		}
		failureTranscriber.chunkPolicy = chunkTranscriber.chunkPolicy
		failedTemporaryRelative := "staging/failed.json.partial"
		failedPublishedRelative := "staging/failed.json"
		failedTemporaryResolved, _ := root.ExternalProcessPath(failedTemporaryRelative)
		failedPublishedResolved, _ := root.ExternalProcessPath(failedPublishedRelative)
		var failureProgress []execution.ProgressUpdate
		failureContext := execution.WithProgressReporter(context.Background(), func(_ context.Context, update execution.ProgressUpdate) error {
			failureProgress = append(failureProgress, update)
			return nil
		})
		_, err = failureTranscriber.Transcribe(failureContext, Request{
			TranscriptID: "TRANSCRIPT-FAILED-CHUNK", AudioArtifact: chunkAudio, InputRoot: root,
			ResolvedAudioPath: audioResolved, ResolvedModelPath: modelPath,
			TemporaryOutputPath: failedTemporaryResolved, PublishedOutputPath: failedPublishedResolved,
			ArtifactPath: failedPublishedRelative, TemporaryArtifactPath: failedTemporaryRelative, OutputRoot: root, Profile: profile,
		})
		last := failureProgress[len(failureProgress)-1]
		if !errors.Is(err, ErrTranscriptionFailed) || last.Status != execution.ProgressFailed || last.ChunkIndex != 2 ||
			last.SourceStartMillis < 2_500 || last.SourceEndMillis < 5_700 {
			t.Fatalf("failed chunk range was not reported: progress=%+v err=%v", failureProgress, err)
		}
	})
}

func containsArgument(arguments []string, value string) bool {
	for _, argument := range arguments {
		if argument == value {
			return true
		}
	}
	return false
}

// Requirement: TRN-010
func TestRetryReusesOnlyVerifiedChunksOfTheSameTranscription(t *testing.T) {
	for _, change := range []string{"same", "model", "audio", "profile", "boundaries", "processor", "corrupt checkpoint"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			root, err := archive.OpenStorageRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			write := func(name string, data []byte) {
				t.Helper()
				if err := root.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			wav := fixturePCM16WAV(t, 10000, func(int64) bool { return false })
			write("speech.wav", wav)
			write("model.bin", []byte("version A"))
			audioProfile, err := audio.KoreanSpeechEnhancementProfile(2)
			if err != nil {
				t.Fatal(err)
			}
			input, err := audio.NewAudioArtifact(archive.Artifact{
				ID: "AUDIO-1", Path: "speech.wav", Size: int64(len(wav)), Checksum: fmt.Sprintf("%x", sha256.Sum256(wav)),
				Origin: archive.Provenance{
					OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingAudioExtract,
					ProcessorVersion: "ffmpeg", ProfileVersion: audioProfile.Reference(), CreatedBy: "W1", CreatedAt: time.Now().UTC(),
				},
			}, audioProfile)
			if err != nil {
				t.Fatal(err)
			}
			resolved := func(name string) string {
				t.Helper()
				p, err := root.ExternalProcessPath(name)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			request := Request{
				TranscriptID: "T1", AudioArtifact: input, InputRoot: root, ResolvedAudioPath: resolved("speech.wav"),
				ModelRoot: root, ModelRelativePath: "model.bin", ResolvedModelPath: resolved("model.bin"),
				OutputRoot: root, ArtifactPath: "out/T1.json", PublishedOutputPath: resolved("out/T1.json"),
				TemporaryArtifactPath: "out/T1.partial.1", TemporaryOutputPath: resolved("out/T1.partial.1"),
				Profile: transcription.Profile{ID: "event-ko", Version: 1, ModelID: "model", Language: "ko", ThreadCount: 2},
			}
			runner := &chunkFixtureRunner{failAt: 2}
			engine, err := NewTranscriber("fixture", "processor-1", "W1", runner, AtomicPublisher{})
			if err != nil {
				t.Fatal(err)
			}
			engine.chunkPolicy = chunkPolicy{MinimumMillis: 2000, TargetMillis: 3000, MaximumMillis: 4000, SilenceMillis: 400, ContextMillis: 100}
			if _, err := engine.Transcribe(context.Background(), request); !errors.Is(err, context.Canceled) {
				t.Fatalf("interruption: %v", err)
			}
			if _, err := root.Stat(request.ArtifactPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted publication: %v", err)
			}
			switch change {
			case "model":
				write("model.bin", []byte("version B"))
			case "audio":
				wav[44] ^= 1
				write("speech.wav", wav)
				if _, err := engine.Transcribe(context.Background(), request); err == nil {
					t.Fatal("changed audio was accepted with the old verified checksum")
				}
				request.AudioArtifact.Artifact.Checksum = fmt.Sprintf("%x", sha256.Sum256(wav))
			case "profile":
				request.Profile.ThreadCount++
			case "boundaries":
				engine.chunkPolicy.ContextMillis++
			case "processor":
				engine.processorVersion = "processor-2"
			case "corrupt checkpoint":
				// Preserve valid JSON and timings. The altered text must still miss.
				files, err := filepath.Glob(filepath.Join(dir, "staging", ".archive-stt", "*", "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("checkpoint files: %v (%v)", files, err)
				}
				data, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(data, []byte("청크")) {
					t.Fatal("missing completed cue")
				}
				if err := os.WriteFile(files[0], bytes.ReplaceAll(data, []byte("청크"), []byte("손상")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			request.TemporaryArtifactPath = "out/T1.partial.2"
			request.TemporaryOutputPath = resolved(request.TemporaryArtifactPath)
			runner.calls = nil
			runner.failAt = 0
			got, err := engine.Transcribe(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			retried := append([][]string(nil), runner.calls...)
			runner.calls = nil
			// A clean target forces a full run of this exact input and plan.
			request.ArtifactPath = "out/reference.json"
			request.PublishedOutputPath = resolved(request.ArtifactPath)
			fresh, err := engine.Transcribe(context.Background(), request)
			if err != nil || got.Artifact.Checksum != fresh.Artifact.Checksum || !reflect.DeepEqual(got.Content, fresh.Content) {
				t.Fatalf("retry differs from fresh result: %+v %+v %v", got.Content, fresh.Content, err)
			}
			offsets := func(calls [][]string) []string {
				var starts []string
				for _, args := range calls {
					for i, arg := range args {
						if arg == "-ot" {
							starts = append(starts, args[i+1])
							break
						}
					}
				}
				return starts
			}
			want := offsets(runner.calls)
			if change == "same" {
				want = want[1:]
			}
			if actual := offsets(retried); !reflect.DeepEqual(actual, want) {
				t.Fatalf("retry engine offsets %v, want %v", actual, want)
			}
			if files, _ := filepath.Glob(filepath.Join(dir, "staging", ".archive-stt", "*", "*.json")); len(files) != 0 {
				t.Fatalf("published target retained private chunks: %v", files)
			}
		})
	}
}
