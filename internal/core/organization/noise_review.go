package organization

import (
	"strings"

	"videoarchive/internal/core/vocabulary"
)

// NoiseCandidateKind identifies a review context. A directory name alone does
// not establish that an original exists or that any file can be discarded.
func NoiseCandidateKind(folder string, role *SourceRoleEvidence) string {
	if role != nil && role.Role == RoleProxy {
		return "CAMERA_PROXY"
	}
	parts := strings.Split(folderKey(folder), "/")
	inLibrary := false
	for i, part := range parts {
		if inLibrary && part == "render files" {
			return "PROJECT_RENDER_CACHE"
		}
		if i > 0 && (parts[i-1] == "xdroot" || parts[i-1] == "m4root") && part == "sub" {
			return "CAMERA_PROXY"
		}
		inLibrary = inLibrary || strings.HasSuffix(part, ".fcpbundle")
	}
	return ""
}

type NoiseReviewExample struct {
	RelativePath    string `json:"relative_path"`
	MatchedOriginal string `json:"matched_original,omitempty"`
	ReasonCode      string `json:"reason_code"`
	NextCheck       string `json:"next_check,omitempty"`
}

type NoiseReview struct {
	Kind                string               `json:"kind"`
	DecisionAuthority   vocabulary.ActorType `json:"decision_authority"`
	Matched             int                  `json:"matched_count"`
	Unconfirmed         int                  `json:"unconfirmed_count"`
	UnconfirmedReasons  map[string]int       `json:"unconfirmed_reasons,omitempty"`
	MatchedExamples     []NoiseReviewExample `json:"matched_examples,omitempty"`
	UnconfirmedExamples []NoiseReviewExample `json:"unconfirmed_examples,omitempty"`
}

// SummarizeNoise uses only the recorded decision and its receipt's file count.
// Keep an independent sample of exceptions even in a folder full of matches.
func SummarizeNoise(d OrganizationDecision, fileCount int) *NoiseReview {
	kind := NoiseCandidateKind(d.Folder, d.SourceRole)
	if kind == "" {
		return nil
	}
	review := &NoiseReview{Kind: kind, DecisionAuthority: vocabulary.ActorHuman, Unconfirmed: max(fileCount, len(d.Inputs))}
	review.UnconfirmedReasons = make(map[string]int)
	if d.ClassificationEvidence == nil || d.ClassificationEvidence.State != "RECORDED" {
		if review.Unconfirmed > 0 {
			review.UnconfirmedReasons["NOT_RECORDED"] = review.Unconfirmed
		}
		return review
	}
	originals := make(map[string]string, len(d.Inputs))
	for _, input := range d.Inputs {
		originals[input.RelativePath] = input.MatchedOriginal
	}
	files := d.ClassificationEvidence.Files
	review.Unconfirmed = max(review.Unconfirmed, len(files))
	if missing := review.Unconfirmed - len(files); missing > 0 {
		review.UnconfirmedReasons["NOT_RECORDED"] = missing
	}
	for _, file := range files {
		example := NoiseReviewExample{RelativePath: file.RelativePath, ReasonCode: file.ReasonCode, NextCheck: file.NextCheck}
		if original := originals[file.RelativePath]; file.ReasonCode == "MATCHED_PROXY" && original != "" {
			review.Matched++
			review.Unconfirmed--
			example.MatchedOriginal = original
			if len(review.MatchedExamples) < 3 {
				review.MatchedExamples = append(review.MatchedExamples, example)
			}
		} else {
			if example.ReasonCode == "" || example.ReasonCode == "MATCHED_PROXY" {
				example.ReasonCode = "NOT_RECORDED"
			}
			review.UnconfirmedReasons[example.ReasonCode]++
			if len(review.UnconfirmedExamples) < 3 {
				review.UnconfirmedExamples = append(review.UnconfirmedExamples, example)
			}
		}
	}
	return review
}
