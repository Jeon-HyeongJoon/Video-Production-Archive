package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
)

// Start with a real published compact package and catalog row. Drive the same
// downstream CLI operations a successful transcription supplies inputs for.
func prepareSTTPackage(t *testing.T, root, source string, audio execution.InputReference, target execution.OutputTarget) func(commandResult) {
	t.Helper()
	const folder = "2026-11-25_recording__0001/"
	const metadata = folder + ".archive-package.json"
	const catalog = "archive-catalog.csv"
	const packageID = "VP-20261125-0001"
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	parameters := execution.StepExecutionParameters{
		OrganizationDecisionID: "D1", WorkspaceDriveID: "V1", InputRevision: 1,
		ArchiveStorageID: "media", MediaFolder: "recordings", Disposition: vocabulary.OrganizationAsIs, SelectedProfileReference: "source@v1",
	}
	targetFor := func(id string, kind execution.OutputKind, name string) execution.OutputTarget {
		return execution.OutputTarget{ID: id, Kind: kind, StorageID: "media", RelativePath: name, OriginalAssetID: audio.OriginalAssetID}
	}
	runStep := func(command string, op execution.Operation, profile string, inputs []execution.InputReference, output execution.OutputTarget) execution.InputReference {
		t.Helper()
		spec := execution.StepExecutionSpec{
			JobID: "JOB-STT", StepID: command, TemplateName: "archive-transcription", Operation: op,
			Parameters: parameters, ProfileReference: profile, Inputs: inputs, Outputs: []execution.OutputTarget{output},
		}
		data, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{command, "--config", filepath.Join(root, "media.yaml"), "--execution-token", "1"}, bytes.NewReader(data), &stdout, &stderr)
		var result commandResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != 0 || !result.Success || len(result.Outputs) != 1 {
			t.Fatalf("%s: %s %s (%v)", command, stdout.String(), stderr.String(), err)
		}
		return execution.InputReference{
			OriginalAssetID: audio.OriginalAssetID, ArtifactID: output.ID, Kind: output.Kind,
			StorageID: output.StorageID, RelativePath: output.RelativePath, Checksum: result.Outputs[0].Checksum,
		}
	}
	sourceBytes := []byte("recording payload")
	if err := os.WriteFile(filepath.Join(source, "recording.mov"), sourceBytes, 0600); err != nil {
		t.Fatal(err)
	}
	original := execution.InputReference{
		OriginalAssetID: audio.OriginalAssetID, OriginalName: "recording.mov", StorageID: "source", RelativePath: "recording.mov",
		Checksum: fmt.Sprintf("%x", sha256.Sum256(sourceBytes)), DurationMillis: 1000, SourceSizeBytes: int64(len(sourceBytes)), SourceContainer: "mov", ProfileReference: "source@v1",
	}
	parameters.SourceInputs = []execution.InputReference{original}
	pkg := runStep("package", execution.OperationOrganizeMedia, "", parameters.SourceInputs, targetFor(packageID, execution.OutputOrganizedMedia, metadata))
	parameters.SourceInputs = nil
	catalogTarget := targetFor("CATALOG", execution.OutputArchiveCatalog, catalog)
	runStep("catalog", execution.OperationWriteArchiveCatalog, "", []execution.InputReference{pkg}, catalogTarget)
	previousDoc, previousCSV := read(metadata), read(catalog)
	expectRevision := func(rev string) {
		t.Helper()
		rows, err := csv.NewReader(bytes.NewReader(read(catalog))).ReadAll()
		if err != nil || len(rows) != 2 {
			t.Fatalf("catalog rows = %v: %v", rows, err)
		}
		for i, k := range rows[0] {
			if k == "package_revision" && rows[1][i] == rev {
				return
			}
		}
		t.Fatalf("expected one row at revision %s: %v", rev, rows)
	}
	expectRevision("1")
	return func(result commandResult) {
		t.Helper()
		if !result.Success {
			if !bytes.Equal(previousDoc, read(metadata)) || !bytes.Equal(previousCSV, read(catalog)) {
				t.Fatal("failed transcription changed the published revision")
			}
			for _, name := range []string{target.RelativePath, folder + "subtitle.srt", folder + "transcript.txt"} {
				if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed transcription left %s: %v", name, err)
				}
			}
		} else {
			if len(result.Outputs) != 1 {
				t.Fatal(result)
			}
			transcript := execution.InputReference{
				OriginalAssetID: audio.OriginalAssetID, ArtifactID: target.ID, Kind: target.Kind,
				StorageID: target.StorageID, RelativePath: target.RelativePath, Checksum: result.Outputs[0].Checksum,
			}
			srt := runStep("subtitle", execution.OperationWriteSubtitle, "event-srt@v1", []execution.InputReference{transcript},
				targetFor("SUB", execution.OutputSubtitle, "out/event.srt"))
			txt := runStep("transcript-text", execution.OperationWriteTranscriptText, "plain-text-timecode@v1", []execution.InputReference{transcript},
				targetFor("TXT", execution.OutputTranscriptText, "out/event.txt"))
			parameters.ArchivePackageID, parameters.ArchivePackageRevision = packageID, 1
			revision := runStep("package-revise", execution.OperationReviseArchivePackage, "", []execution.InputReference{transcript, srt, txt},
				targetFor(packageID, execution.OutputArchiveRevision, metadata))
			runStep("catalog", execution.OperationWriteArchiveCatalog, "", []execution.InputReference{revision}, catalogTarget)
			expectRevision("2")
			if got := string(read(folder + "subtitle.srt")); got != "1\n00:00:00,000 --> 00:00:01,000\n행사 녹화\n\n" {
				t.Fatalf("published SRT %q", got)
			}
			if got := string(read(folder + "transcript.txt")); got != "[00:00:00.000 --> 00:00:01.000] 행사 녹화\n" {
				t.Fatalf("published TXT %q", got)
			}
			var pkg struct {
				Revisions []struct {
					Transcript json.RawMessage `json:"transcript"`
				} `json:"revisions"`
			}
			if err := json.Unmarshal(read(metadata), &pkg); err != nil || len(pkg.Revisions) != 2 {
				t.Fatalf("stored revisions: %v (%v)", pkg, err)
			}
			var embedded bytes.Buffer
			if err := json.Compact(&embedded, pkg.Revisions[1].Transcript); err != nil || !bytes.Equal(embedded.Bytes(), read(target.RelativePath)) {
				t.Fatalf("stored Transcript differs from verified JSON: %v", err)
			}
		}
		if fmt.Sprintf("%x", sha256.Sum256(read(audio.RelativePath))) != audio.Checksum {
			t.Fatal("input audio changed")
		}
		stored, err := os.ReadFile(filepath.Join(source, "recording.mov"))
		if err != nil || !bytes.Equal(sourceBytes, stored) {
			t.Fatal("source recording changed", err)
		}
	}
}
