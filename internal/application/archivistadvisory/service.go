package archivistadvisory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/application/archiveintakeworkspace"
	"videoarchive/internal/application/archivistcontrol"
	"videoarchive/internal/application/workdiagnostics"
	"videoarchive/internal/core/archivistruntime"
	"videoarchive/internal/core/vocabulary"
)

var ErrInvalidConfiguration = errors.New("archivist advisory configuration is invalid")

type SessionReader interface {
	Projection(context.Context) ([]archivistcontrol.SessionProjection, error)
}

type DriveReader interface {
	Projection(context.Context) ([]archiveintakeworkspace.DriveView, error)
}

type DiagnosticReader interface {
	Diagnose(context.Context, archivistruntime.WorkReference) (workdiagnostics.Snapshot, error)
}

type TurnWriter interface {
	HandleAdvisoryEvent(context.Context, archivistcontrol.AdvisoryEvent) (archivistcontrol.TurnRecord, bool, error)
}

type Service struct {
	sessions    SessionReader
	drives      DriveReader
	diagnostics DiagnosticReader
	turns       TurnWriter
}

func New(sessions SessionReader, drives DriveReader, diagnostics DiagnosticReader, turns TurnWriter) (*Service, error) {
	if sessions == nil || drives == nil || diagnostics == nil || turns == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Service{sessions: sessions, drives: drives, diagnostics: diagnostics, turns: turns}, nil
}

func (service *Service) Evaluate(ctx context.Context) error {
	sessions, err := service.sessions.Projection(ctx)
	if err != nil || len(sessions) == 0 {
		return err
	}
	ordered := append([]archivistcontrol.SessionProjection(nil), sessions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := latestActivity(ordered[i])
		right := latestActivity(ordered[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return ordered[i].Session.ID < ordered[j].Session.ID
	})
	var combined error
	for _, session := range ordered {
		reference := latestWorkReference(session)
		if reference == nil {
			continue
		}
		diagnostic, diagnoseErr := service.diagnostics.Diagnose(ctx, *reference)
		if diagnoseErr != nil {
			combined = errors.Join(combined, diagnoseErr)
			continue
		}
		if !diagnostic.ShouldReport {
			continue
		}
		_, _, queueErr := service.turns.HandleAdvisoryEvent(ctx, archivistcontrol.AdvisoryEvent{
			SessionID: session.Session.ID, Kind: archivistcontrol.AdvisoryWorkDiagnostic,
			TriggerID: diagnostic.Fingerprint, Work: reference,
			Prompt: "Inspect the current bound work with archive.work.diagnostics.get.v1. Report observed facts separately from hypotheses, explain why progress needs attention, and suggest the safest next action. Do not retry, cancel, classify, or start another stage automatically.",
		})
		if queueErr != nil {
			combined = errors.Join(combined, queueErr)
		}
	}
	latestSession := ordered[0].Session.ID
	drives, err := service.drives.Projection(ctx)
	if err != nil {
		return errors.Join(combined, err)
	}
	for _, drive := range drives {
		if !readySource(drive) {
			continue
		}
		triggerID := strings.Join([]string{
			drive.WorkspaceDriveID,
			uintString(drive.CandidateRevision),
			uintString(drive.FolderDiscoveryRevision),
		}, ":")
		_, _, queueErr := service.turns.HandleAdvisoryEvent(ctx, archivistcontrol.AdvisoryEvent{
			SessionID: latestSession, Kind: archivistcontrol.AdvisoryNewDrive, TriggerID: triggerID,
			Prompt: "A newly connected source drive has completed folder discovery. Use the current candidate tools to inspect source folders and eligible archive storage, prepare one explicit selection, and explain the expected work. If the selection is safe, call archive.run.start.v1 only to create a human-confirmed ActionCard; do not claim that work started before the card is executed.",
		})
		if queueErr != nil {
			combined = errors.Join(combined, queueErr)
		}
	}
	return combined
}

func latestActivity(session archivistcontrol.SessionProjection) time.Time {
	latest := session.CreatedAt
	for _, turn := range session.Turns {
		if turn.UpdatedAt.After(latest) {
			latest = turn.UpdatedAt
		}
	}
	return latest
}

func latestWorkReference(session archivistcontrol.SessionProjection) *archivistruntime.WorkReference {
	for index := len(session.Turns) - 1; index >= 0; index-- {
		if session.Turns[index].WorkReference == nil {
			continue
		}
		copy := *session.Turns[index].WorkReference
		return &copy
	}
	return nil
}

func readySource(drive archiveintakeworkspace.DriveView) bool {
	if drive.ConnectionState != vocabulary.VolumeConnected || drive.FolderDiscoveryState != vocabulary.FolderDiscoveryReady ||
		drive.CandidateRevision == 0 || drive.FolderDiscoveryRevision == 0 || drive.DiscoveredFolderCount == 0 ||
		(drive.State != "UNCLASSIFIED" && drive.State != "CONTENT_CHANGED") {
		return false
	}
	for _, storage := range drive.StorageCandidates {
		if storage.Role == vocabulary.StorageArchivePrimary && storage.Accessible && storage.StorageID != drive.SourceStorageID && storage.FreeBytes > 0 {
			return true
		}
	}
	return false
}

func uintString(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
