//go:build destination_catalog_contract

package workerexecutor

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ARC-021
func TestDestinationRevisionRefusesWrongLayoutBeforePublishing(t *testing.T) {
	tool := workerconfig.LocalTool{Executable: os.Args[0], Version: "unused"}
	conf := workerconfig.Configuration{ID: "contract-worker", AccessibleStorageIDs: []string{"archive"},
		StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "archive", Root: t.TempDir()}},
		Tools:        workerconfig.MediaTools{FFmpeg: tool, FFprobe: tool}}
	roots, err := workerconfig.OpenStorageRoots(conf)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	root := roots.MustRoot("archive")
	write := func(name, body string) string {
		t.Helper()
		if err := root.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, sum, err := root.FileSHA256(name)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	checksum := write("input.mp4", "input")
	pub, err := archivebundle.NewPublisher(root, root)
	if err != nil {
		t.Fatal(err)
	}
	const id = "VP-20261124-LEGACY"
	stored, err := pub.PublishInitialV2(archivebundle.InitialRequest{
		LayoutVersion: archive.PackageLayoutFlatResultFolder, PackageID: id, DecisionID: "DECISION", WorkspaceDriveID: "VOLUME", MediaFolder: "recording",
		Disposition: vocabulary.OrganizationAsIs, ProfileReference: "archive-1080p-6mbps@v1", CreatedAt: time.Now().UTC(),
		Sources: []archivebundle.SourceReference{{OriginalAssetID: "ASSET", OriginalName: "input.mp4", RelativePath: "input.mp4", Checksum: checksum}},
		Video:   archivebundle.FileSource{Root: archivebundle.RootSource, Path: "input.mp4", Checksum: checksum},
	})
	if err != nil {
		t.Fatal(err)
	}
	var inputs []execution.InputReference
	for _, file := range []struct {
		kind       execution.OutputKind
		name, body string
	}{
		{execution.OutputTranscript, "transcript.json", `{"segments":[]}`},
		{execution.OutputTranscriptText, "transcript.txt", "text"},
		{execution.OutputSubtitle, "subtitle.srt", "1\n00:00:00,000 --> 00:00:01,000\ntext\n"},
	} {
		inputs = append(inputs, execution.InputReference{OriginalAssetID: "ASSET", ArtifactID: string(file.kind), Kind: file.kind, StorageID: "archive", RelativePath: file.name, Checksum: write(file.name, file.body)})
	}
	wrong := path.Join(path.Dir(stored.MetadataPath), ".archive-package.json")
	spec, err := execution.NewStepExecutionSpec(execution.StepExecutionSpec{JobID: "STT-JOB", StepID: "archive_package_revision", TemplateName: "archive-transcription", Operation: execution.OperationReviseArchivePackage,
		Inputs: inputs, Outputs: []execution.OutputTarget{{ID: id, Kind: execution.OutputArchiveRevision, StorageID: "archive", RelativePath: wrong}},
		Parameters: execution.StepExecutionParameters{OrganizationDecisionID: "DECISION", WorkspaceDriveID: "VOLUME", ArchiveStorageID: "archive", InputRevision: 1, ArchivePackageID: id, ArchivePackageRevision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := NewProduction(conf, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := reg.Execute(context.Background(), workerclient.Assignment{JobID: spec.JobID, StepID: spec.StepID, FencingToken: 1, LeaseExpiresAt: time.Now().Add(time.Minute), ExecutionSpec: spec})
	current, err := pub.Current(id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.CauseCode != "PACKAGE_LAYOUT_PATH_MISMATCH" || current.Revision != stored.Revision || current.ManifestDigest != stored.ManifestDigest || current.SubtitlePath != "" {
		t.Fatalf("wrong-path settlement success=%v cause=%s revision=%d (was %d) subtitle=%q", result.Success, result.CauseCode, current.Revision, stored.Revision, current.SubtitlePath)
	}
}
