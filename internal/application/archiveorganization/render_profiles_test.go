package archiveorganization

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videoarchive/internal/adapter/ffprobe"
	"videoarchive/internal/adapter/workerconfig"
	"videoarchive/internal/application/workerexecutor"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: RND-007
func TestWorkerPublishesMixedRoleProfilesFromOneFolder(t *testing.T) {
	encoder, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg required")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("FFprobe required")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	const folder = "370105-행사_촬영-본당"
	src, dest := filepath.Join(tmp, "source"), filepath.Join(tmp, "destination")
	for _, dir := range []string{filepath.Join(src, folder), dest} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := workerconfig.Configuration{ID: "worker", StorageRoots: []workerconfig.StorageRootConfiguration{{StorageID: "src", Root: src}, {StorageID: "dest", Root: dest}}, AccessibleStorageIDs: []string{"src", "dest"}, Tools: workerconfig.MediaTools{FFmpeg: workerconfig.LocalTool{Executable: encoder, Version: "test"}, FFprobe: workerconfig.LocalTool{Executable: probe, Version: "test"}}}
	roots, err := workerconfig.OpenStorageRoots(config)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	inspector, err := ffprobe.NewInspector(probe, "test", "worker", ffprobe.OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	policy := organization.DefaultPolicy()
	policy.Version = organization.DrivePolicyVersion
	req := organization.AutoRequest{Folder: folder, WorkspaceDriveID: "vol", InputRevision: 3, Policy: policy, DecisionID: "D", DecidedAt: time.Now().UTC(), EventContext: organization.EventContext{InputRevision: 3, EventFolder: folder}}
	want := map[string]string{"cut": "archive-1080p-20mbps@v1", "rec": "archive-1080p-6mbps@v1"}
	for _, file := range []struct{ id, name string }{{"rec", "행사 녹화.mp4"}, {"cut", "C0001.MP4"}} {
		rel := path.Join(folder, file.name)
		full := filepath.Join(src, filepath.FromSlash(rel))
		out, err := exec.Command(encoder, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=320x180:r=24", "-t", "1", "-c:v", "libx264", "-threads", "1", "-an", full).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		_, sum, err := roots.MustRoot("src").FileSHA256(rel)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := inspector.Inspect(ctx, ffprobe.Request{InspectionID: "inspect-" + file.id, OriginalAssetID: file.id, OriginalChecksum: sum, ResolvedInputPath: full})
		if err != nil {
			t.Fatal(err)
		}
		req.Inputs = append(req.Inputs, organization.Input{OriginalAssetID: file.id, RelativePath: rel, Inspection: inspection})
	}
	d, err := organization.Classify(req)
	if err != nil || d.Disposition != vocabulary.OrganizationRenderRecordings || d.ProfileReference != "" {
		t.Fatal(d, err)
	}
	const jobID = "JOB-MIXED"
	const template = "archive-intake-render-recordings"
	spec, err := firstWorkerSpec("RUN", jobID, template, "src", "dest", d)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := workerexecutor.NewProduction(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	packages := make(map[string]string)
	var completed []execution.CompletedStep
	for _, step := range []string{"render", "qc", "organize", "archive_catalog"} {
		if step != "render" {
			spec, err = execution.MaterializeDependentStepExecutionSpec(execution.MaterializeRequest{JobID: jobID, TemplateName: template, StepID: step, Completed: completed})
			if err != nil {
				t.Fatal(step, err)
			}
		}
		result := registry.ExecuteDirect(ctx, spec, 1)
		if !result.Success {
			t.Fatalf("%s: %+v", step, result)
		}
		if step == "render" || step == "qc" {
			for _, in := range spec.Inputs {
				if in.ProfileReference != want[in.OriginalAssetID] {
					t.Fatalf("%s %s profile %q", step, in.OriginalAssetID, in.ProfileReference)
				}
			}
		}
		if step == "qc" || step == "organize" {
			for i, target := range spec.Outputs {
				id := target.OriginalAssetID
				if step == "qc" {
					id = spec.Inputs[i].OriginalAssetID
				} else {
					packages[id] = target.ID
				}
				raw, err := roots.MustRoot("dest").ReadFile(target.RelativePath)
				var proof struct {
					Profile string `json:"profile_reference"`
				}
				if err != nil || json.Unmarshal(raw, &proof) != nil || proof.Profile != want[id] {
					t.Fatalf("%s %s profile %q, expected %q (%v)", step, id, proof.Profile, want[id], err)
				}
			}
		}
		if step == "archive_catalog" {
			break
		}
		record, err := execution.NewStepOutputRecord(spec, 1, config.ID, time.Now().UTC(), result.Outputs, result.VerificationReferences)
		if err != nil {
			t.Fatal(step, err)
		}
		completed = append(completed, execution.CompletedStep{StepID: step, Spec: spec, OutputRecord: record})
	}
	for _, in := range req.Inputs {
		_, sum, err := roots.MustRoot("src").FileSHA256(in.RelativePath)
		if err != nil || sum != in.Inspection.OriginalChecksum {
			t.Fatal("source changed", in.RelativePath, err)
		}
	}
	raw, err := roots.MustRoot("dest").ReadFile("archive-catalog.csv")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range req.Inputs {
		found := false
		for _, row := range strings.Split(string(raw), "\n") {
			if packages[in.OriginalAssetID] != "" && strings.Contains(row, packages[in.OriginalAssetID]) && strings.Contains(row, want[in.OriginalAssetID]) {
				found = true
			}
		}
		if !found {
			t.Fatalf("catalog lost %s profile %s: %s", in.OriginalAssetID, want[in.OriginalAssetID], raw)
		}
	}
}
