//go:build windows

package ffprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Requirement: MED-003
func TestWindowsFFprobeInspectsRepresentativeH264AACWithoutChangingSource(t *testing.T) {
	ffmpegPath, ffprobePath := windowsMediaAcceptanceTools(t)
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "representative h264 aac.mp4")
	createWindowsRepresentativeVideo(t, ffmpegPath, inputPath)
	before := fileDigest(t, inputPath)

	versionOutput, err := exec.Command(ffprobePath, "-version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(strings.SplitN(string(versionOutput), "\n", 2)[0])
	inspector, err := NewInspector(ffprobePath, version, "WINDOWS-ACCEPTANCE-WORKER", OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := inspector.Inspect(context.Background(), Request{
		InspectionID: "INSPECTION-WINDOWS-1", OriginalAssetID: "ASSET-WINDOWS-1",
		OriginalChecksum: before, ResolvedInputPath: inputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsValue(inspection.Containers, "mov") && !containsValue(inspection.Containers, "mp4") {
		t.Fatalf("containers=%v want MP4 family", inspection.Containers)
	}
	if inspection.DurationMillis < 3_900 || inspection.DurationMillis > 4_200 || inspection.SizeBytes <= 0 || inspection.BitRate <= 0 ||
		len(inspection.VideoStreams) != 1 || inspection.VideoStreams[0].Codec != "h264" || inspection.VideoStreams[0].Width != 1280 ||
		inspection.VideoStreams[0].Height != 720 || inspection.VideoStreams[0].BitRate <= 0 || len(inspection.AudioStreams) != 1 ||
		inspection.AudioStreams[0].Codec != "aac" || inspection.AudioStreams[0].SampleRateHz != 48_000 {
		t.Fatalf("actual ffprobe inspection is incomplete: %+v", inspection)
	}
	if after := fileDigest(t, inputPath); after != before {
		t.Fatalf("source checksum changed: before=%s after=%s", before, after)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal(map[string]any{
		"requirement_id": "MED-003", "worker_id": "WINDOWS-ACCEPTANCE-WORKER", "host": host,
		"processor_version": version, "source_checksum": before, "duration_millis": inspection.DurationMillis,
		"size_bytes": inspection.SizeBytes, "bit_rate": inspection.BitRate,
		"video_codec": inspection.VideoStreams[0].Codec, "audio_codec": inspection.AudioStreams[0].Codec,
	})
	t.Logf("acceptance_evidence=%s", evidence)
}

func windowsMediaAcceptanceTools(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("ARCHIVE_WINDOWS_MEDIA_ACCEPTANCE") != "1" {
		t.Skip("set ARCHIVE_WINDOWS_MEDIA_ACCEPTANCE=1 on the isolated Windows acceptance host")
	}
	ffmpegPath := os.Getenv("ARCHIVE_FFMPEG_PATH")
	ffprobePath := os.Getenv("ARCHIVE_FFPROBE_PATH")
	if ffmpegPath == "" || ffprobePath == "" {
		t.Fatal("ARCHIVE_FFMPEG_PATH and ARCHIVE_FFPROBE_PATH are required")
	}
	return ffmpegPath, ffprobePath
}

func createWindowsRepresentativeVideo(t *testing.T, ffmpegPath, outputPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "4", "-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-b:v", "4000k",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-threads", "2", "-shortest", outputPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create representative video: %v: %s", err, output)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
