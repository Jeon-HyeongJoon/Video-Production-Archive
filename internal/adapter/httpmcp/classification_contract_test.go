package httpmcp

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

// Requirement: ORG-023
func TestHoldDetailReturnsRecordedClassificationEvidence(t *testing.T) {
	type assetFact struct {
		Path   string   `json:"relative_path"`
		Reason string   `json:"reason_code"`
		Basis  []string `json:"basis"`
		Next   string   `json:"next_check"`
	}
	for _, legacy := range []bool{false, true} {
		state := holdReviewDriveState(t)
		facts := []assetFact{
			{"recordings/a.mov", "CHANNEL_UNKNOWN", []string{"INSPECTION-ASSET-A"}, "CHECK_CHANNEL"},
			{"recordings/b.mov", "ORDER_UNKNOWN", []string{"INSPECTION-ASSET-B"}, "CHECK_SEQUENCE"},
		}
		if !legacy {
			// Supply the recorded projection to the real MCP transport.
			fields, err := json.Marshal(map[string]any{"ClassificationEvidence": map[string]any{"policy_version": 3, "state": "RECORDED", "files": facts}})
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(fields, &state.view.Folders[0]); err != nil {
				t.Fatal(err)
			}
		}
		response := mcpTestRequest(holdReviewMCPHandler(t, state), http.MethodPost, "/mcp/v1/call", archiveToolCall("HOLD-EVIDENCE", ToolOrganizationHoldGet, `{"folder":"recordings"}`), "secret")
		var body struct {
			Data struct {
				Run      string `json:"archive_run_id"`
				Decision string `json:"decision_id"`
				Revision int    `json:"input_revision"`
				Evidence struct {
					State   string      `json:"state"`
					Version int         `json:"policy_version"`
					Files   []assetFact `json:"files"`
				} `json:"classification_evidence"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("MCP %d: %v", response.Code, err)
		}
		data := body.Data
		if data.Run != "ARCHIVE-RUN-1" || data.Decision != "DECISION-HOLD" || data.Revision != 7 {
			t.Fatal("wrong recorded decision")
		}
		if legacy {
			if data.Evidence.State != "NOT_RECORDED" || len(data.Evidence.Files) != 0 {
				t.Errorf("legacy evidence: %+v", data.Evidence)
			}
			continue
		}
		if data.Evidence.State != "RECORDED" || data.Evidence.Version != 3 || len(data.Evidence.Files) != len(facts) {
			t.Errorf("missing v3 recorded evidence: %+v", data.Evidence)
			continue
		}
		for _, want := range facts {
			found := false
			for _, got := range data.Evidence.Files {
				if got.Path == want.Path && got.Reason == want.Reason && got.Next == want.Next && len(got.Basis) == 1 && got.Basis[0] == want.Basis[0] {
					found = true
				}
			}
			if !found {
				t.Errorf("missing recorded reason or remedy for %s", want.Path)
			}
		}
	}
}

// Requirement: ORG-026
func TestArchiveRunStatusReportsEventOutputWorkEvidence(t *testing.T) {
	state := holdReviewDriveState(t)
	const facts = `{"EventOutputEvidence":[{"archive_run_id":"ARCHIVE-RUN-1","input_revision":7,"relative_video":"행사/PGM.mp4","stage":"RENDERING","completed":20,"total":60,"unit":"seconds","basis":["progress-7"]},{"archive_run_id":"ARCHIVE-RUN-1","input_revision":7,"relative_video":"행사/천장캠.mp4","stage":"FAILED","cause_code":"ENCODER_UNAVAILABLE","basis":["failure-3"]},{"archive_run_id":"ARCHIVE-RUN-1","input_revision":7,"relative_video":"행사/측면캠.mp4","stage":"STORED","basis":["qc-4","manifest-4"]},{"archive_run_id":"ARCHIVE-RUN-1","input_revision":7,"relative_video":"행사/VJ.mp4","stage":"SUBTITLE_REVIEW","cause_code":"INVALID_UTF8","issue_count":10,"basis":["native-caption-9"]}]}`
	if err := json.Unmarshal([]byte(facts), &state.view); err != nil {
		t.Fatal(err)
	}
	response := mcpTestRequest(holdReviewMCPHandler(t, state), http.MethodPost, "/mcp/v1/call", archiveToolCall("EVENT-WORK", ToolArchiveRunStatus, `{}`), "secret")
	var body struct {
		Data struct {
			Outputs []struct {
				Run        string   `json:"archive_run_id"`
				Revision   int      `json:"input_revision"`
				Video      string   `json:"relative_video"`
				Stage      string   `json:"stage"`
				Completed  int      `json:"completed"`
				Total      int      `json:"total"`
				Unit       string   `json:"unit"`
				Cause      string   `json:"cause_code"`
				IssueCount int      `json:"issue_count"`
				Basis      []string `json:"basis"`
			} `json:"event_output_evidence"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
		t.Fatalf("MCP %d: %v", response.Code, err)
	}
	if len(body.Data.Outputs) != 4 {
		t.Fatalf("expected four recorded channel states, got %d", len(body.Data.Outputs))
	}
	states := map[string]bool{}
	for _, o := range body.Data.Outputs {
		if o.Run != "ARCHIVE-RUN-1" || o.Revision != 7 {
			t.Errorf("wrong run/revision: %+v", o)
		}
		switch o.Stage {
		case "RENDERING":
			states[o.Stage] = o.Video == "행사/PGM.mp4" && o.Completed == 20 && o.Total == 60 && o.Unit == "seconds" && slices.Equal(o.Basis, []string{"progress-7"})
		case "FAILED":
			states[o.Stage] = o.Video == "행사/천장캠.mp4" && o.Cause == "ENCODER_UNAVAILABLE" && slices.Equal(o.Basis, []string{"failure-3"})
		case "SUBTITLE_REVIEW":
			states[o.Stage] = o.Video == "행사/VJ.mp4" && o.Cause == "INVALID_UTF8" && o.IssueCount == 10 && slices.Equal(o.Basis, []string{"native-caption-9"})
		case "STORED":
			states[o.Stage] = o.Video == "행사/측면캠.mp4" && slices.Equal(o.Basis, []string{"qc-4", "manifest-4"})
		}
	}
	if !states["RENDERING"] || !states["FAILED"] || !states["STORED"] || !states["SUBTITLE_REVIEW"] {
		t.Errorf("missing evidence: %v", states)
	}
}

// Requirement: MCP-015
func TestArchivistListsHumanNoiseChoicesWithRecordedEvidence(t *testing.T) {
	state := holdReviewDriveState(t)
	f := &state.view.Folders[0]
	f.NoiseReview = &organization.NoiseReview{Kind: "CAMERA_PROXY", DecisionAuthority: "HUMAN", Matched: 8, Unconfirmed: 1,
		MatchedExamples:     []organization.NoiseReviewExample{{RelativePath: "SUB/C0001S01.mp4", MatchedOriginal: "CLIP/C0001.mp4", ReasonCode: "MATCHED_PROXY"}},
		UnconfirmedExamples: []organization.NoiseReviewExample{{RelativePath: "SUB/C0002S01.mp4", ReasonCode: "UNPAIRED_PROXY"}}}
	f.WorkOptions = []archiveintakeworkspace.WorkOptionView{{Disposition: vocabulary.OrganizationHold}, {Disposition: vocabulary.OrganizationExcludeFromRun}}
	handler := holdReviewMCPHandler(t, state)
	for _, tool := range []string{ToolOrganizationHoldsList, ToolOrganizationHoldGet} {
		input := "{}"
		if tool == ToolOrganizationHoldGet {
			input = `{"folder":"recordings"}`
		}
		resp := mcpTestRequest(handler, http.MethodPost, "/mcp/v1/call", archiveToolCall("READ", tool, input), "secret")
		var body struct {
			Data json.RawMessage `json:"data"`
		}
		if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &body) != nil {
			t.Fatal(resp.Body.String())
		}
		data := body.Data
		if tool == ToolOrganizationHoldsList {
			var list struct {
				Holds []json.RawMessage `json:"holds"`
			}
			if json.Unmarshal(data, &list) != nil || len(list.Holds) != 1 {
				t.Fatal(string(data))
			}
			data = list.Holds[0]
		}
		var item struct {
			Review   *organization.NoiseReview               `json:"noise_review"`
			Options  []archiveintakeworkspace.WorkOptionView `json:"work_options"`
			Decision string                                  `json:"decision_id"`
		}
		if json.Unmarshal(data, &item) != nil || !reflect.DeepEqual(item.Review, f.NoiseReview) || !slices.Equal(item.Options, f.WorkOptions) || item.Decision != f.DecisionID || len(state.overrides) != 0 {
			t.Fatalf("noise evidence or options lost: %s", data)
		}
	}
}
