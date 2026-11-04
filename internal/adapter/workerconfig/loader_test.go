package workerconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/core/device"
)

// Requirement: CFG-007
func TestWorkerConfigurationRequiresStrictSecureOutboundSettings(t *testing.T) {
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	scratchRoot := filepath.Join(directory, "scratch")
	for _, root := range []string{sourceRoot, scratchRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ffmpegPath, ffprobePath := writeToolFixtures(t, directory)
	valid := `schema_version: 1
worker:
  id: worker-main
  control_url: https://control.internal:8443
  credential_ref: wincred://archive/workers/worker-main
  heartbeat_interval: 10s
  storage_roots:
    - storage_id: source-main
      root: ` + sourceRoot + `
    - storage_id: scratch-main
      root: ` + scratchRoot + `
  tools:
    ffmpeg:
      executable: ` + ffmpegPath + `
      version: ffmpeg-fixture-v1
    ffprobe:
      executable: ` + ffprobePath + `
      version: ffprobe-fixture-v1
`
	path := filepath.Join(directory, "worker.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ID != "worker-main" || configuration.ControlURL.String() != "https://control.internal:8443" ||
		configuration.CredentialReference != "wincred://archive/workers/worker-main" ||
		configuration.HeartbeatInterval != 10*time.Second || len(configuration.AccessibleStorageIDs) != 2 {
		t.Fatalf("worker configuration was not normalized: %+v", configuration)
	}
	dpapi := stringsReplace(valid, "wincred://archive/workers/worker-main", "dpapi://C:/Archive/secrets/worker-main.dpapi")
	dpapiPath := filepath.Join(directory, "worker-dpapi.yaml")
	if err := os.WriteFile(dpapiPath, []byte(dpapi), 0o600); err != nil {
		t.Fatal(err)
	}
	dpapiConfiguration, err := Load(dpapiPath)
	if err != nil || dpapiConfiguration.CredentialReference != "dpapi://C:/Archive/secrets/worker-main.dpapi" {
		t.Fatalf("DPAPI Worker configuration was not accepted: %+v err=%v", dpapiConfiguration, err)
	}
	for index, payload := range []string{
		valid + "unknown: true\n",
		stringsReplace(valid, "https://control.internal:8443", "http://192.168.40.8:8080"),
		stringsReplace(valid, "wincred://archive/workers/worker-main", "dpapi://relative/worker-main.dpapi"),
		stringsReplace(valid, "wincred://archive/workers/worker-main", "plaintext-secret"),
	} {
		candidate := filepath.Join(directory, fmtName(index)+".yaml")
		if err := os.WriteFile(candidate, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(candidate); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid worker configuration %d was accepted: %v", index, err)
		}
	}
}

// Requirement: CFG-008
func TestWorkerConfigurationOpensOnlyMappedAbsoluteStorageRootsBeforeExecution(t *testing.T) {
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	workingRoot := filepath.Join(directory, "working")
	for _, root := range []string{sourceRoot, workingRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "clip.mp4"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	ffmpegPath, ffprobePath := writeToolFixtures(t, directory)
	valid := `schema_version: 1
worker:
  id: worker-main
  control_url: https://control.internal:8443
  credential_ref: wincred://archive/workers/worker-main
  heartbeat_interval: 10s
  storage_roots:
    - storage_id: source-main
      root: ` + sourceRoot + `
    - storage_id: working-main
      root: ` + workingRoot + `
  tools:
    ffmpeg:
      executable: ` + ffmpegPath + `
      version: ffmpeg-fixture-v1
    ffprobe:
      executable: ` + ffprobePath + `
      version: ffprobe-fixture-v1
`
	path := filepath.Join(directory, "worker-roots.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(configuration.AccessibleStorageIDs, ",") != "source-main,working-main" {
		t.Fatalf("heartbeat Storage IDs were not derived from roots: %v", configuration.AccessibleStorageIDs)
	}
	roots, err := OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	source, ok := roots.Root("source-main")
	if !ok {
		t.Fatal("source StorageRoot was not opened")
	}
	content, err := source.ReadFile("clip.mp4")
	if err != nil || string(content) != "fixture" {
		t.Fatalf("mapped StorageRoot did not resolve the safe relative file: %q err=%v", content, err)
	}
	if _, ok := roots.Root(sourceRoot); ok {
		t.Fatal("absolute root was exposed as a Storage ID")
	}

	invalidDocuments := []string{
		stringsReplace(valid, "storage_id: working-main", "storage_id: source-main"),
		stringsReplace(valid, workingRoot, "relative/working"),
		stringsReplace(valid, workingRoot, filepath.Join(directory, "missing")),
	}
	for index, document := range invalidDocuments {
		candidate := filepath.Join(directory, fmt.Sprintf("invalid-root-%d.yaml", index))
		if err := os.WriteFile(candidate, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, loadErr := Load(candidate)
		if loadErr == nil {
			opened, openErr := OpenStorageRoots(loaded)
			if opened != nil {
				opened.Close()
			}
			if openErr == nil {
				t.Fatalf("unsafe Storage root configuration %d was accepted", index)
			}
		}
	}
}

// Requirement: DEV-013
func TestWorkerReconcilesRoleSeparatedAttachedVolumeStorageRoots(t *testing.T) {
	directory := t.TempDir()
	staticPath := filepath.Join(directory, "static")
	attachedPath := filepath.Join(directory, "attached")
	for _, root := range []string{staticPath, attachedPath} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(attachedPath, "clip.mp4"), []byte("attached-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := OpenStorageRoots(Configuration{
		StorageRoots:         []StorageRootConfiguration{{StorageID: "static-archive", Root: staticPath}},
		AccessibleStorageIDs: []string{"static-archive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	volumeID := "VOLUME-ATTACHED-1"
	archiveStorageID := device.AttachedArchiveStorageID(volumeID)
	if err := roots.SyncAttachedVolumes([]device.ConnectedVolume{{
		PhysicalDeviceID: "PHYSICAL-1", VolumeID: volumeID,
		StorageID: "source-attached-1", ArchiveStorageID: archiveStorageID, Root: attachedPath,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, storageID := range []string{"source-attached-1", archiveStorageID} {
		root, found := roots.Root(storageID)
		if !found {
			t.Fatalf("role-specific attached StorageRoot %s was not opened", storageID)
		}
		payload, readErr := root.ReadFile("clip.mp4")
		if readErr != nil || string(payload) != "attached-fixture" {
			t.Fatalf("attached StorageRoot %s did not stay within the observed root: %q err=%v", storageID, payload, readErr)
		}
	}
	if err := roots.SyncAttachedVolumes(nil); err != nil {
		t.Fatal(err)
	}
	if _, found := roots.Root("source-attached-1"); found {
		t.Fatal("detached SOURCE_MEDIA capability remained open")
	}
	if _, found := roots.Root(archiveStorageID); found {
		t.Fatal("detached ARCHIVE_PRIMARY capability remained open")
	}
	if _, found := roots.Root("static-archive"); !found {
		t.Fatal("runtime reconciliation removed a boot-configured StorageRoot")
	}
}

// Requirement: CFG-009
func TestWorkerConfigurationValidatesLocalMediaToolchainBeforeExecution(t *testing.T) {
	directory := t.TempDir()
	storageRoot := filepath.Join(directory, "storage")
	if err := os.Mkdir(storageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpegPath := filepath.Join(directory, "ffmpeg")
	ffprobePath := filepath.Join(directory, "ffprobe")
	for _, executable := range []string{ffmpegPath, ffprobePath} {
		if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	valid := `schema_version: 1
worker:
  id: worker-main
  control_url: https://control.internal:8443
  credential_ref: wincred://archive/workers/worker-main
  heartbeat_interval: 10s
  storage_roots:
    - storage_id: working-main
      root: ` + storageRoot + `
  tools:
    ffmpeg:
      executable: ` + ffmpegPath + `
      version: ffmpeg-fixture-v1
    ffprobe:
      executable: ` + ffprobePath + `
      version: ffprobe-fixture-v1
`
	path := filepath.Join(directory, "worker-tools.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.FFmpeg.Executable != ffmpegPath || configuration.Tools.FFmpeg.Version != "ffmpeg-fixture-v1" ||
		configuration.Tools.FFprobe.Executable != ffprobePath || configuration.Tools.FFprobe.Version != "ffprobe-fixture-v1" {
		t.Fatalf("media toolchain was not preserved: %+v", configuration.Tools)
	}
	for index, payload := range []string{
		stringsReplace(valid, ffmpegPath, "relative/ffmpeg"),
		stringsReplace(valid, ffprobePath, filepath.Join(directory, "missing-ffprobe")),
		stringsReplace(valid, "ffmpeg-fixture-v1", ""),
		stringsReplace(valid, "version: ffprobe-fixture-v1", "version: ffprobe-fixture-v1\n      unknown: true"),
	} {
		candidate := filepath.Join(directory, fmt.Sprintf("invalid-tool-%d.yaml", index))
		if err := os.WriteFile(candidate, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(candidate); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid media toolchain %d was accepted: %v", index, err)
		}
	}
}

// Requirement: CFG-010
func TestWorkerConfigurationValidatesOptionalWhisperToolModelAndProfile(t *testing.T) {
	directory := t.TempDir()
	storageRoot := filepath.Join(directory, "storage")
	if err := os.MkdirAll(filepath.Join(storageRoot, "models"), 0o700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(storageRoot, "models", "ggml.bin")
	if err := os.WriteFile(modelPath, []byte("model-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	ffmpegPath, ffprobePath := writeToolFixtures(t, directory)
	whisperPath := filepath.Join(directory, "whisper-cli")
	if err := os.WriteFile(whisperPath, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `schema_version: 1
worker:
  id: worker-stt
  control_url: https://control.internal:8443
  credential_ref: wincred://archive/workers/worker-stt
  heartbeat_interval: 10s
  storage_roots:
    - storage_id: models-main
      root: ` + storageRoot + `
  tools:
    ffmpeg:
      executable: ` + ffmpegPath + `
      version: ffmpeg-fixture-v1
    ffprobe:
      executable: ` + ffprobePath + `
      version: ffprobe-fixture-v1
    whispercpp:
      executable: ` + whisperPath + `
      version: whisper-fixture-v1
      model_storage_id: models-main
      model_relative_path: models/ggml.bin
      profile:
        id: event-ko
        version: 1
        model_id: ggml-fixture
        language: ko
        thread_count: 4
`
	path := filepath.Join(directory, "worker-whisper.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path)
	if err != nil || configuration.Tools.WhisperCPP == nil {
		t.Fatalf("valid whisper toolchain was rejected: %+v err=%v", configuration.Tools.WhisperCPP, err)
	}
	roots, err := OpenStorageRoots(configuration)
	if err != nil {
		t.Fatal(err)
	}
	roots.Close()
	tool := configuration.Tools.WhisperCPP
	if tool.Executable != whisperPath || tool.ModelStorageID != "models-main" || tool.ModelRelativePath != "models/ggml.bin" ||
		tool.Profile.Reference() != "event-ko@v1" {
		t.Fatalf("whisper toolchain was not normalized: %+v", tool)
	}

	withoutWhisper := valid[:strings.Index(valid, "    whispercpp:")]
	absentPath := filepath.Join(directory, "worker-without-whisper.yaml")
	if err := os.WriteFile(absentPath, []byte(withoutWhisper), 0o600); err != nil {
		t.Fatal(err)
	}
	absent, err := Load(absentPath)
	if err != nil || absent.Tools.WhisperCPP != nil {
		t.Fatalf("optional whisper block was not optional: %+v err=%v", absent.Tools.WhisperCPP, err)
	}

	invalid := []string{
		stringsReplace(valid, "model_storage_id: models-main", "model_storage_id: unknown"),
		stringsReplace(valid, "model_relative_path: models/ggml.bin", "model_relative_path: ../ggml.bin"),
		stringsReplace(valid, "model_relative_path: models/ggml.bin", "model_relative_path: models/missing.bin"),
		stringsReplace(valid, "language: ko", "language: korean"),
		stringsReplace(valid, "thread_count: 4", "thread_count: 0"),
		stringsReplace(valid, "version: whisper-fixture-v1", "version: ''"),
		stringsReplace(valid, "thread_count: 4", "thread_count: 4\n        unknown: true"),
	}
	for index, payload := range invalid {
		candidate := filepath.Join(directory, fmt.Sprintf("invalid-whisper-%d.yaml", index))
		if err := os.WriteFile(candidate, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, loadErr := Load(candidate)
		if loadErr == nil {
			opened, openErr := OpenStorageRoots(loaded)
			if opened != nil {
				opened.Close()
			}
			if openErr == nil {
				t.Fatalf("invalid whisper toolchain %d was accepted", index)
			}
		}
	}
}

func stringsReplace(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}

func fmtName(index int) string {
	return fmt.Sprintf("invalid-%d", index)
}

func writeToolFixtures(t *testing.T, directory string) (string, string) {
	t.Helper()
	ffmpegPath := filepath.Join(directory, "ffmpeg-fixture")
	ffprobePath := filepath.Join(directory, "ffprobe-fixture")
	for _, executable := range []string{ffmpegPath, ffprobePath} {
		if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ffmpegPath, ffprobePath
}
