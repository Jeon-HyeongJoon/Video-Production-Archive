package pipeline

import (
	"slices"
	"testing"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: PIP-001
func TestOccasionRecordingTemplateRequiresQCApprovalAndVerifiedArchiveCopies(t *testing.T) {
	template := OccasionRecording()
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(template.Steps["qc"].DependsOn, []string{"proxy", "thumbnail", "content_review"}) ||
		!template.Steps["archive_approval"].ApprovalRequired ||
		!slices.Equal(template.Steps["commit_archive"].DependsOn, []string{"verify_archive_primary", "verify_archive_mirror"}) {
		t.Fatalf("event workflow does not enforce integrated QC and approval: %+v", template.Steps)
	}
}

// Requirement: PIP-002
func TestVJTemplateRequiresTimelinePreviewApprovalBeforeRender(t *testing.T) {
	template := VJCompilation()
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	approval := template.Steps["timeline_approval"]
	if !approval.ApprovalRequired || !slices.Equal(approval.DependsOn, []string{"preview"}) ||
		!slices.Equal(template.Steps["render"].DependsOn, []string{"timeline_approval"}) ||
		!template.Steps["archive_approval"].ApprovalRequired ||
		!slices.Equal(template.Steps["archive_approval"].DependsOn, []string{"stage_archive_package"}) {
		t.Fatalf("VJ review gate is invalid: %+v", template.Steps)
	}
}

// Requirement: PIP-003
func TestFinalEditTemplateRecordsVersionBeforeDerivativesAndDeliveryAfterQC(t *testing.T) {
	template := FinalEdit()
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(template.Steps["analyze"].DependsOn, []string{"record_edit_version"}) ||
		!slices.Equal(template.Steps["record_delivery"].DependsOn, []string{"qc"}) {
		t.Fatalf("final edit ordering is invalid: %+v", template.Steps)
	}
}

// Requirement: PIP-004
func TestLargeFileIntakeReleasesWorkspaceDriveWithoutReturnApproval(t *testing.T) {
	template := LargeFileIntake()
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	if template.Name != "large-file-intake" ||
		!slices.Equal(template.Steps["register_original_assets"].DependsOn, []string{"inspect_inputs"}) ||
		!slices.Equal(template.Steps["verify_first"].DependsOn, []string{"copy_first"}) ||
		!slices.Equal(template.Steps["copy_second"].DependsOn, []string{"verify_first"}) ||
		!slices.Equal(template.Steps["verify_second"].DependsOn, []string{"copy_second"}) ||
		!slices.Equal(template.Steps["release_workspace_drive"].DependsOn, []string{"verify_second"}) {
		t.Fatalf("large-file intake verification chain is invalid: %+v", template.Steps)
	}
	if _, exists := template.Steps["return_approval"]; exists {
		t.Fatal("large-file intake contains a custody return approval")
	}
}

// Requirement: PIP-005
func TestTemplateRejectsCyclesAndRequiresResourceAndProcessingDeclarations(t *testing.T) {
	cycle := Template{Name: "cycle", Steps: map[string]StepDefinition{
		"a": {ID: "a", DependsOn: []string{"b"}, ResourceClass: vocabulary.ResourceControl, ExecutorKind: vocabulary.ExecutorControl},
		"b": {ID: "b", DependsOn: []string{"a"}, ResourceClass: vocabulary.ResourceControl, ExecutorKind: vocabulary.ExecutorControl},
	}}
	if err := cycle.Validate(); err == nil {
		t.Fatal("cyclic template accepted")
	}
	missingDeclaration := Template{Name: "missing", Steps: map[string]StepDefinition{
		"render": {ID: "render", ResourceClass: vocabulary.ResourceHeavy, ExecutorKind: vocabulary.ExecutorWorker},
	}}
	if err := missingDeclaration.Validate(); err == nil {
		t.Fatal("heavy step without processing kind accepted")
	}
	for _, template := range []Template{OccasionRecording(), VJCompilation(), FinalEdit(), LargeFileIntake()} {
		if err := template.Validate(); err != nil {
			t.Fatalf("%s is invalid: %v", template.Name, err)
		}
	}
	typo := Template{Name: "typo", Steps: map[string]StepDefinition{
		"render": {
			ID: "render", ResourceClass: vocabulary.ResourceHeavy, ExecutorKind: vocabulary.ExecutorWorker,
			RequiredProcessingKinds: []vocabulary.ProcessingKind{"RENDERR"},
		},
	}}
	if err := typo.Validate(); err == nil {
		t.Fatal("unknown processing kind accepted")
	}
}

// Requirement: PIP-006
func TestSummaryUsesAgentGatewayWithHumanFallbackAndContentReview(t *testing.T) {
	template := OccasionRecording()
	summary := template.Steps["summary"]
	review := template.Steps["content_review"]
	if summary.ExecutorKind != vocabulary.ExecutorAgentGateway || summary.FallbackExecutorKind != vocabulary.ExecutorHuman ||
		!slices.Equal(summary.RequiredProcessingKinds, []vocabulary.ProcessingKind{vocabulary.ProcessingSummary}) {
		t.Fatalf("summary routing is unsafe: %+v", summary)
	}
	if review.ExecutorKind != vocabulary.ExecutorHuman || !slices.Equal(review.DependsOn, []string{"summary"}) ||
		!slices.Contains(template.Steps["qc"].DependsOn, "content_review") {
		t.Fatalf("content review is not an explicit QC input: review=%+v qc=%+v", review, template.Steps["qc"])
	}
}
