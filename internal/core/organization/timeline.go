package organization

import (
	"errors"
	"strings"

	"videoarchive/internal/core/vocabulary"
)

var ErrInvalidTimeline = errors.New("source timeline is incomplete or inconsistent")

type SourceClip struct {
	Sequence        uint32 `json:"sequence"`
	OriginalAssetID string `json:"original_asset_id"`
	InMillis        int64  `json:"in_millis"`
	OutMillis       int64  `json:"out_millis"`
}

type CutMarker struct {
	BeforeClipID string `json:"before_clip_id"`
	AfterClipID  string `json:"after_clip_id"`
	AtMillis     int64  `json:"at_millis"`
}

type SourceTimeline struct {
	ID         string       `json:"id"`
	Revision   uint64       `json:"revision"`
	DecisionID string       `json:"decision_id"`
	Clips      []SourceClip `json:"clips"`
	Markers    []CutMarker  `json:"markers"`
}

func BuildSourceTimeline(id string, revision uint64, decision OrganizationDecision) (SourceTimeline, error) {
	if id == "" || revision == 0 || decision.ID == "" || decision.Disposition != vocabulary.OrganizationCompileSourceSequence || len(decision.Inputs) < 2 {
		return SourceTimeline{}, ErrInvalidDecision
	}
	timeline := SourceTimeline{ID: id, Revision: revision, DecisionID: decision.ID}
	var elapsed int64
	for index, input := range decision.Inputs {
		duration := input.Inspection.DurationMillis
		if duration <= 0 {
			return SourceTimeline{}, ErrInvalidDecision
		}
		timeline.Clips = append(timeline.Clips, SourceClip{
			Sequence: uint32(index + 1), OriginalAssetID: input.OriginalAssetID, InMillis: 0, OutMillis: duration,
		})
		elapsed += duration
		if index < len(decision.Inputs)-1 {
			timeline.Markers = append(timeline.Markers, CutMarker{
				BeforeClipID: input.OriginalAssetID, AfterClipID: decision.Inputs[index+1].OriginalAssetID, AtMillis: elapsed,
			})
		}
	}
	return timeline, nil
}

func (timeline SourceTimeline) Validate() error {
	if strings.TrimSpace(timeline.ID) == "" || timeline.Revision == 0 || strings.TrimSpace(timeline.DecisionID) == "" ||
		len(timeline.Clips) < 2 || len(timeline.Markers) != len(timeline.Clips)-1 {
		return ErrInvalidTimeline
	}
	seen := make(map[string]bool, len(timeline.Clips))
	var elapsed int64
	for index, clip := range timeline.Clips {
		if clip.Sequence != uint32(index+1) || strings.TrimSpace(clip.OriginalAssetID) == "" || seen[clip.OriginalAssetID] ||
			clip.InMillis < 0 || clip.OutMillis <= clip.InMillis {
			return ErrInvalidTimeline
		}
		seen[clip.OriginalAssetID] = true
		elapsed += clip.OutMillis - clip.InMillis
		if index < len(timeline.Markers) {
			marker := timeline.Markers[index]
			if marker.BeforeClipID != clip.OriginalAssetID || marker.AfterClipID != timeline.Clips[index+1].OriginalAssetID ||
				marker.AtMillis != elapsed {
				return ErrInvalidTimeline
			}
		}
	}
	return nil
}

func (timeline SourceTimeline) DurationMillis() int64 {
	var duration int64
	for _, clip := range timeline.Clips {
		duration += clip.OutMillis - clip.InMillis
	}
	return duration
}
