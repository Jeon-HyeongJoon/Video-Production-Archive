package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func completeRequirement(id, status, level string) requirement {
	return requirement{
		ID: id, Design: "workflow-engine", Statement: "observable behavior",
		Given: "a precondition", When: "an action occurs", Then: "a result is observed",
		Status: status, Level: level,
	}
}

func TestValidateAllowsPlannedRequirementWithoutTest(t *testing.T) {
	req := completeRequirement("WFL-900", "planned", "integration")
	implemented, planned, err := validate(t.TempDir(), []requirement{req}, nil)
	if err != nil || implemented != 0 || planned != 1 {
		t.Fatalf("implemented=%d planned=%d err=%v", implemented, planned, err)
	}
}

func TestValidateRequiresExactlyOneTestForImplementedRequirement(t *testing.T) {
	root := t.TempDir()
	testFile := filepath.Join(root, "rule_test.go")
	if err := os.WriteFile(testFile, []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := completeRequirement("WFL-901", "implemented", "unit")
	req.TestFile = "rule_test.go"
	req.TestName = "TestRule"
	ref := testRef{ID: req.ID, File: req.TestFile, Name: req.TestName}
	implemented, planned, err := validate(root, []requirement{req}, []testRef{ref})
	if err != nil || implemented != 1 || planned != 0 {
		t.Fatalf("implemented=%d planned=%d err=%v", implemented, planned, err)
	}
	if _, _, err := validate(root, []requirement{req}, nil); err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Fatalf("missing test was accepted: %v", err)
	}
}

func TestValidateRejectsUnknownStatusAndLevel(t *testing.T) {
	req := completeRequirement("WFL-902", "draft", "system")
	_, _, err := validate(t.TempDir(), []requirement{req}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown status") || !strings.Contains(err.Error(), "unknown test level") {
		t.Fatalf("invalid lifecycle accepted: %v", err)
	}
}

func TestSemanticLockDetectsMeaningChangeButIgnoresTestLocation(t *testing.T) {
	req := completeRequirement("WFL-903", "implemented", "unit")
	lock := semanticLock{Version: 1, Requirements: requirementFingerprints([]requirement{req})}
	req.TestFile = "moved_test.go"
	req.TestName = "TestMoved"
	if err := validateSemanticLock([]requirement{req}, lock); err != nil {
		t.Fatalf("test ref changed requirement meaning: %v", err)
	}
	req.Then = "a different business result"
	if err := validateSemanticLock([]requirement{req}, lock); err == nil || !strings.Contains(err.Error(), "meaning changed") {
		t.Fatalf("semantic change was not detected: %v", err)
	}
}
