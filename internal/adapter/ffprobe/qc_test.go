package ffprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Requirement: QCT-002
func TestFFprobeAdapterObservesPublishedRenditionBytesForQC(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "qc-arguments.txt")
	fixturePath := filepath.Join(directory, "qc-ffprobe.json")
	publishedPath := filepath.Join(directory, "proxy & verified.mp4")
	published := []byte("published-rendition-bytes")
	if err := os.WriteFile(publishedPath, published, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := `{
  "format": {"format_name":"mov,mp4", "duration":"60.040", "size":"26", "bit_rate":"2000000"},
  "streams": [
    {"index":2,"codec_type":"video","codec_name":"h264","width":1280,"height":720,"duration":"N/A","avg_frame_rate":"30/1"},
    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"duration":"58.025","avg_frame_rate":"30/1"},
    {"index":1,"codec_type":"audio","codec_name":"aac","sample_rate":"48000","channels":2}
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
	observation, err := inspector.InspectPublishedOutput(context.Background(), publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(published)
	if observation.Checksum != hex.EncodeToString(digest[:]) || observation.SizeBytes != int64(len(published)) ||
		!reflect.DeepEqual(observation.Containers, []string{"mov", "mp4"}) || observation.DurationMillis != 60_040 ||
		observation.VideoDurationMillis != 58_025 || observation.VideoCodec != "h264" || observation.AudioCodec != "aac" ||
		observation.Width != 1920 || observation.Height != 1080 || !observation.Decodable {
		t.Fatalf("QC observation is incomplete: %+v", observation)
	}
	argumentPayload, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-i", publishedPath}
	gotArguments := strings.Split(strings.TrimSuffix(string(argumentPayload), "\n"), "\n")
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("ffprobe QC arguments=%q want=%q", gotArguments, wantArguments)
	}
	t.Run("published MP4 family containers", func(t *testing.T) {
		encoder, err := exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("FFmpeg required")
		}
		probe, err := exec.LookPath("ffprobe")
		if err != nil {
			t.Skip("FFprobe required")
		}
		inspector, err := NewInspector(probe, "test", "worker", OSRunner{})
		if err != nil {
			t.Fatal(err)
		}
		for _, muxer := range []string{"mp4", "mov", "3gp", "3g2"} {
			file := filepath.Join(directory, muxer+"-rendition.mp4")
			cmd := exec.Command(encoder, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=s=32x32:r=1",
				"-frames:v", "1", "-c:v", "libx264", "-threads", "1", "-f", muxer, file)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v: %s", muxer, err, out)
			}
			observed, err := inspector.InspectPublishedOutput(context.Background(), file)
			if err != nil || !reflect.DeepEqual(observed.Containers, []string{muxer}) {
				t.Errorf("%s bytes under .mp4 name: containers=%v, err=%v", muxer, observed.Containers, err)
			}
		}
	})

}
