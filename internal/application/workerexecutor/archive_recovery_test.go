package workerexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"testing"
	"time"

	"videoarchive/internal/adapter/archivebundle"
	"videoarchive/internal/adapter/archivecatalog"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerclient"
	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
)

func testInterleavedArchiveRecovery(t *testing.T) {
	f := newArchiveRecoveryFixture(t, archive.PackageLayoutCompactResultFolder)
	root := f.roots.MustRoot("archive")
	reconciler, err := archivecatalog.NewReconciler(root)
	if err != nil {
		t.Fatal(err)
	}
	if catalog, err := reconciler.Rebuild(); err != nil || catalog.RowCount != 2 {
		t.Fatalf("initial catalog: %+v, %v", catalog, err)
	}
	// Each Registry has its own handles to the same physical destination.
	otherRoots, err := workerconfig.OpenStorageRoots(f.configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer otherRoots.Close()
	second, err := NewProduction(f.configuration, otherRoots)
	if err != nil {
		t.Fatal(err)
	}
	bSpec := f.recoveryWithNewText(t, f.other, 1, "other")
	newerA := f.recoveryWithNewText(t, f.stored, 2, "corrected")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	currentSHA := func(file string) string {
		t.Helper()
		_, sha, err := root.FileSHA256(file)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	checkPublication := func(result workerclient.ExecutionResult, spec execution.StepExecutionSpec, rev uint64) string {
		t.Helper()
		if !result.Success || len(result.Outputs) != 1 || len(result.VerificationReferences) == 0 {
			t.Fatalf("publication failed: %+v", result)
		}
		out := result.Outputs[0]
		current, err := f.publisher.Current(spec.Parameters.ArchivePackageID)
		want := execution.PublishedPackage{PackageID: spec.Parameters.ArchivePackageID, Revision: rev, ManifestDigest: current.ManifestDigest}
		p := out.CatalogPublication
		if err != nil || p == nil || p.ArchiveRunID != spec.Parameters.ArchiveRunID || len(p.Packages) != 1 ||
			p.Packages[0] != want || p.CatalogSHA256 != out.Checksum || currentSHA(out.RelativePath) != out.Checksum {
			t.Fatalf("receipt and destination disagree: output=%+v publication=%+v expected=%+v err=%v", out, p, want, err)
		}
		return out.Checksum
	}
	first := f.registry.ExecuteOperation(ctx, f.spec, 1)
	firstSHA := checkPublication(first, f.spec, 2)
	aMetadata := currentSHA(f.stored.MetadataPath)
	publishedA, err := f.publisher.Current(f.stored.PackageID)
	if err != nil {
		t.Fatal(err)
	}

	// B acquires the catalog output lock. Start A's duplicate assignment while
	// B holds it, so the replay must observe the CSV containing B's new revision.
	bLocked, aStarted := make(chan struct{}), make(chan struct{})
	bCtx := execution.WithProgressReporter(ctx, func(ctx context.Context, update execution.ProgressUpdate) error {
		if update.Status == execution.ProgressExecuting {
			close(bLocked)
			select {
			case <-aStarted:
			case <-ctx.Done():
			}
		}
		return nil
	})
	bDone := make(chan workerclient.ExecutionResult, 1)
	go func() { bDone <- second.ExecuteOperation(bCtx, bSpec, 1) }()
	select {
	case <-bLocked:
	case result := <-bDone:
		t.Fatalf("B failed before execution: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	aCtx := execution.WithProgressReporter(ctx, func(_ context.Context, update execution.ProgressUpdate) error {
		if update.Status == execution.ProgressCheckingInputs {
			close(aStarted)
		}
		return nil
	})
	duplicate := f.registry.ExecuteOperation(aCtx, f.spec, 2)
	bResult := <-bDone
	bSHA := checkPublication(bResult, bSpec, 2)
	if checkPublication(duplicate, f.spec, 2) != bSHA || bSHA == firstSHA || currentSHA(f.stored.MetadataPath) != aMetadata {
		t.Fatal("interleaved replay changed A or returned an obsolete CSV")
	}
	bMetadata := currentSHA(f.other.MetadataPath)
	third := f.registry.ExecuteOperation(ctx, newerA, 1)
	thirdSHA := checkPublication(third, newerA, 3)
	aMetadata = currentSHA(f.stored.MetadataPath)
	late := second.ExecuteOperation(ctx, f.spec, 3)
	after, err := f.publisher.Current(f.stored.PackageID)
	if late.Success || late.CauseCode != "ARCHIVE_PACKAGE_STATE_INVALID" || len(late.Outputs)+len(late.VerificationReferences) != 0 ||
		err != nil || after.Revision != 3 || currentSHA(f.stored.MetadataPath) != aMetadata || currentSHA("archive-catalog.csv") != thirdSHA {
		t.Fatalf("old STT recovery after revision 3: success=%t cause=%s outputs=%d actual_revision=%d catalog_preserved=%t err=%v",
			late.Success, late.CauseCode, len(late.Outputs), after.Revision, currentSHA("archive-catalog.csv") == thirdSHA, err)
	}
	// A normal revision may replace the public document as soon as the
	// previous package writer releases its lock, before its output is reported.
	if hash, err := archiveRevisionChecksum(root, publishedA); !errors.Is(err, archivebundle.ErrPackageState) || hash != "" {
		t.Fatalf("revision 2 returned a checksum for revision 3: %s, %v", hash, err)
	}
	if currentSHA(f.other.MetadataPath) != bMetadata {
		t.Fatal("A's recovery changed B's published metadata")
	}
	data, err := root.ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("catalog rows: %v, %v", rows, err)
	}
	byID := map[string][]string{rows[1][17]: rows[1], rows[2][17]: rows[2]}
	for _, spec := range []execution.StepExecutionSpec{newerA, bSpec} {
		p, err := f.publisher.Current(spec.Parameters.ArchivePackageID)
		row := byID[p.PackageID]
		source := spec.Parameters.SourceInputs[0]
		_, sourceSHA, sourceErr := f.roots.MustRoot("source").FileSHA256(source.RelativePath)
		if err != nil || sourceErr != nil || sourceSHA != source.Checksum || currentSHA(p.VideoPath) != sourceSHA ||
			len(row) != 18 || row[4] != p.VideoPath || row[5] != strconv.FormatUint(spec.Parameters.ArchivePackageRevision+1, 10) ||
			row[6] != p.ManifestDigest || currentSHA(row[12]) != spec.Inputs[1].Checksum || currentSHA(row[13]) != spec.Inputs[2].Checksum {
			t.Fatalf("published files, sources or row differ for %s: %v, %v, %v", p.PackageID, row, err, sourceErr)
		}
		data, err := root.ReadFile(p.MetadataPath)
		var doc archivebundle.CompactDocument
		if err != nil || json.Unmarshal(data, &doc) != nil || uint64(len(doc.Revisions)) != p.Revision {
			t.Fatalf("invalid recovery history for %s", p.PackageID)
		}
		var embedded bytes.Buffer
		if json.Compact(&embedded, doc.Revisions[p.Revision-1].Transcript) != nil ||
			fmt.Sprintf("%x", sha256.Sum256(embedded.Bytes())) != spec.Inputs[0].Checksum {
			t.Fatalf("wrong embedded transcript for %s revision %d", p.PackageID, p.Revision)
		}
	}
	if report, err := reconciler.Verify(); err != nil || !report.Current() || len(report.Entries) != 2 {
		t.Fatalf("final independent verification: %+v, %v", report, err)
	}
}

func (f archiveRecoveryFixture) recoveryWithNewText(t *testing.T, pkg archivebundle.Package, base uint64, text string) execution.StepExecutionSpec {
	t.Helper()
	spec := f.spec
	spec.JobID += "-" + text
	spec.Parameters.ArchiveRunID += "-" + text
	spec.Parameters.ArchivePackageID, spec.Parameters.ArchivePackageRevision = pkg.PackageID, base
	source := spec.Parameters.SourceInputs[0]
	if pkg.PackageID == f.other.PackageID {
		source.OriginalAssetID, source.OriginalName, source.RelativePath = "ASSET-2", "other.mp4", "other.mp4"
		_, sum, err := f.roots.MustRoot("source").FileSHA256(source.RelativePath)
		if err != nil {
			t.Fatal(err)
		}
		source.Checksum = sum
	}
	spec.Parameters.SourceInputs = []execution.InputReference{source}
	root := f.roots.MustRoot("archive")
	spec.Inputs = append([]execution.InputReference(nil), spec.Inputs...)
	payloads := []string{fmt.Sprintf(`{"schema_version":1,"text":%q}`, text), "[00:00:00.000 --> 00:00:01.000] " + text + "\n", "1\n00:00:00,000 --> 00:00:01,000\n" + text + "\n"}
	for i := range spec.Inputs {
		input := &spec.Inputs[i]
		input.OriginalAssetID, input.ArtifactID = source.OriginalAssetID, input.ArtifactID+"-"+text
		input.RelativePath = path.Join("staging", text+"-"+path.Base(input.RelativePath))
		if err := root.WriteFile(input.RelativePath, []byte(payloads[i]), 0o600); err != nil {
			t.Fatal(err)
		}
		_, hash, err := root.FileSHA256(input.RelativePath)
		if err != nil {
			t.Fatal(err)
		}
		input.Checksum = hash
	}
	spec, err := execution.NewStepExecutionSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
