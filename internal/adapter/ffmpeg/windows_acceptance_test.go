//go:build windows

package ffmpeg

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
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"videoarchive/internal/core/audio"
	"videoarchive/internal/core/rendering"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

type processMemoryCounters struct {
	Size                    uint32
	PageFaultCount          uint32
	PeakWorkingSetSize      uintptr
	WorkingSetSize          uintptr
	QuotaPeakPagedPoolUsage uintptr
	QuotaPagedPoolUsage     uintptr
	QuotaPeakNonPagedUsage  uintptr
	QuotaNonPagedUsage      uintptr
	PagefileUsage           uintptr
	PeakPagefileUsage       uintptr
}

type measuredWindowsRunner struct {
	Elapsed             time.Duration
	PeakWorkingSetBytes uint64
}

func (runner *measuredWindowsRunner) Run(ctx context.Context, executable string, arguments []string) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	startedAt := time.Now()
	if err := command.Start(); err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(command.Process.Pid))
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	defer windows.CloseHandle(handle)
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waited:
			runner.observePeak(handle)
			runner.Elapsed = time.Since(startedAt)
			return err
		case <-ticker.C:
			runner.observePeak(handle)
		}
	}
}

func (runner *measuredWindowsRunner) observePeak(handle windows.Handle) {
	counters := processMemoryCounters{Size: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	result, _, _ := getProcessMemoryInfo.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size),
	)
	if result != 0 && uint64(counters.PeakWorkingSetSize) > runner.PeakWorkingSetBytes {
		runner.PeakWorkingSetBytes = uint64(counters.PeakWorkingSetSize)
	}
}

type actualFFprobeVerifier struct {
	executable string
	kind       string
	observed   probeObservation
}

type probeObservation struct {
	FormatName  string
	VideoCodec  string
	AudioCodec  string
	PixelFormat string
	Width       int
	Height      int
	SampleRate  int
	Channels    int
}

