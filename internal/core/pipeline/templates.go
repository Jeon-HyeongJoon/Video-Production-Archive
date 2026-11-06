package pipeline

import (
	"fmt"

	"videoarchive/internal/core/vocabulary"
)

type StepDefinition struct {
	ID                      string
	DependsOn               []string
	ApprovalRequired        bool
	ResourceClass           vocabulary.ResourceClass
	ExecutorKind            vocabulary.ExecutorKind
	FallbackExecutorKind    vocabulary.ExecutorKind
	RequiredProcessingKinds []vocabulary.ProcessingKind
	InitialWaitReason       vocabulary.WaitReason
}

type Template struct {
	Name  string
	Steps map[string]StepDefinition
}

func newTemplate(name string, steps ...StepDefinition) Template {
	template := Template{Name: name, Steps: make(map[string]StepDefinition, len(steps))}
	for _, step := range steps {
		template.Steps[step.ID] = step
	}
	return template
}

func controlStep(id string, dependsOn ...string) StepDefinition {
	return StepDefinition{ID: id, DependsOn: dependsOn, ResourceClass: vocabulary.ResourceControl, ExecutorKind: vocabulary.ExecutorControl}
}

func humanStep(id string, dependsOn ...string) StepDefinition {
	return StepDefinition{ID: id, DependsOn: dependsOn, ResourceClass: vocabulary.ResourceControl, ExecutorKind: vocabulary.ExecutorHuman}
}

func waitingHumanStep(id string, reason vocabulary.WaitReason, dependsOn ...string) StepDefinition {
	step := humanStep(id, dependsOn...)
	step.InitialWaitReason = reason
	return step
}

func approvalStep(id string, dependsOn ...string) StepDefinition {
	step := humanStep(id, dependsOn...)
	step.ApprovalRequired = true
	return step
}

func workerStep(id string, class vocabulary.ResourceClass, processingKind vocabulary.ProcessingKind, dependsOn ...string) StepDefinition {
	return StepDefinition{
		ID: id, DependsOn: dependsOn, ResourceClass: class, ExecutorKind: vocabulary.ExecutorWorker,
		RequiredProcessingKinds: []vocabulary.ProcessingKind{processingKind},
	}
}

func lightStep(id string, processingKind vocabulary.ProcessingKind, dependsOn ...string) StepDefinition {
	return workerStep(id, vocabulary.ResourceLight, processingKind, dependsOn...)
}

func heavyStep(id string, processingKind vocabulary.ProcessingKind, dependsOn ...string) StepDefinition {
	return workerStep(id, vocabulary.ResourceHeavy, processingKind, dependsOn...)
}

func archivistStep(id string, processingKind vocabulary.ProcessingKind, fallback vocabulary.ExecutorKind, dependsOn ...string) StepDefinition {
	return StepDefinition{
		ID: id, DependsOn: dependsOn, ResourceClass: vocabulary.ResourceLight,
		ExecutorKind: vocabulary.ExecutorArchivistGateway, FallbackExecutorKind: fallback,
		RequiredProcessingKinds: []vocabulary.ProcessingKind{processingKind},
	}
}

func archiveSteps(after string) []StepDefinition {
	return []StepDefinition{
		controlStep("stage_archive_package", after),
		approvalStep("archive_approval", "stage_archive_package"),
		heavyStep("copy_archive_primary", vocabulary.ProcessingChecksum, "archive_approval"),
		heavyStep("verify_archive_primary", vocabulary.ProcessingChecksum, "copy_archive_primary"),
		heavyStep("copy_archive_mirror", vocabulary.ProcessingChecksum, "verify_archive_primary"),
		heavyStep("verify_archive_mirror", vocabulary.ProcessingChecksum, "copy_archive_mirror"),
		controlStep("commit_archive", "verify_archive_primary", "verify_archive_mirror"),
	}
}

