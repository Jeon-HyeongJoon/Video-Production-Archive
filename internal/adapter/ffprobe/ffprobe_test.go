package ffprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const ffprobeFixtureProcess = "VIDEO_ARCHIVE_FFPROBE_FIXTURE_PROCESS"

func TestMain(m *testing.M) {
	if os.Getenv(ffprobeFixtureProcess) == "1" {
		arguments := strings.Join(os.Args[1:], "\n") + "\n"
		if err := os.WriteFile(os.Getenv("FFPROBE_FIXTURE_ARGUMENTS"), []byte(arguments), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		payload, err := os.ReadFile(os.Getenv("FFPROBE_FIXTURE_JSON"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := os.Stdout.Write(payload); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func ffprobeFixtureExecutable(t *testing.T, argumentsPath, fixturePath string) string {
	t.Helper()
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ffprobeFixtureProcess, "1")
	t.Setenv("FFPROBE_FIXTURE_ARGUMENTS", argumentsPath)
	t.Setenv("FFPROBE_FIXTURE_JSON", fixturePath)
	return executablePath
}

// Requirement: MED-002
func TestFFprobeAdapterExecutesArgumentArrayAndCreatesMediaInspection(t *testing.T) {
	directory := t.TempDir()
	fixturePath := filepath.Join(directory, "ffprobe.json")
	argumentsPath := filepath.Join(directory, "arguments.txt")
	fixture := `{
  "format": {
    "format_name": "mov,mp4,m4a,3gp,3g2,mj2",
    "duration": "60.060000",
    "size": "125829120",
    "bit_rate": "16760539"
  },
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080,
     "avg_frame_rate": "30000/1001", "r_frame_rate": "30000/1001", "pix_fmt": "yuv420p"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "sample_rate": "48000",
     "channels": 2, "channel_layout": "stereo"}
  ]
}`
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	executablePath := ffprobeFixtureExecutable(t, argumentsPath, fixturePath)

	inspector, err := NewInspector(executablePath, "ffprobe-8.0", "WORKER-1", OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	resolvedPath := filepath.Join(directory, "camera", "clip & keep.mov")
	inspection, err := inspector.Inspect(context.Background(), Request{
		InspectionID: "INSPECTION-1", OriginalAssetID: "ASSET-1",
		OriginalChecksum:  strings.Repeat("a", 64),
		ResolvedInputPath: resolvedPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-i", resolvedPath}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("ffprobe arguments=%q want=%q", gotArguments, wantArguments)
	}
	if inspection.ID != "INSPECTION-1" || inspection.OriginalAssetID != "ASSET-1" ||
		inspection.DurationMillis != 60_060 || inspection.SizeBytes != 125_829_120 || inspection.BitRate != 16_760_539 ||
		len(inspection.VideoStreams) != 1 || inspection.VideoStreams[0].Codec != "h264" ||
		inspection.VideoStreams[0].FrameRate.Numerator != 30_000 || inspection.VideoStreams[0].FrameRate.Denominator != 1_001 ||
		len(inspection.AudioStreams) != 1 || inspection.AudioStreams[0].SampleRateHz != 48_000 ||
		inspection.ProcessorVersion != "ffprobe-8.0" || inspection.ObservedBy != "WORKER-1" || inspection.InspectedAt.IsZero() {
		t.Fatalf("ffprobe result was not mapped to MediaInspection: %+v", inspection)
	}
}
