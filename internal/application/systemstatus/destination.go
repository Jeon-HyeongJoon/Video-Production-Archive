package systemstatus

import (
	"context"
	"sort"
	"time"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/storage"
	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/worker"
)

type DestinationEvidence struct {
	Report       execution.CatalogVerification
	LastMutation time.Time
}
type DestinationRecords interface {
	DestinationEvidence(context.Context) (map[string]DestinationEvidence, error)
}
type DestinationStorages interface {
	ListAccess(context.Context) ([]storage.Access, error)
}

func (s *Service) SetDestinationReaders(storages DestinationStorages, records DestinationRecords) {
	s.storages = storages
	s.destinations = records
}

func (s *Service) destinationCatalogs(ctx context.Context, workers []worker.Status) ([]execution.CatalogVerification, error) {
	if s.storages == nil || s.destinations == nil {
		return nil, nil
	}
	registered, err := s.storages.ListAccess(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := s.destinations.DestinationEvidence(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, a := range registered {
		if a.Role == vocabulary.StorageArchivePrimary {
			known[a.StorageID] = true
		}
	}
	online := map[string]bool{}
	for _, w := range workers {
		for _, v := range w.Metrics.AttachedVolumes {
			known[v.ArchiveStorageID] = true
		}
		if w.State == "OFFLINE" || w.State == "DISABLED" {
			continue
		}
		disconnected := map[string]bool{}
		for _, obs := range w.Metrics.StorageObservations {
			disconnected[obs.StorageID] = !obs.Connected
		}
		for id, available := range w.Metrics.AccessibleStorageIDs {
			if available && !disconnected[id] {
				online[id] = true
			}
		}
	}
	for id := range evidence {
		known[id] = true
	}
	rows := make([]execution.CatalogVerification, 0, len(known))
	for id := range known {
		e := evidence[id]
		v := e.Report
		switch {
		case v.ReportID == "":
			v = execution.CatalogVerification{ArchiveStorageID: id, State: "NOT_OBSERVED", NextCheck: "VERIFY_DESTINATION"}
		case !online[id]:
			v.State, v.NextCheck = "STALE", "RECONNECT_AND_VERIFY_DESTINATION"
		case !e.LastMutation.IsZero() && !e.LastMutation.Before(v.ObservedAt):
			v.State, v.NextCheck = "STALE", "VERIFY_AFTER_PUBLICATION"
		}
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ArchiveStorageID < rows[j].ArchiveStorageID })
	return rows, nil
}
