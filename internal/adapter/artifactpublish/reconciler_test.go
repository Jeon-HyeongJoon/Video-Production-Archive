package artifactpublish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

type countingProcessor struct{ calls int }

func (processor *countingProcessor) Run(context.Context, string) error {
	processor.calls++
	return nil
}

// Requirement: EXE-003
func TestRestartReconcilesPublishedArtifactWithoutRerunningProcessor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.db")
	publishedPath := filepath.Join(root, "archive", "proxy.mp4")
	if err := os.MkdirAll(filepath.Dir(publishedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified-published-video")
	if err := os.WriteFile(publishedPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	artifact := archive.Artifact{
		ID: "ART-1", Path: publishedPath, Size: int64(len(payload)), Checksum: checksum,
		Origin: archive.Provenance{OriginalAssetID: "ASSET-1", InputArtifactIDs: []string{}, ProcessingKind: vocabulary.ProcessingRender,
			ProcessorVersion: "ffmpeg-test", ProfileVersion: "proxy-v1", CreatedBy: "worker-1", CreatedAt: time.Now().UTC()},
	}

	beforeRestart, err := OpenRepository(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := beforeRestart.SaveInterruptedPublish(ctx, Receipt{StagingID: "STAGE-1", StepID: "STEP-1", PublishedPath: publishedPath, Checksum: checksum, Artifact: artifact}); err != nil {
		t.Fatal(err)
	}
	if err := beforeRestart.Close(); err != nil {
		t.Fatal(err)
	}

	afterRestart, err := OpenRepository(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer afterRestart.Close()
	processor := &countingProcessor{}
	if err := NewReconciler(afterRestart, processor).Reconcile(ctx, "STAGE-1"); err != nil {
		t.Fatal(err)
	}
	if processor.calls != 0 {
		t.Fatalf("processor reran %d times", processor.calls)
	}
	stored, found, err := afterRestart.Artifact(ctx, artifact.ID)
	if err != nil || !found || stored.Checksum != checksum {
		t.Fatalf("published file was not restored as artifact: artifact=%+v found=%v err=%v", stored, found, err)
	}
	state, err := afterRestart.StepState(ctx, "STEP-1")
	if err != nil || state != workflow.Verifying {
		t.Fatalf("step state=%s err=%v, want VERIFYING", state, err)
	}
}
