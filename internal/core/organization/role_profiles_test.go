package organization

import (
	"path"
	"slices"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-029
func TestSelectedCaptureInputsPinProfilesByRoleAcrossDrives(t *testing.T) {
	const capture = "37년도/# 1. 교회 촬영물/370105-남양주-주일정오예배_촬영-본당"
	type file struct {
		name              string
		duration, bitrate int64
		width, height     int
	}
	short := file{"C0002.MP4", 12000, 28_000_000, 1920, 1080}
	first := short
	first.name = "C0001.MP4"
	gap := short
	gap.name = "C0004.MP4"
	const card = "촬영물/391216-행사/카드1"
	named := func(name string) file {
		f := short
		f.name = name
		return f
	}
	conflict := named("VJ-소스컷_PGM-녹화.mp4")
	// These names and measurements reproduce the five inspected inputs from
	// the Namyangju run; the checksum and asset identity stay separate per file.
	mixed := []file{
		{"금곡 138기.mp4", 1755887, 216928, 854, 480}, {"금곡138기.mp4", 1755887, 216928, 854, 480},
		{"파일명 손실된 파일 (273).mov", 9010, 97864653, 3840, 2160}, {"파일명 손실된 파일 (300).mov", 11545, 29811092, 1920, 1080}, {"파일명 손실된 파일 (343).mov", 12913, 29560207, 1920, 1080},
	}
	const render = vocabulary.OrganizationRenderRecordings
	const sequence = vocabulary.OrganizationCompileSourceSequence
	const hold = vocabulary.OrganizationHold
	for _, tc := range []struct {
		name, folder string
		files        []file
		version      uint32
		want         vocabulary.OrganizationDisposition
		roles        []string
	}{
		{"mixed event", capture, mixed, 4, render, []string{RoleRecording, RoleRecording, RoleSource, RoleSource, RoleSource}},
		{"pinned previous policy", capture, mixed, 3, hold, nil},
		{"undated capture on another drive", "촬영물/개막식", []file{first}, 4, render, []string{RoleSource}},
		{"one capture", capture, []file{short}, 4, render, []string{RoleSource}},
		{"VJ sequence on another drive", "다른행사/VJ", []file{short, first}, 4, sequence, []string{RoleSource, RoleSource}},
		{"unverified order preserves VJ cuts", "다른행사/VJ", []file{short, gap}, 4, render, []string{RoleSource, RoleSource}},
		{"program recordings", capture + "/PGM", []file{short, first}, 4, render, []string{RoleRecording, RoleRecording}},
		{"recorder name", "380101-행사/녹화본", []file{first}, 4, render, []string{RoleRecording}},
		{"proxy", capture + "/M4ROOT/SUB", []file{short}, 4, hold, nil},
		{"format bucket", "37년도/영상_mov", mixed, 4, hold, nil},
		{"reference", capture + "/편집/Output", []file{short}, 4, hold, nil},
		{"conflicting roles", "다른행사/VJ/PGM", []file{short}, 4, hold, nil},
		{"explicit PGM filename", card, []file{named("PGM-녹화.mp4")}, 4, render, []string{RoleRecording}},
		{"explicit VJ filename", card, []file{named("VJ-소스컷.mp4")}, 4, render, []string{RoleSource}},
		{"PGM with generic capture word", card, []file{named("PGM_촬영.mp4")}, 4, render, []string{RoleRecording}},
		{"VJ with embedded pgm letters", card, []file{named("VJ_Alpgmnotes.mp4")}, 4, render, []string{RoleSource}},
		{"conflict within one filename", card, []file{conflict}, 4, hold, nil},
		{"conflict on undated capture card", "촬영물/카드1", []file{conflict}, 4, hold, nil},
		{"previous policy uses explicit PGM folder", card + "/PGM", []file{conflict}, 3, render, []string{RoleRecording}},
		{"conflict holds independently identified sibling", card, []file{named("PGM-녹화.mp4"), conflict}, 4, hold, nil},
		{"unknown role", "380102-자료/미확인", []file{short}, 4, hold, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := DefaultPolicy()
			policy.Version = tc.version
			req := AutoRequest{DecisionID: "DECISION", WorkspaceDriveID: "DRIVE", Folder: tc.folder, InputRevision: 11, Policy: policy, DecidedAt: time.Unix(100, 0)}
			anchor, _ := eventAnchor(tc.folder)
			req.EventContext = EventContext{InputRevision: 11, EventFolder: anchor}
			for _, f := range tc.files {
				req.Inputs = append(req.Inputs, organizationInput(f.name, path.Join(tc.folder, f.name), f.duration, f.bitrate, f.width, f.height))
			}
			d, err := Classify(req)
			if err != nil || d.Disposition != tc.want {
				t.Fatalf("%s: %+v, %v", tc.folder, d, err)
			}
			if tc.want == hold {
				if len(d.Actions) != 0 || d.ProfileReference != "" || d.ClassificationEvidence == nil {
					t.Fatal(d)
				}
				for _, v := range d.Inputs {
					if path.Base(v.RelativePath) == conflict.name {
						found := slices.ContainsFunc(d.ClassificationEvidence.Files, func(f FileEvidence) bool {
							return f.RelativePath == v.RelativePath && f.ReasonCode == "FILENAME_ROLE_CONFLICT" &&
								f.NextCheck == "CHECK_SOURCE_OR_RECORDING" && slices.Contains(f.Basis, v.Inspection.ID)
						})
						if !found {
							t.Fatalf("missing conflicting filename and inspection evidence: %+v", d.ClassificationEvidence)
						}
					}
					if v.RenderSelection != nil {
						t.Fatal("partial render plan on HOLD")
					}
				}
				return
			}
			if len(d.Inputs) != len(tc.files) || len(d.Inputs) != len(d.OrderedOriginalAssetIDs) {
				t.Fatal(d)
			}
			for i, v := range d.Inputs {
				role := tc.roles[i]
				if tc.version == ContextPolicyVersion {
					if d.SourceRole.Role != role || d.ProfileReference != mustOrganizationProfile(6_000).Reference() || v.RenderSelection != nil {
						t.Fatalf("legacy recording decision: %+v", d)
					}
					continue
				}
				target := 6_000
				if role == RoleSource {
					target = 20_000
				}
				s := v.RenderSelection
				if s == nil || s.Role != role || s.ProfileReference != mustOrganizationProfile(target).Reference() || s.Basis == "" || v.Inspection.ID == "" || v.OriginalAssetID != d.OrderedOriginalAssetIDs[i] {
					t.Fatalf("unbound selection: %+v", v)
				}
			}
			if tc.name == "mixed event" && (d.ProfileReference != "" || d.SourceRole.Role != RoleMixed) {
				t.Fatal(d)
			}
			if tc.want == sequence {
				timeline, err := BuildSourceTimeline("TIMELINE", 1, d)
				if err != nil || len(timeline.Markers) != 1 || timeline.Markers[0].AtMillis != first.duration || d.Inputs[0].OriginalAssetID != first.name {
					t.Fatal(timeline, err)
				}
			}
		})
	}
}
