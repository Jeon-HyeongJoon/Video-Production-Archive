package asset

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"videoarchive/internal/core/intake"
)

var (
	ErrInvalidOriginal   = errors.New("original asset is incomplete or invalid")
	ErrOriginalImmutable = errors.New("original asset is immutable")
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// OriginalAsset is the stable content identity registered from an input-file
// observation. Storage and path remain properties of observations and copies.
type OriginalAsset struct {
	ID                          string    `json:"id"`
	Checksum                    string    `json:"checksum"`
	OriginalName                string    `json:"original_name"`
	SizeBytes                   int64     `json:"size_bytes"`
	RegisteredFromObservationID string    `json:"registered_from_observation_id"`
	RegisteredBy                string    `json:"registered_by"`
	RegisteredAt                time.Time `json:"registered_at"`
}

func RegisterOriginalAsset(id string, observation intake.InputFileObservation, actorID string, registeredAt time.Time) (OriginalAsset, error) {
	normalizedObservation, err := intake.NewInputFileObservation(observation)
	if err != nil {
		return OriginalAsset{}, ErrInvalidOriginal
	}
	value := OriginalAsset{
		ID:                          strings.TrimSpace(id),
		Checksum:                    normalizedObservation.Checksum,
		OriginalName:                normalizedObservation.OriginalName,
		SizeBytes:                   normalizedObservation.SizeBytes,
		RegisteredFromObservationID: normalizedObservation.ID,
		RegisteredBy:                strings.TrimSpace(actorID),
		RegisteredAt:                registeredAt,
	}
	if err := value.Validate(); err != nil {
		return OriginalAsset{}, err
	}
	return value, nil
}

func (a OriginalAsset) Validate() error {
	if strings.TrimSpace(a.ID) == "" || !sha256Pattern.MatchString(a.Checksum) ||
		strings.TrimSpace(a.OriginalName) == "" || a.SizeBytes <= 0 ||
		strings.TrimSpace(a.RegisteredFromObservationID) == "" ||
		strings.TrimSpace(a.RegisteredBy) == "" || a.RegisteredAt.IsZero() {
		return ErrInvalidOriginal
	}
	return nil
}

func (a *OriginalAsset) ReplaceChecksum(checksum string) error {
	if checksum != a.Checksum {
		return ErrOriginalImmutable
	}
	return nil
}

func (a *OriginalAsset) ReplaceOriginalName(name string) error {
	if name != a.OriginalName {
		return ErrOriginalImmutable
	}
	return nil
}

type Supersession struct {
	PreviousAssetID string
	NewAssetID      string
}

func SupersedeOriginal(previous, replacement OriginalAsset) (Supersession, error) {
	if previous.Validate() != nil || replacement.Validate() != nil || previous.ID == replacement.ID {
		return Supersession{}, ErrOriginalImmutable
	}
	return Supersession{PreviousAssetID: previous.ID, NewAssetID: replacement.ID}, nil
}
