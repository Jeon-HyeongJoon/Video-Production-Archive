package sqlitecommand

import (
	"context"
	"encoding/json"
	"time"
	"videoarchive/internal/application/systemstatus"
	"videoarchive/internal/core/execution"
)

// Only the small report summary or mutation timestamp crosses the status read boundary.
func (s *Store) DestinationEvidence(ctx context.Context) (map[string]systemstatus.DestinationEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
 json_extract(step.value,'$.execution_spec.parameters.archive_storage_id'),
 json_extract(step.value,'$.execution_spec.operation'),
 json_extract(step.value,'$.last_event_at'),
 json_extract(step.value,'$.output_record.outputs[0].catalog_verification')
 FROM aggregate_states a, json_each(CAST(a.state AS TEXT),'$.steps') step
 WHERE json_extract(step.value,'$.execution_spec.parameters.archive_storage_id') IS NOT NULL
 AND json_extract(step.value,'$.workflow.State')='COMPLETED'
 AND json_extract(step.value,'$.execution_spec.operation') IN (?,?,?,?,?,?)`, execution.OperationVerifyArchiveCatalog, execution.OperationWriteArchiveCatalog, execution.OperationReviseArchivePackage, execution.OperationOrganizeMedia, execution.OperationRebuildArchiveCatalog, execution.OperationReconcileArchivePublication)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := map[string]systemstatus.DestinationEvidence{}
	for rows.Next() {
		var id, at string
		var op execution.Operation
		var payload *string
		if err := rows.Scan(&id, &op, &at, &payload); err != nil {
			return nil, err
		}
		e := results[id]
		if op == execution.OperationVerifyArchiveCatalog {
			if payload == nil {
				continue
			}
			var v execution.CatalogVerification
			if err := json.Unmarshal([]byte(*payload), &v); err != nil {
				return nil, err
			}
			if v.ArchiveStorageID == id && (v.ObservedAt.After(e.Report.ObservedAt) || (v.ObservedAt.Equal(e.Report.ObservedAt) && v.ReportID > e.Report.ReportID)) {
				e.Report = v
			}
		} else {
			t, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return nil, err
			}
			if t.After(e.LastMutation) {
				e.LastMutation = t
			}
		}
		results[id] = e
	}
	return results, rows.Err()
}
