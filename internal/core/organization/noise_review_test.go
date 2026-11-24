package organization

import (
	"fmt"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-027
func TestNoiseReviewSummarizesRecordedMatchesAndExceptions(t *testing.T) {
	const folder = "event/M4ROOT/SUB"
	policy := DefaultPolicy()
	policy.Version = DrivePolicyVersion
	request := AutoRequest{DecisionID: "D", Folder: folder, WorkspaceDriveID: "V", InputRevision: 3, Policy: policy, DecidedAt: time.Now(), CameraOriginals: CameraOriginalIndex{}}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("C%04d", i)
		request.Inputs = append(request.Inputs, organizationInput(id, folder+"/"+id+"S03.MP4", 5000, 20_000_000, 1920, 1080))
		if i < 5 {
			request.CameraOriginals.Observe("event/M4ROOT/CLIP/"+id+".MP4", 20_000_000)
		}
	}
	d, err := Classify(request)
	if err != nil || d.Disposition != vocabulary.OrganizationHold || len(d.Actions) != 0 {
		t.Fatalf("automatic decision: %+v %v", d, err)
	}
	got := SummarizeNoise(d, 5)
	if got == nil || got.Matched != 4 || got.Unconfirmed != 1 || got.UnconfirmedReasons["UNPAIRED_PROXY"] != 1 || len(got.MatchedExamples) != 3 || len(got.UnconfirmedExamples) != 1 || got.UnconfirmedExamples[0].ReasonCode != "UNPAIRED_PROXY" ||
		got.MatchedExamples[0].MatchedOriginal != "event/M4ROOT/CLIP/C0001.MP4" {
		t.Fatalf("review: %+v", got)
	}
	d.ClassificationEvidence = nil
	if old := SummarizeNoise(d, 6); old.Matched != 0 || old.Unconfirmed != 6 || old.UnconfirmedReasons["NOT_RECORDED"] != 6 || len(old.MatchedExamples) != 0 {
		t.Fatalf("legacy review: %+v", old)
	}
	for _, tc := range []struct{ folder, kind string }{{"event/XDROOT/Sub", "CAMERA_PROXY"}, {"edit.fcpbundle/event/Render Files/High Quality Media", "PROJECT_RENDER_CACHE"}, {"edit.fcpbundle/event/Original Media", ""}, {"event/Output", ""}, {"집회/촬영물", ""}} {
		if got := NoiseCandidateKind(tc.folder, nil); got != tc.kind {
			t.Errorf("%s = %q, want %q", tc.folder, got, tc.kind)
		}
	}
}
