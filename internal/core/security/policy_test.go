package security

import (
	"testing"
	"time"

	"videoarchive/internal/core/approval"
	"videoarchive/internal/core/confirmation"
	"videoarchive/internal/core/vocabulary"
)

func validConfirmation() *confirmation.Record {
	return &confirmation.Record{
		ID: "CONF-1", ActorType: vocabulary.ActorHuman, ActorID: "operator-1", TargetID: "disk-7",
		ExpectedLabel: "ARCHIVE-042", EnteredLabel: "ARCHIVE-042", OccurredAt: time.Now(),
	}
}

func validApproval() approval.Record {
	return approval.Record{
		ID: "APR-1", Sequence: 1, ActorType: vocabulary.ActorHuman, ActorID: "operator-1",
		TargetID: "PACKAGE-1", TargetRevision: 3, Decision: vocabulary.ApprovalApproved,
		Reason: "QC reviewed", Basis: []string{"QC-1"}, OccurredAt: time.Now(),
	}
}

func validDestructiveApproval() approval.Record {
	record := validApproval()
	record.TargetID = "disk-7"
	record.TargetRevision = 1
	record.Basis = []string{"DEVICE-INSPECTION-1"}
	return record
}

// Requirement: SEC-001
func TestDestructiveActionUsesBootPolicyWithoutAdminInDisabledMode(t *testing.T) {
	valid := Request{
		ActorType: vocabulary.ActorHuman, AuthMode: AuthDisabled, Role: Operator, Action: Destructive,
		TargetID: "disk-7", TargetRevision: 1, ConfigPolicyAllows: true,
		Approvals: []approval.Record{validDestructiveApproval()}, Confirmation: validConfirmation(),
	}
	cases := []Request{
		func() Request { value := valid; value.ActorType = vocabulary.ActorAgent; return value }(),
		func() Request { value := valid; value.ConfigPolicyAllows = false; return value }(),
		func() Request { value := valid; value.Approvals = nil; return value }(),
		func() Request { value := valid; value.Confirmation = nil; return value }(),
		func() Request {
			value := valid
			mismatch := *value.Confirmation
			mismatch.TargetID = "disk-8"
			value.Confirmation = &mismatch
			return value
		}(),
	}
	for _, request := range cases {
		if Authorize(request) {
			t.Fatalf("unauthorized destructive request accepted: %+v", request)
		}
	}
	if !Authorize(valid) {
		t.Fatal("boot-policy-enabled and fully confirmed operator request rejected")
	}
	valid.AuthMode = AuthLocal
	valid.Role = Admin
	valid.ConfigPolicyAllows = false
	if !Authorize(valid) {
		t.Fatal("fully confirmed local-auth admin request rejected")
	}
}

// Requirement: SEC-002
func TestAgentProcessActionRequiresAutomationPolicy(t *testing.T) {
	request := Request{ActorType: vocabulary.ActorAgent, Action: Process}
	if Authorize(request) {
		t.Fatal("agent process action bypassed policy")
	}
	request.AutomationPolicyAllows = true
	if !Authorize(request) {
		t.Fatal("policy-approved agent process action rejected")
	}
}

// Requirement: SEC-003
func TestCommitRequiresExplicitApprovalFromHumanRole(t *testing.T) {
	approved := validApproval()
	for _, request := range []Request{
		{ActorType: vocabulary.ActorAgent, Role: Operator, Action: Commit, TargetID: "PACKAGE-1", TargetRevision: 3, Approvals: []approval.Record{approved}},
		{ActorType: vocabulary.ActorHuman, Role: Operator, Action: Commit, TargetID: "PACKAGE-1", TargetRevision: 3},
		{ActorType: vocabulary.ActorHuman, Role: Admin, Action: Commit, TargetID: "PACKAGE-1", TargetRevision: 4, Approvals: []approval.Record{approved}},
	} {
		if Authorize(request) {
			t.Fatalf("unapproved or agent commit accepted: %+v", request)
		}
	}
	for _, role := range []UserRole{Operator, Admin} {
		if !Authorize(Request{
			ActorType: vocabulary.ActorHuman, Role: role, Action: Commit,
			TargetID: "PACKAGE-1", TargetRevision: 3, Approvals: []approval.Record{approved},
		}) {
			t.Fatalf("approved human commit rejected for %s", role)
		}
	}
}

// Requirement: SEC-004
func TestRemoteManagementRequiresLocalAuthAdmin(t *testing.T) {
	for _, request := range []Request{
		{ActorType: vocabulary.ActorAgent, AuthMode: AuthLocal, Action: ManageSecurity},
		{ActorType: vocabulary.ActorHuman, AuthMode: AuthLocal, Role: Operator, Action: ManageSecurity},
		{ActorType: vocabulary.ActorHuman, AuthMode: AuthDisabled, Role: Admin, Action: ManageSecurity},
	} {
		if Authorize(request) {
			t.Fatalf("security settings allowed: %+v", request)
		}
	}
	if !Authorize(Request{ActorType: vocabulary.ActorHuman, AuthMode: AuthLocal, Role: Admin, Action: ManageSecurity}) {
		t.Fatal("security settings rejected for admin")
	}
}