func OccasionRecording() Template {
	steps := []StepDefinition{
		lightStep("intake", vocabulary.ProcessingInspect),
		lightStep("analyze", vocabulary.ProcessingInspect, "intake"),
		heavyStep("proxy", vocabulary.ProcessingRender, "analyze"),
		lightStep("thumbnail", vocabulary.ProcessingThumbnail, "analyze"),
		lightStep("audio", vocabulary.ProcessingAudioExtract, "analyze"),
		heavyStep("transcript", vocabulary.ProcessingSTT, "audio"),
		archivistStep("summary", vocabulary.ProcessingSummary, vocabulary.ExecutorHuman, "transcript"),
		humanStep("content_review", "summary"),
		heavyStep("qc", vocabulary.ProcessingQC, "proxy", "thumbnail", "content_review"),
	}
	return newTemplate("occasion-recording", append(steps, archiveSteps("qc")...)...)
}

func VJCompilation() Template {
	return newTemplate("vj-compilation",
		lightStep("intake", vocabulary.ProcessingInspect),
		lightStep("inspect_clips", vocabulary.ProcessingInspect, "intake"),
		humanStep("select_ok_cuts", "inspect_clips"),
		humanStep("timeline", "select_ok_cuts"),
		heavyStep("preview", vocabulary.ProcessingRender, "timeline"),
		approvalStep("timeline_approval", "preview"),
		heavyStep("render", vocabulary.ProcessingRender, "timeline_approval"),
		heavyStep("qc", vocabulary.ProcessingQC, "render"),
		controlStep("stage_archive_package", "qc"),
		approvalStep("archive_approval", "stage_archive_package"),
		heavyStep("copy_archive_primary", vocabulary.ProcessingChecksum, "archive_approval"),
		heavyStep("verify_archive_primary", vocabulary.ProcessingChecksum, "copy_archive_primary"),
		heavyStep("copy_archive_mirror", vocabulary.ProcessingChecksum, "verify_archive_primary"),
		heavyStep("verify_archive_mirror", vocabulary.ProcessingChecksum, "copy_archive_mirror"),
		controlStep("commit_archive", "verify_archive_primary", "verify_archive_mirror"),
	)
}

func FinalEdit() Template {
	return newTemplate("final-edit",
		lightStep("intake", vocabulary.ProcessingInspect),
		controlStep("duplicate_check", "intake"),
		controlStep("record_edit_version", "duplicate_check"),
		lightStep("analyze", vocabulary.ProcessingInspect, "record_edit_version"),
		heavyStep("proxy", vocabulary.ProcessingRender, "analyze"),
		lightStep("representative_image", vocabulary.ProcessingThumbnail, "analyze"),
		heavyStep("qc", vocabulary.ProcessingQC, "proxy", "representative_image"),
		controlStep("record_delivery", "qc"),
	)
}

func LargeFileIntake() Template {
	return newTemplate("large-file-intake",
		lightStep("inspect_inputs", vocabulary.ProcessingInspect),
		controlStep("register_original_assets", "inspect_inputs"),
		heavyStep("copy_first", vocabulary.ProcessingChecksum, "register_original_assets"),
		heavyStep("verify_first", vocabulary.ProcessingChecksum, "copy_first"),
		heavyStep("copy_second", vocabulary.ProcessingChecksum, "verify_first"),
		heavyStep("verify_second", vocabulary.ProcessingChecksum, "copy_second"),
		lightStep("release_workspace_drive", vocabulary.ProcessingDevice, "verify_second"),
	)
}

// ArchiveIntakeInspection is the Worker-side boundary between a fixed folder
// scope and the per-folder organization Jobs created from its observations.
func ArchiveIntakeInspection() Template {
	return newTemplate("archive-intake-inspection",
		lightStep("inspect", vocabulary.ProcessingInspect),
	)
}

