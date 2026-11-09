package archiveorganization

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

var ErrInsufficientArchiveCapacity = errors.New("archive storage capacity is insufficient")

const archiveCapacityMinimumReserveBytes uint64 = 64 << 20

// ArchiveCapacityError is safe to project to an operator: it contains only a
// stable Storage identity and byte counts, never a StorageRoot path.
type ArchiveCapacityError struct {
	StorageID            string
	EstimatedOutputBytes uint64
	SafetyReserveBytes   uint64
	RequiredBytes        uint64
	FreeBytes            uint64
}

func (failure *ArchiveCapacityError) Error() string {
	return fmt.Sprintf("%s: storage %s requires %d bytes (%d estimated + %d reserve), %d free",
		ErrInsufficientArchiveCapacity, failure.StorageID, failure.RequiredBytes,
		failure.EstimatedOutputBytes, failure.SafetyReserveBytes, failure.FreeBytes)
}

func (*ArchiveCapacityError) Unwrap() error { return ErrInsufficientArchiveCapacity }

// RequiredArchiveCapacity adds five percent headroom, rounded up, with a
// 64 MiB minimum for package sidecars and temporary publication metadata.
func RequiredArchiveCapacity(estimatedOutputBytes uint64) (requiredBytes, safetyReserveBytes uint64) {
	safetyReserveBytes = estimatedOutputBytes / 20
	if estimatedOutputBytes%20 != 0 {
		safetyReserveBytes++
	}
	if safetyReserveBytes < archiveCapacityMinimumReserveBytes {
		safetyReserveBytes = archiveCapacityMinimumReserveBytes
	}
	if estimatedOutputBytes > math.MaxUint64-safetyReserveBytes {
		return math.MaxUint64, safetyReserveBytes
	}
	return estimatedOutputBytes + safetyReserveBytes, safetyReserveBytes
}

func ValidateArchiveCapacity(storageID string, estimatedOutputBytes, freeBytes uint64) error {
	requiredBytes, reserveBytes := RequiredArchiveCapacity(estimatedOutputBytes)
	if freeBytes >= requiredBytes {
		return nil
	}
	return &ArchiveCapacityError{
		StorageID: strings.TrimSpace(storageID), EstimatedOutputBytes: estimatedOutputBytes,
		SafetyReserveBytes: reserveBytes, RequiredBytes: requiredBytes, FreeBytes: freeBytes,
	}
}
