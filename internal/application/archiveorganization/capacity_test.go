package archiveorganization

import (
	"errors"
	"testing"
)

// Requirement: ARC-019
func TestArchiveRunCapacityAdmissionIncludesSafetyReserveAndReturnsTypedShortage(t *testing.T) {
	const estimated = uint64(2 << 30)
	required, reserve := RequiredArchiveCapacity(estimated)
	if reserve != 107_374_183 || required != estimated+reserve {
		t.Fatalf("unexpected five percent reserve: required=%d reserve=%d", required, reserve)
	}
	if err := ValidateArchiveCapacity("archive-primary", estimated, required); err != nil {
		t.Fatalf("exact required capacity should pass: %v", err)
	}
	err := ValidateArchiveCapacity("archive-primary", estimated, required-1)
	if !errors.Is(err, ErrInsufficientArchiveCapacity) {
		t.Fatalf("shortage was not typed: %v", err)
	}
	var shortage *ArchiveCapacityError
	if !errors.As(err, &shortage) || shortage.StorageID != "archive-primary" ||
		shortage.EstimatedOutputBytes != estimated || shortage.SafetyReserveBytes != reserve ||
		shortage.RequiredBytes != required || shortage.FreeBytes != required-1 {
		t.Fatalf("shortage facts were not preserved: %+v", shortage)
	}

	minimumRequired, minimumReserve := RequiredArchiveCapacity(1)
	if minimumReserve != archiveCapacityMinimumReserveBytes || minimumRequired != 1+archiveCapacityMinimumReserveBytes {
		t.Fatalf("small jobs must keep the minimum publication reserve: required=%d reserve=%d", minimumRequired, minimumReserve)
	}
}
