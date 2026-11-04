package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validOntology() ontology {
	return ontology{
		Version: 1,
		Layers:  []vocabulary{{ID: "product", Description: "제품"}, {ID: "shared", Description: "공통"}},
		Kinds: []kind{
			{ID: "entity", Description: "개체"},
			{ID: "category", Description: "상위 개념"},
			{ID: "state-set", Description: "상태 집합", HasValues: true, CodeSuffix: "State"},
		},
		RelationTypes: []vocabulary{{ID: "is-a", Description: "종류"}},
		FollowUpTypes: []vocabulary{{ID: "code", Description: "코드 변경"}},
		Terms: []term{
			{
				ID: "evidence", Layer: "shared", Kind: "category", Name: name{KO: "증적", EN: "Evidence"},
				Definition: "관찰 기록", Forbidden: forbidden{Words: []string{"증거"}}, Sources: []string{"user"}, Decided: "2026-10-19",
			},
			{
				ID: "job", Layer: "product", Kind: "entity", Name: name{KO: "작업", EN: "Job"},
				Definition: "실행 인스턴스", Relations: []relation{{Type: "is-a", Target: "evidence"}}, Sources: []string{"user"}, Decided: "2026-10-19",
			},
			{
				ID: "job-state", Layer: "product", Kind: "state-set", Name: name{KO: "작업 상태", EN: "Job State"}, CodeName: "JobState",
				Definition: "상태 집합", Values: []vocabulary{{ID: "QUEUED", Description: "대기"}}, Sources: []string{"user"}, Decided: "2026-10-19",
			},
		},
		Open:      []openQuestion{{ID: "event", Words: []string{"Event"}, Senses: []sense{{Meaning: "행사"}, {Meaning: "도메인 이벤트"}}}},
		FollowUps: []followUp{{Term: "job", Type: "code", From: "pipeline.Job", To: "workflow.Job"}},
	}
}

func TestValidOntologyHasNoProblems(t *testing.T) {
	if problems := validate(validOntology()); len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
}

func TestValidateRejectsAmbiguousOrDanglingDefinitions(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ontology)
		want string
	}{
		{"duplicate id", func(o *ontology) { o.Terms[1].ID = "evidence" }, "duplicate term evidence"},
		{"unknown layer", func(o *ontology) { o.Terms[0].Layer = "ops" }, "unknown layer"},
		{"unknown kind", func(o *ontology) { o.Terms[0].Kind = "thing" }, "unknown kind"},
		{"missing definition", func(o *ontology) { o.Terms[0].Definition = "" }, "incomplete definition"},
		{"invalid date", func(o *ontology) { o.Terms[0].Decided = "today" }, "invalid decided date"},
		{"one word names two terms", func(o *ontology) { o.Terms[1].Name.KO = "증적" }, `"증적" names both evidence and job`},
		{"term forbids its own name", func(o *ontology) { o.Terms[0].Forbidden.Words = []string{"증적"} }, `evidence forbids its own name "증적"`},
		{"follow-up for unknown term", func(o *ontology) { o.FollowUps[0].Term = "missing" }, "belongs to unknown term missing"},
		{"follow-up with unknown type", func(o *ontology) { o.FollowUps[0].Type = "chat" }, `follow-up for job has unknown type "chat"`},
		{"follow-up without target", func(o *ontology) { o.FollowUps[0].To = "" }, "follow-up for job needs from and to"},
		{"unknown relation type", func(o *ontology) { o.Terms[1].Relations[0].Type = "owns" }, "unknown relation type"},
		{"dangling relation", func(o *ontology) { o.Terms[1].Relations[0].Target = "missing" }, "unknown term missing"},
		{"decided term still open", func(o *ontology) { o.Open[0].ID = "job" }, "already a decided term"},
		{"open question with one sense", func(o *ontology) { o.Open[0].Senses = o.Open[0].Senses[:1] }, "at least two senses"},
		{"vocabulary without description", func(o *ontology) { o.Kinds[0].Description = "" }, "kind entity has no description"},
		{"state set without values", func(o *ontology) { o.Terms[2].Values = nil }, "job-state is a state-set and needs values"},
		{"values on a kind without values", func(o *ontology) { o.Terms[0].Values = []vocabulary{{ID: "A", Description: "a"}} }, "does not take values"},
		{"lower case value", func(o *ontology) { o.Terms[2].Values[0].ID = "queued" }, `value "queued" of job-state is not upper SNAKE_CASE`},
		{"state set code name suffix", func(o *ontology) { o.Terms[2].CodeName = "JobStatus" }, `code name "JobStatus" must end with "State"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := validOntology()
			tc.edit(&entries)
			problems := strings.Join(validate(entries), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("problems=%q, want %q", problems, tc.want)
			}
		})
	}
}

func TestForbiddenIdentifierIsReportedWithLocation(t *testing.T) {
	root := t.TempDir()
	source := "package sample\n\ntype PipelineStep struct{}\n\nvar allowed = \"PipelineStep\"\n"
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := forbiddenIdentifiers(root, []term{{ID: "template-step", Forbidden: forbidden{Code: []string{"PipelineStep"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "sample.go:3 uses PipelineStep") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestGlobalForbiddenWordIsReportedButContextualWordIsNotScanned(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "design.md"), []byte("use Device Archivist here\nordinary workflow word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := globallyForbiddenWords(root, []string{"Device Archivist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "design.md:1") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestSharedVocabularyArchitectureRejectsDuplicateTypesAndStringFields(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "internal", "core", "sample")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "package sample\n\ntype WaitReason string\ntype Step struct { ProcessingKind string }\n"
	if err := os.WriteFile(filepath.Join(directory, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := sharedVocabularyArchitecture(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "redefines shared vocabulary type WaitReason") ||
		!strings.Contains(joined, "stores shared vocabulary field ProcessingKind as string") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestSharedVocabularyValuesDetectOntologyCodeDrift(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "internal", "core", "vocabulary")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "package vocabulary\n\ntype WaitReason string\nconst (\n WaitMemory WaitReason = \"MEMORY\"\n WaitDisk WaitReason = \"DISK\"\n)\n"
	if err := os.WriteFile(filepath.Join(directory, "vocabulary.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	terms := []term{{CodeName: "WaitReason", Values: []vocabulary{{ID: "MEMORY"}, {ID: "SLOT"}}}}
	problems, err := sharedVocabularyValues(root, terms)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "code has value DISK missing from ontology") ||
		!strings.Contains(joined, "ontology has value SLOT missing from code") {
		t.Fatalf("problems=%v", problems)
	}
}