func ArchiveOrganization(disposition vocabulary.OrganizationDisposition) (Template, error) {
	switch disposition {
	case vocabulary.OrganizationAsIs:
		return newTemplate("archive-intake-as-is",
			lightStep("qc", vocabulary.ProcessingQC),
			lightStep("organize", vocabulary.ProcessingOrganize, "qc"),
			lightStep("archive_catalog", vocabulary.ProcessingOrganize, "organize"),
		), nil
	case vocabulary.OrganizationRenderRecordings:
		return newTemplate("archive-intake-render-recordings",
			heavyStep("render", vocabulary.ProcessingRender),
			heavyStep("qc", vocabulary.ProcessingQC, "render"),
			lightStep("organize", vocabulary.ProcessingOrganize, "qc"),
			lightStep("archive_catalog", vocabulary.ProcessingOrganize, "organize"),
		), nil
	case vocabulary.OrganizationCompileSourceSequence:
		return newTemplate("archive-intake-source-sequence",
			controlStep("source_timeline"),
			heavyStep("sequence_render", vocabulary.ProcessingRender, "source_timeline"),
			heavyStep("qc", vocabulary.ProcessingQC, "sequence_render"),
			lightStep("organize", vocabulary.ProcessingOrganize, "qc"),
			lightStep("archive_catalog", vocabulary.ProcessingOrganize, "organize"),
		), nil
	case vocabulary.OrganizationHold:
		return newTemplate("archive-intake-hold", waitingHumanStep("resolve_classification", vocabulary.WaitInput)), nil
	default:
		return Template{}, fmt.Errorf("invalid organization disposition %q", disposition)
	}
}

// ArchiveTranscription is a separate stage-two Job. It is created only for a
// stored recording selected by the owning ArchiveRun gate.
func ArchiveTranscription() Template {
	return newTemplate("archive-transcription",
		lightStep("audio", vocabulary.ProcessingAudioExtract),
		heavyStep("transcript", vocabulary.ProcessingSTT, "audio"),
		lightStep("subtitle", vocabulary.ProcessingOrganize, "transcript"),
		lightStep("transcript_text", vocabulary.ProcessingOrganize, "transcript"),
		lightStep("archive_package_revision", vocabulary.ProcessingOrganize, "subtitle", "transcript_text"),
		lightStep("archive_catalog", vocabulary.ProcessingOrganize, "archive_package_revision"),
	)
}

func (template Template) Validate() error {
	for id, step := range template.Steps {
		if id == "" || step.ID != id {
			return fmt.Errorf("invalid step identity %q", id)
		}
		if !step.ResourceClass.Valid() || !step.ExecutorKind.Valid() {
			return fmt.Errorf("step %s has invalid resource or executor class", id)
		}
		if step.InitialWaitReason != "" && (!step.InitialWaitReason.Valid() || step.ExecutorKind != vocabulary.ExecutorHuman) {
			return fmt.Errorf("step %s has invalid initial wait reason", id)
		}
		requiresProcessing := step.ExecutorKind == vocabulary.ExecutorWorker || step.ExecutorKind == vocabulary.ExecutorArchivistGateway
		if requiresProcessing != (len(step.RequiredProcessingKinds) > 0) {
			return fmt.Errorf("step %s has inconsistent processing declaration", id)
		}
		for _, processingKind := range step.RequiredProcessingKinds {
			if !processingKind.Valid() {
				return fmt.Errorf("step %s has invalid processing kind %q", id, processingKind)
			}
			if processingKind == vocabulary.ProcessingSummary && step.ExecutorKind != vocabulary.ExecutorArchivistGateway {
				return fmt.Errorf("step %s routes SUMMARY outside Archivist Gateway", id)
			}
		}
		if step.ExecutorKind == vocabulary.ExecutorArchivistGateway {
			if step.FallbackExecutorKind != vocabulary.ExecutorHuman && step.FallbackExecutorKind != vocabulary.ExecutorControl {
				return fmt.Errorf("step %s has no non-AI fallback", id)
			}
		} else if step.FallbackExecutorKind != "" {
			return fmt.Errorf("step %s declares an irrelevant fallback executor", id)
		}
		for _, dependency := range step.DependsOn {
			if _, ok := template.Steps[dependency]; !ok {
				return fmt.Errorf("step %s has missing dependency %s", id, dependency)
			}
		}
	}
	visiting := make(map[string]bool, len(template.Steps))
	visited := make(map[string]bool, len(template.Steps))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("template contains dependency cycle at %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range template.Steps[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range template.Steps {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
