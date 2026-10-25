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

func agentStep(id string, processingKind vocabulary.ProcessingKind, fallback vocabulary.ExecutorKind, dependsOn ...string) StepDefinition {
	return StepDefinition{
		ID: id, DependsOn: dependsOn, ResourceClass: vocabulary.ResourceLight,
		ExecutorKind: vocabulary.ExecutorAgentGateway, FallbackExecutorKind: fallback,
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
		agentStep("summary", vocabulary.ProcessingSummary, vocabulary.ExecutorHuman, "transcript"),
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

func SourceMediaIntake() Template {
	return newTemplate("source-media-intake",
		lightStep("detect_device", vocabulary.ProcessingDevice),
		lightStep("inspect_volume", vocabulary.ProcessingDevice, "detect_device"),
		humanStep("assign_source_role", "inspect_volume"),
		heavyStep("copy_first", vocabulary.ProcessingChecksum, "assign_source_role"),
		heavyStep("verify_first", vocabulary.ProcessingChecksum, "copy_first"),
		heavyStep("copy_second", vocabulary.ProcessingChecksum, "verify_first"),
		heavyStep("verify_second", vocabulary.ProcessingChecksum, "copy_second"),
		approvalStep("return_approval", "verify_first", "verify_second"),
		lightStep("safe_eject", vocabulary.ProcessingDevice, "return_approval"),
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
		requiresProcessing := step.ExecutorKind == vocabulary.ExecutorWorker || step.ExecutorKind == vocabulary.ExecutorAgentGateway
		if requiresProcessing != (len(step.RequiredProcessingKinds) > 0) {
			return fmt.Errorf("step %s has inconsistent processing declaration", id)
		}
		for _, processingKind := range step.RequiredProcessingKinds {
			if !processingKind.Valid() {
				return fmt.Errorf("step %s has invalid processing kind %q", id, processingKind)
			}
			if processingKind == vocabulary.ProcessingSummary && step.ExecutorKind != vocabulary.ExecutorAgentGateway {
				return fmt.Errorf("step %s routes SUMMARY outside Agent Gateway", id)
			}
		}
		if step.ExecutorKind == vocabulary.ExecutorAgentGateway {
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
