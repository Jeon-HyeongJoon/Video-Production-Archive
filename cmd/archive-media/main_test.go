package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: APP-009
func TestStandaloneMediaCLIExecutesTypedOperationWithoutControlOrWorkerRuntime(t *testing.T) {
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "source")
	archiveRoot := filepath.Join(directory, "archive")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archiveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("standalone-source")
	if err := os.WriteFile(filepath.Join(sourceRoot, "recordings", "event.mov"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configurationPath := filepath.Join(directory, "media.yaml")
	configuration := fmt.Sprintf(`schema_version: 1
media:
  id: standalone-cli
  storage_roots:
    - storage_id: source
      root: %s
    - storage_id: archive
      root: %s
  tools:
    ffmpeg: {executable: %s, version: fixture-v1}
    ffprobe: {executable: %s, version: fixture-v1}
`, strconv.Quote(sourceRoot), strconv.Quote(archiveRoot), strconv.Quote(executable), strconv.Quote(executable))
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	input := execution.InputReference{
		OriginalAssetID: "ASSET-CLI-1", OriginalName: "event.mov", StorageID: "source",
		RelativePath: "recordings/event.mov", Checksum: checksum, DurationMillis: 1_000,
	}
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{
		JobID: "CLI-JOB-1", StepID: "package", TemplateName: "standalone-package", Operation: execution.OperationOrganizeMedia,
		Inputs: []execution.InputReference{input},
		Outputs: []execution.OutputTarget{{
			ID: "VP-20261106-CLI", Kind: execution.OutputOrganizedMedia, StorageID: "archive",
			RelativePath: "2026-11-06_event__CLI/metadata.json", OriginalAssetID: input.OriginalAssetID,
		}},
		Parameters: execution.StepExecutionParameters{
			OrganizationDecisionID: "DECISION-CLI-1", WorkspaceDriveID: "VOLUME-CLI-1", ArchiveStorageID: "archive",
			MediaFolder: "recordings", InputRevision: 1, Disposition: vocabulary.OrganizationAsIs,
			SelectedProfileReference: "archive-1080p-6mbps@v1", SourceInputs: []execution.InputReference{input},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec.Digest = ""
	specificationPath := filepath.Join(directory, "package.json")
	specification, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("write standalone operation spec: %v", err)
	}
	if err := os.WriteFile(specificationPath, specification, 0o600); err != nil {
		t.Fatalf("write standalone operation spec: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"package", "--config", configurationPath, "--spec", specificationPath, "--execution-token", "41",
	}, bytes.NewReader(nil), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("standalone CLI failed with %d: stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Success || result.Operation != execution.OperationOrganizeMedia ||
		len(result.Outputs) != 1 || result.Outputs[0].RelativePath != spec.Outputs[0].RelativePath || result.Outputs[0].Checksum == "" {
		t.Fatalf("standalone CLI result is incomplete: result=%+v err=%v", result, err)
	}
	stored, err := os.ReadFile(filepath.Join(archiveRoot, filepath.FromSlash(spec.Outputs[0].RelativePath)))
	if err != nil || !bytes.Contains(stored, []byte(`"original_name": "event.mov"`)) {
		t.Fatalf("standalone CLI did not publish the package: %s err=%v", stored, err)
	}
	unchanged, err := os.ReadFile(filepath.Join(sourceRoot, "recordings", "event.mov"))
	if err != nil || !bytes.Equal(unchanged, payload) {
		t.Fatalf("standalone CLI changed the source: %q err=%v", unchanged, err)
	}
}
