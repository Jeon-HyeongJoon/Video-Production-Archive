package intake

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"time"
)

var ErrInvalidFileObservation = errors.New("input file observation is incomplete or invalid")

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var windowsDrivePattern = regexp.MustCompile(`^[a-zA-Z]:/`)

// InputFileObservation is an append-only fact about a file beneath a
// registered Storage root. The absolute root is deliberately not retained.
type InputFileObservation struct {
	StorageID        string
	WorkspaceDriveID string
	RelativePath     string
	OriginalName     string
	SizeBytes        int64
	Checksum         string
	ObservedBy       string
	ObservedAt       time.Time
}

func NewInputFileObservation(value InputFileObservation) (InputFileObservation, error) {
	value.StorageID = strings.TrimSpace(value.StorageID)
	value.WorkspaceDriveID = strings.TrimSpace(value.WorkspaceDriveID)
	value.RelativePath = strings.TrimSpace(value.RelativePath)
	value.OriginalName = strings.TrimSpace(value.OriginalName)
	value.Checksum = strings.ToLower(strings.TrimSpace(value.Checksum))
	value.ObservedBy = strings.TrimSpace(value.ObservedBy)
	if value.StorageID == "" || value.WorkspaceDriveID == "" || value.RelativePath == "" || value.OriginalName == "" ||
		value.SizeBytes <= 0 || !sha256Pattern.MatchString(value.Checksum) || value.ObservedBy == "" || value.ObservedAt.IsZero() {
		return InputFileObservation{}, ErrInvalidFileObservation
	}
	clean := path.Clean(strings.ReplaceAll(value.RelativePath, "\\", "/"))
	if clean != value.RelativePath || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
		strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "//") || windowsDrivePattern.MatchString(clean) || path.Base(clean) != value.OriginalName {
		return InputFileObservation{}, ErrInvalidFileObservation
	}
	return value, nil
}
