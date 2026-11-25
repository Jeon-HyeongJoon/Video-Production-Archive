package archiveintakeworkspace

import (
	"path"
	"time"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/job"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/workflow"
)

// A Folder Job can publish an individual package for every input, or one
// package for a sequence. The organize targets pin the inputs by package ID.
type storedOutput struct {
	output        execution.StepOutput
	presentation  archive.PackagePresentation
	directory     string
	originalIDs   []string
	role, profile string
	recordedAt    time.Time
	basis         []string
}

func storedOutputs(d organization.OrganizationDecision, state job.State) []storedOutput {
	var organize, qc *job.Step
	for i := range state.Steps {
		step := &state.Steps[i]
		if step.Workflow.State != workflow.Completed || step.OutputRecord == nil {
			continue
		}
		switch step.Workflow.ID {
		case "organize":
			organize = step
		case "qc":
			qc = step
		}
	}
	if organize == nil {
		return nil
	}

	targets := map[string]execution.OutputTarget{}
	if organize.ExecutionSpec != nil {
		for _, target := range organize.ExecutionSpec.Outputs {
			targets[target.ID] = target
		}
	}
	inputs := make(map[string]organization.Input, len(d.Inputs))
	var names, ids []string
	for _, in := range d.Inputs {
		inputs[in.OriginalAssetID] = in
		names = append(names, path.Base(in.RelativePath))
		ids = append(ids, in.OriginalAssetID)
	}
	// QC and organize can visit the inputs in different orders.
	qcRefs := make(map[string][]string)
	if qc != nil {
		refs := outputVerification(qc.OutputRecord, execution.OperationQCRendition)
		if len(qc.OutputRecord.Outputs) <= 1 {
			qcRefs[""] = qc.OutputRecord.VerificationReferences
		}
		if qc.ExecutionSpec != nil && len(qc.ExecutionSpec.Inputs) == len(qc.OutputRecord.Outputs) {
			for i, out := range qc.OutputRecord.Outputs {
				qcRefs[qc.ExecutionSpec.Inputs[i].OriginalAssetID] = refs[out.ID]
			}
		}
	}
	record := organize.OutputRecord
	refs := outputVerification(record, execution.OperationOrganizeMedia)
	var packages []storedOutput
	for _, out := range record.Outputs {
		if out.Kind != execution.OutputOrganizedMedia || out.ID == "" {
			continue
		}
		target := targets[out.ID]
		pkg := storedOutput{output: out, directory: completedWorkRelativePath(out, target), profile: d.ProfileReference, recordedAt: record.RecordedAt}
		var sourceNames []string
		if in, ok := inputs[target.OriginalAssetID]; ok {
			pkg.originalIDs = []string{in.OriginalAssetID}
			sourceNames = []string{path.Base(in.RelativePath)}
			if in.RenderSelection != nil {
				pkg.role, pkg.profile = in.RenderSelection.Role, in.RenderSelection.ProfileReference
			}
		} else if target.OriginalAssetID == "" && len(record.Outputs) == 1 {
			// A combined package has no individual target. Single older receipts
			// also predate the persisted target-to-original association.
			pkg.originalIDs, sourceNames = ids, names
		}
		if pkg.role == "" && d.SourceRole != nil {
			pkg.role = d.SourceRole.Role
		}
		pkg.presentation, _ = archive.PresentPackage(out.ID, d.Folder, sourceNames)
		if pkg.directory == path.Join("packages", out.ID, "revisions", "000001") && pkg.presentation.VideoFileName != "" {
			pkg.presentation.VideoFileName = "video.mp4"
		}
		qcBasis := qcRefs[target.OriginalAssetID]
		if len(record.Outputs) == 1 && len(qcBasis) == 0 {
			qcBasis = qcRefs[""]
		}
		pkg.basis = append(pkg.basis, qcBasis...)
		pkg.basis = append(pkg.basis, refs[out.ID]...)
		pkg.basis = append(pkg.basis, out.RelativePath)
		packages = append(packages, pkg)
	}
	return packages
}

// Old single-output receipts can contain opaque verification references.
// For multiple outputs only attach the proof recorded for that ID and digest.
func outputVerification(record *execution.StepOutputRecord, operation execution.Operation) map[string][]string {
	result := make(map[string][]string, len(record.Outputs))
	if len(record.Outputs) == 1 {
		result[record.Outputs[0].ID] = record.VerificationReferences
		return result
	}
	verified := make(map[string]bool, len(record.VerificationReferences))
	for _, ref := range record.VerificationReferences {
		verified[ref] = true
	}
	for _, out := range record.Outputs {
		ref := out.VerificationReference(operation)
		if verified[ref] {
			result[out.ID] = []string{ref}
		}
	}
	return result
}