func (verifier *actualFFprobeVerifier) Verify(path string) error {
	output, err := exec.Command(verifier.executable,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-i", path,
	).Output()
	if err != nil {
		return err
	}
	var document struct {
		Format struct {
			Name string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			Type        string `json:"codec_type"`
			Codec       string `json:"codec_name"`
			PixelFormat string `json:"pix_fmt"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			SampleRate  string `json:"sample_rate"`
			Channels    int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		return err
	}
	verifier.observed.FormatName = document.Format.Name
	for _, stream := range document.Streams {
		switch stream.Type {
		case "video":
			verifier.observed.VideoCodec = stream.Codec
			verifier.observed.PixelFormat = stream.PixelFormat
			verifier.observed.Width = stream.Width
			verifier.observed.Height = stream.Height
		case "audio":
			verifier.observed.AudioCodec = stream.Codec
			verifier.observed.Channels = stream.Channels
			_, _ = fmt.Sscanf(stream.SampleRate, "%d", &verifier.observed.SampleRate)
		}
	}
	switch verifier.kind {
	case "video":
		if !strings.Contains(verifier.observed.FormatName, "mp4") || verifier.observed.VideoCodec != "h264" ||
			verifier.observed.PixelFormat != "yuv420p" || verifier.observed.Width <= 0 || verifier.observed.Width > 1920 ||
			verifier.observed.Height <= 0 || verifier.observed.Height > 1080 || verifier.observed.AudioCodec != "aac" {
			return errors.New("rendered output does not match H.264/AAC MP4 acceptance contract")
		}
	case "audio":
		if !strings.Contains(verifier.observed.FormatName, "wav") || verifier.observed.AudioCodec != "pcm_s16le" ||
			verifier.observed.SampleRate != 16_000 || verifier.observed.Channels != 1 || verifier.observed.VideoCodec != "" {
			return errors.New("audio output does not match mono 16kHz PCM WAV acceptance contract")
		}
	default:
		return errors.New("unknown acceptance verifier kind")
	}
	return nil
}

// Requirement: RND-003
func TestWindowsFFmpegRendersVerifiedThreadLimitedH264AACRendition(t *testing.T) {
	ffmpegPath, ffprobePath, version := windowsFFmpegAcceptanceTools(t)
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "representative input.mp4")
	createRepresentativeInput(t, ffmpegPath, inputPath)
	before := acceptanceFileDigest(t, inputPath)
	runner := &measuredWindowsRunner{}
	verifier := &actualFFprobeVerifier{executable: ffprobePath, kind: "video"}
	renderer, err := NewRenderer(ffmpegPath, version, "WINDOWS-ACCEPTANCE-WORKER", runner, verifier, AtomicPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	profile := rendering.Profile{
		ID: "archive-event-1080p", Version: 1, Container: rendering.ContainerMP4,
		VideoEncoder: rendering.VideoEncoderX264, AudioEncoder: rendering.AudioEncoderAAC,
		PixelFormat: rendering.PixelFormatYUV420P, MaxWidth: 1920, MaxHeight: 1080,
		ConstantRateFactor: 20, AudioBitRateKbps: 192, ThreadCount: 2,
	}
	publishedPath := filepath.Join(directory, "rendered.mp4")
	result, err := renderer.Render(context.Background(), Request{
		RenditionID: "RENDITION-WINDOWS-1", OriginalAssetID: "ASSET-WINDOWS-1", ResolvedInputPath: inputPath,
		TemporaryOutputPath: publishedPath + ".partial", PublishedOutputPath: publishedPath,
		ArtifactPath: "acceptance/rendered.mp4", Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionEvidence(t, "RND-003", version, runner, result.Artifact.Checksum, map[string]any{
		"profile": result.Artifact.Origin.ProfileVersion, "thread_count": profile.ThreadCount,
		"video_codec": verifier.observed.VideoCodec, "audio_codec": verifier.observed.AudioCodec,
		"width": verifier.observed.Width, "height": verifier.observed.Height,
	})
	if result.Artifact.Checksum != acceptanceFileDigest(t, publishedPath) || result.Artifact.Origin.CreatedBy != "WINDOWS-ACCEPTANCE-WORKER" {
		t.Fatalf("render evidence is incomplete: %+v", result)
	}
	if after := acceptanceFileDigest(t, inputPath); after != before {
		t.Fatalf("source checksum changed: before=%s after=%s", before, after)
	}
}

// Requirement: AUD-003
func TestWindowsFFmpegExtractsVerifiedMono16kPCMAudio(t *testing.T) {
	ffmpegPath, ffprobePath, version := windowsFFmpegAcceptanceTools(t)
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "representative audio input.mp4")
	createRepresentativeInput(t, ffmpegPath, inputPath)
	before := acceptanceFileDigest(t, inputPath)
	runner := &measuredWindowsRunner{}
	verifier := &actualFFprobeVerifier{executable: ffprobePath, kind: "audio"}
	extractor, err := NewAudioExtractor(ffmpegPath, version, "WINDOWS-ACCEPTANCE-WORKER", runner, verifier, AtomicPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	profile := audio.Profile{
		ID: "speech-pcm-16k", Version: 1, Container: audio.ContainerWAV, Codec: audio.CodecPCM16LE,
		SampleRateHz: audio.SpeechRateHz, Channels: audio.SpeechChannels, ThreadCount: 2,
	}
	publishedPath := filepath.Join(directory, "speech.wav")
	result, err := extractor.Extract(context.Background(), AudioRequest{
		AudioArtifactID: "AUDIO-WINDOWS-1", OriginalAssetID: "ASSET-WINDOWS-1", ResolvedInputPath: inputPath,
		TemporaryOutputPath: publishedPath + ".partial", PublishedOutputPath: publishedPath,
		ArtifactPath: "acceptance/speech.wav", Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionEvidence(t, "AUD-003", version, runner, result.Artifact.Checksum, map[string]any{
		"profile": result.Artifact.Origin.ProfileVersion, "thread_count": profile.ThreadCount,
		"audio_codec": verifier.observed.AudioCodec, "sample_rate_hz": verifier.observed.SampleRate,
		"channels": verifier.observed.Channels,
	})
	if result.Artifact.Checksum != acceptanceFileDigest(t, publishedPath) || result.Artifact.Origin.CreatedBy != "WINDOWS-ACCEPTANCE-WORKER" {
		t.Fatalf("audio evidence is incomplete: %+v", result)
	}
	if after := acceptanceFileDigest(t, inputPath); after != before {
		t.Fatalf("source checksum changed: before=%s after=%s", before, after)
	}
}

func windowsFFmpegAcceptanceTools(t *testing.T) (string, string, string) {
	t.Helper()
	if os.Getenv("ARCHIVE_WINDOWS_MEDIA_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_MEDIA_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	ffmpegPath := os.Getenv("ARCHIVE_FFMPEG_PATH")
	ffprobePath := os.Getenv("ARCHIVE_FFPROBE_PATH")
	if ffmpegPath == "" || ffprobePath == "" {
		t.Fatal("ARCHIVE_FFMPEG_PATH and ARCHIVE_FFPROBE_PATH are required")
	}
	versionOutput, err := exec.Command(ffmpegPath, "-version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return ffmpegPath, ffprobePath, strings.TrimSpace(strings.SplitN(string(versionOutput), "\n", 2)[0])
}

func createRepresentativeInput(t *testing.T, ffmpegPath, outputPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "4", "-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-b:v", "4000k",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-threads", "2", "-shortest", outputPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("create representative input: %v: %s", err, output)
	}
}

func acceptanceFileDigest(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func assertExecutionEvidence(t *testing.T, requirementID, version string, runner *measuredWindowsRunner, checksum string, values map[string]any) {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if runner.Elapsed <= 0 || runner.PeakWorkingSetBytes == 0 || checksum == "" || host == "" {
		t.Fatalf("execution evidence is incomplete: elapsed=%s peak=%d checksum=%s host=%s", runner.Elapsed, runner.PeakWorkingSetBytes, checksum, host)
	}
	values["requirement_id"] = requirementID
	values["worker_id"] = "WINDOWS-ACCEPTANCE-WORKER"
	values["host"] = host
	values["processor_version"] = version
	values["checksum"] = checksum
	values["elapsed_millis"] = runner.Elapsed.Milliseconds()
	values["peak_working_set_bytes"] = runner.PeakWorkingSetBytes
	payload, _ := json.Marshal(values)
	t.Logf("acceptance_evidence=%s", payload)
}
