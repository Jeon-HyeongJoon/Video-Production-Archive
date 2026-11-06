package device

import (
	"errors"
	"testing"
	"time"

	"videoarchive/internal/core/confirmation"
	"videoarchive/internal/core/vocabulary"
)

// Planned contract: DEV-001
func TestSharedReadsCoexistAndBlockExclusiveWrite(t *testing.T) {
	locks := NewLockManager()
	if err := locks.Acquire("vol-1", "reader-a", SharedRead); err != nil {
		t.Fatal(err)
	}
	if err := locks.Acquire("vol-1", "reader-b", SharedRead); err != nil {
		t.Fatal(err)
	}
	if err := locks.Acquire("vol-1", "writer", ExclusiveWrite); !errors.Is(err, ErrLockConflict) {
		t.Fatalf("exclusive write was not blocked: %v", err)
	}
	locks.Release("vol-1", "reader-a")
	locks.Release("vol-1", "reader-b")
	if err := locks.Acquire("vol-1", "writer", ExclusiveWrite); err != nil {
		t.Fatalf("exclusive write did not acquire after readers left: %v", err)
	}
}

// Planned contract: DEV-002
func TestMaintenanceLockBlocksEveryOtherMode(t *testing.T) {
	locks := NewLockManager()
	if err := locks.Acquire("vol-1", "admin", Maintenance); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []LockMode{SharedRead, ExclusiveWrite, Maintenance, EjectPending} {
		if err := locks.Acquire("vol-1", "other", mode); !errors.Is(err, ErrLockConflict) {
			t.Fatalf("mode %s was not blocked: %v", mode, err)
		}
	}
}

// Planned contract: DEV-003
func TestEjectPendingRequiresNoLocksAndBlocksNewWork(t *testing.T) {
	locks := NewLockManager()
	if err := locks.Acquire("vol-1", "reader", SharedRead); err != nil {
		t.Fatal(err)
	}
	if err := locks.Acquire("vol-1", "eject", EjectPending); !errors.Is(err, ErrLockConflict) {
		t.Fatalf("eject acquired over active work: %v", err)
	}
	locks.Release("vol-1", "reader")
	if err := locks.Acquire("vol-1", "eject", EjectPending); err != nil {
		t.Fatal(err)
	}
	if err := locks.Acquire("vol-1", "late-reader", SharedRead); !errors.Is(err, ErrLockConflict) {
		t.Fatalf("new work acquired after eject pending: %v", err)
	}
}

func validFormatRequest() FormatRequest {
	return FormatRequest{
		TargetID: "disk-7", RequiredCopies: 2, VerifiedCopies: 2, ChecksumsVerified: true,
		Confirmation: &confirmation.Record{
			ID: "CONF-1", ActorType: vocabulary.ActorHuman, ActorID: "operator-1", TargetID: "disk-7",
			ExpectedLabel: "ARCHIVE-042", EnteredLabel: "ARCHIVE-042", OccurredAt: time.Now(),
		},
	}
}

// Planned contract: DEV-004
func TestFormatRequiresDeviceSafetyCopiesChecksumsAndTargetConfirmation(t *testing.T) {
	if err := ValidateFormat(validFormatRequest()); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cases := []func(*FormatRequest){
		func(r *FormatRequest) { r.IsSystemDisk = true },
		func(r *FormatRequest) { r.HasActiveLocks = true },
		func(r *FormatRequest) { r.ContainsPreservedData = true },
		func(r *FormatRequest) { r.VerifiedCopies = 1 },
		func(r *FormatRequest) { r.ChecksumsVerified = false },
		func(r *FormatRequest) { r.Confirmation = nil },
		func(r *FormatRequest) { r.Confirmation.EnteredLabel = "OTHER" },
	}
	for index, mutate := range cases {
		request := validFormatRequest()
		mutate(&request)
		if err := ValidateFormat(request); !errors.Is(err, ErrFormatPrecondition) {
			t.Fatalf("case %d accepted: %+v", index, request)
		}
	}
}
