package organization

import (
	"slices"
	"strings"

	"videoarchive/internal/core/vocabulary"
)

const ContextPolicyVersion uint32 = 3
const DrivePolicyVersion uint32 = 4

type ClassificationEvidence struct {
	State         string         `json:"state"`
	PolicyVersion uint32         `json:"policy_version,omitempty"`
	Files         []FileEvidence `json:"files,omitempty"`
}

type FileEvidence struct {
	RelativePath string   `json:"relative_path"`
	ReasonCode   string   `json:"reason_code"`
	Basis        []string `json:"basis"`
	NextCheck    string   `json:"next_check"`
}

func (d *OrganizationDecision) recordHold(context EventContext, inputs []Input) {
	if d.Disposition != vocabulary.OrganizationHold || len(d.HoldReasons) == 0 {
		return
	}
	d.EventPreview = nil
	record := &ClassificationEvidence{State: "RECORDED", PolicyVersion: d.PolicyVersion}
	if len(d.Inputs) > 0 {
		inputs = d.Inputs
	}
	noiseKind := NoiseCandidateKind(d.Folder, d.SourceRole)
	for _, v := range inputs {
		if !safeRelative(v.RelativePath) {
			continue
		}
		f := FileEvidence{RelativePath: v.RelativePath, ReasonCode: string(d.HoldReasons[0]), NextCheck: "CHECK_EVENT_AND_ROLE", Basis: []string{d.Folder}}
		if v.Inspection.ID != "" {
			f.Basis = append(f.Basis, v.Inspection.ID)
		}
		if d.SourceRole != nil {
			f.Basis = append(f.Basis, d.SourceRole.Basis)
		}
		switch {
		case d.HoldReasons[0] == vocabulary.OrganizationHoldMissingInspection:
			f.NextCheck = "CHECK_INSPECTION"
		case context.ListingChanged:
			f.ReasonCode = "LISTING_CHANGED"
			f.NextCheck = "RESCAN_AND_COMPARE_CARD_LIST"
			f.Basis = append(f.Basis, context.ListingDigest)
		case len(context.ConflictingEventFolders) > 0:
			f.ReasonCode = "EVENT_CONFLICT"
			f.NextCheck = "CHECK_EVENT_OWNERSHIP"
			f.Basis = append(f.Basis, context.ConflictingEventFolders...)
		case len(context.OverlappingPositionFolders) > 0:
			f.ReasonCode = "POSITION_OVERLAP"
			f.NextCheck = "CHECK_CHANNEL_COVERAGE"
			f.Basis = append(f.Basis, context.OverlappingPositionFolders...)
		case context.EditorialWitness != "":
			f.ReasonCode = "EDITORIAL_INPUT"
			f.NextCheck = "CHECK_ORIGINAL_SOURCE"
			f.Basis = append(f.Basis, context.EditorialWitness)
		case v.MatchedOriginal != "":
			f.ReasonCode = "MATCHED_PROXY"
			f.NextCheck = "REVIEW_MATCHED_ORIGINAL"
			f.Basis = append(f.Basis, v.MatchedOriginal)
		case noiseKind == "PROJECT_RENDER_CACHE":
			f.ReasonCode, f.NextCheck = "PROJECT_RENDER_CACHE", "CHECK_PROJECT_ORIGINAL"
		case noiseKind == "CAMERA_PROXY":
			f.ReasonCode = "UNPAIRED_PROXY"
			f.NextCheck = "FIND_CARD_ORIGINAL"
		case context.InputRevision != 0 && context.EventFolder == "":
			f.ReasonCode = "EVENT_UNKNOWN"
			f.NextCheck = "CHECK_EVENT_OWNERSHIP"
		case d.HoldReasons[0] == vocabulary.OrganizationHoldIncompatibleSequence:
			f.NextCheck = "CHECK_STREAM_COMPATIBILITY"
		case d.HoldReasons[0] == vocabulary.OrganizationHoldNonconsecutiveSequence || (d.SourceRole != nil && d.SourceRole.Role == RoleSource):
			f.NextCheck = "CHECK_SEQUENCE_AND_CARD_LIST"
		case d.SourceRole != nil && d.SourceRole.Role == RoleRecording:
			f.NextCheck = "CHECK_CHANNEL_AND_SPLITS"
		}
		record.Files = append(record.Files, f)
	}
	slices.SortFunc(record.Files, func(a, b FileEvidence) int { return strings.Compare(a.RelativePath, b.RelativePath) })
	d.ClassificationEvidence = record
}

func corroboratedPosition(folder string, ctx EventContext, inputs []Input) bool {
	pos, parent := positionBranch(folder, ctx.EventFolder)
	if pos == "" || len(inputs) != 1 || SourceMetadataHint(folder, inputs[0].Inspection) != LongRecordingHint {
		return false
	}
	for _, p := range ctx.ChannelFolders {
		name, otherParent := programBranch(p, ctx.EventFolder)
		if name != "" && otherParent == parent && p != folder && !editorialPath(p) {
			return true
		}
	}
	return false
}
