package sqlitecatalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/catalog"
	"videoarchive/internal/core/intake"
	"videoarchive/internal/core/mediaanalysis"
)

var (
	ErrRecordNotFound    = errors.New("catalog record not found")
	ErrRecordConflict    = errors.New("catalog record conflicts with immutable record")
	ErrReferenceMismatch = errors.New("catalog record does not match its referenced content")
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS input_file_observations (
			observation_id TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS input_file_observations_checksum
			ON input_file_observations(checksum, observation_id)`,
		`CREATE TABLE IF NOT EXISTS original_assets (
			asset_id TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			registered_observation_id TEXT NOT NULL UNIQUE
				REFERENCES input_file_observations(observation_id),
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS original_assets_checksum
			ON original_assets(checksum, asset_id)`,
		`CREATE TABLE IF NOT EXISTS media_inspections (
			inspection_id TEXT PRIMARY KEY,
			original_asset_id TEXT NOT NULL REFERENCES original_assets(asset_id),
			original_checksum TEXT NOT NULL,
			inspected_at TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS media_inspections_original_time
			ON media_inspections(original_asset_id, inspected_at, inspection_id)`,
		`CREATE TABLE IF NOT EXISTS catalog_records (
			record_id TEXT PRIMARY KEY,
			original_asset_id TEXT NOT NULL REFERENCES original_assets(asset_id),
			revision INTEGER NOT NULL,
			title_sort TEXT NOT NULL,
			payload BLOB NOT NULL,
			UNIQUE(original_asset_id, revision)
		)`,
		`CREATE INDEX IF NOT EXISTS catalog_records_latest
			ON catalog_records(original_asset_id, revision DESC)`,
		`CREATE INDEX IF NOT EXISTS catalog_records_title
			ON catalog_records(title_sort, original_asset_id)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite catalog: %w", err)
		}
	}
	return nil
}

func (store *Store) AppendObservation(ctx context.Context, observation intake.InputFileObservation) error {
	normalized, err := intake.NewInputFileObservation(observation)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	return store.appendImmutable(ctx,
		`INSERT INTO input_file_observations(observation_id, checksum, payload) VALUES (?, ?, ?)`,
		`SELECT payload FROM input_file_observations WHERE observation_id = ?`,
		normalized.ID, payload, normalized.Checksum)
}

func (store *Store) Observation(ctx context.Context, observationID string) (intake.InputFileObservation, error) {
	var payload []byte
	err := store.db.QueryRowContext(ctx, `
		SELECT payload FROM input_file_observations WHERE observation_id = ?
	`, strings.TrimSpace(observationID)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return intake.InputFileObservation{}, ErrRecordNotFound
	}
	if err != nil {
		return intake.InputFileObservation{}, err
	}
	var observation intake.InputFileObservation
	if err := json.Unmarshal(payload, &observation); err != nil {
		return intake.InputFileObservation{}, err
	}
	return intake.NewInputFileObservation(observation)
}

func (store *Store) AppendOriginal(ctx context.Context, original asset.OriginalAsset) error {
	if err := original.Validate(); err != nil {
		return err
	}
	observation, err := store.Observation(ctx, original.RegisteredFromObservationID)
	if err != nil {
		return err
	}
	if original.Checksum != observation.Checksum || original.OriginalName != observation.OriginalName || original.SizeBytes != observation.SizeBytes {
		return ErrReferenceMismatch
	}
	payload, err := json.Marshal(original)
	if err != nil {
		return err
	}
	return store.appendImmutable(ctx,
		`INSERT INTO original_assets(asset_id, checksum, registered_observation_id, payload) VALUES (?, ?, ?, ?)`,
		`SELECT payload FROM original_assets WHERE asset_id = ?`,
		original.ID, payload, original.Checksum, original.RegisteredFromObservationID)
}

func (store *Store) FindOriginalsByChecksum(ctx context.Context, checksum string) ([]asset.OriginalAsset, error) {
	checksum = strings.ToLower(strings.TrimSpace(checksum))
	rows, err := store.db.QueryContext(ctx, `
		SELECT payload FROM original_assets WHERE checksum = ? ORDER BY asset_id
	`, checksum)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var originals []asset.OriginalAsset
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var original asset.OriginalAsset
		if err := json.Unmarshal(payload, &original); err != nil {
			return nil, err
		}
		if err := original.Validate(); err != nil {
			return nil, err
		}
		originals = append(originals, original)
	}
	return originals, rows.Err()
}

func (store *Store) AppendInspection(ctx context.Context, inspection mediaanalysis.MediaInspection) error {
	normalized, err := mediaanalysis.NewMediaInspection(inspection)
	if err != nil {
		return err
	}
	var originalChecksum string
	err = store.db.QueryRowContext(ctx, `
		SELECT checksum FROM original_assets WHERE asset_id = ?
	`, normalized.OriginalAssetID).Scan(&originalChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRecordNotFound
	}
	if err != nil {
		return err
	}
	if normalized.OriginalChecksum != originalChecksum {
		return ErrReferenceMismatch
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	return store.appendImmutable(ctx,
		`INSERT INTO media_inspections(inspection_id, original_asset_id, original_checksum, inspected_at, payload) VALUES (?, ?, ?, ?, ?)`,
		`SELECT payload FROM media_inspections WHERE inspection_id = ?`,
		normalized.ID, payload, normalized.OriginalAssetID, normalized.OriginalChecksum, normalized.InspectedAt.UTC().Format(time.RFC3339Nano))
}

func (store *Store) InspectionsByOriginal(ctx context.Context, originalAssetID string) ([]mediaanalysis.MediaInspection, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT payload FROM media_inspections
		WHERE original_asset_id = ? ORDER BY inspected_at, inspection_id
	`, strings.TrimSpace(originalAssetID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var inspections []mediaanalysis.MediaInspection
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var inspection mediaanalysis.MediaInspection
		if err := json.Unmarshal(payload, &inspection); err != nil {
			return nil, err
		}
		normalized, err := mediaanalysis.NewMediaInspection(inspection)
		if err != nil {
			return nil, err
		}
		inspections = append(inspections, normalized)
	}
	return inspections, rows.Err()
}

func (store *Store) AppendCatalogRecord(ctx context.Context, record catalog.CatalogRecord) error {
	normalized, err := catalog.NewRecord(record)
	if err != nil {
		return err
	}
	var exists int
	err = store.db.QueryRowContext(ctx, `SELECT 1 FROM original_assets WHERE asset_id = ?`, normalized.OriginalAssetID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRecordNotFound
	}
	if err != nil {
		return err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	var existing []byte
	err = store.db.QueryRowContext(ctx, `
		SELECT payload FROM catalog_records WHERE original_asset_id = ? AND revision = ?
	`, normalized.OriginalAssetID, normalized.Revision).Scan(&existing)
	if err == nil {
		if bytes.Equal(existing, payload) {
			return nil
		}
		return ErrRecordConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return store.appendImmutable(ctx,
		`INSERT INTO catalog_records(record_id, original_asset_id, revision, title_sort, payload) VALUES (?, ?, ?, ?, ?)`,
		`SELECT payload FROM catalog_records WHERE record_id = ?`,
		normalized.ID, payload, normalized.OriginalAssetID, normalized.Revision, strings.ToLower(normalized.Title))
}

func (store *Store) Search(ctx context.Context, query catalog.SearchQuery) ([]catalog.SearchResult, error) {
	normalizedQuery, err := catalog.NormalizeQuery(query)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
		SELECT catalog_records.payload, original_assets.payload, media_inspections.payload
		FROM catalog_records
		JOIN original_assets ON original_assets.asset_id = catalog_records.original_asset_id
		LEFT JOIN media_inspections ON media_inspections.inspection_id = (
			SELECT latest_inspection.inspection_id FROM media_inspections AS latest_inspection
			WHERE latest_inspection.original_asset_id = catalog_records.original_asset_id
			ORDER BY latest_inspection.inspected_at DESC, latest_inspection.inspection_id DESC LIMIT 1
		)
		WHERE catalog_records.revision = (
			SELECT MAX(latest.revision) FROM catalog_records AS latest
			WHERE latest.original_asset_id = catalog_records.original_asset_id
		)
		ORDER BY catalog_records.title_sort, catalog_records.original_asset_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []catalog.SearchResult
	for rows.Next() {
		var metadataPayload, originalPayload, inspectionPayload []byte
		if err := rows.Scan(&metadataPayload, &originalPayload, &inspectionPayload); err != nil {
			return nil, err
		}
		var metadata catalog.CatalogRecord
		var original asset.OriginalAsset
		if err := json.Unmarshal(metadataPayload, &metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(originalPayload, &original); err != nil {
			return nil, err
		}
		metadata, err = catalog.NewRecord(metadata)
		if err != nil || original.Validate() != nil {
			return nil, ErrRecordConflict
		}
		if !matchesText(metadata, normalizedQuery.Text) {
			continue
		}
		var inspection *mediaanalysis.MediaInspection
		if len(inspectionPayload) > 0 {
			var observed mediaanalysis.MediaInspection
			if err := json.Unmarshal(inspectionPayload, &observed); err != nil {
				return nil, err
			}
			normalized, err := mediaanalysis.NewMediaInspection(observed)
			if err != nil {
				return nil, err
			}
			inspection = &normalized
		}
		if !matchesInspection(inspection, normalizedQuery) {
			continue
		}
		results = append(results, catalog.SearchResult{Original: original, Metadata: metadata, Inspection: inspection})
	}
	return results, rows.Err()
}

func matchesText(record catalog.CatalogRecord, text string) bool {
	if text == "" {
		return true
	}
	haystack := strings.ToLower(record.Title + "\n" + record.Description + "\n" + strings.Join(record.Keywords, "\n"))
	return strings.Contains(haystack, text)
}

func matchesInspection(inspection *mediaanalysis.MediaInspection, query catalog.SearchQuery) bool {
	if query.VideoCodec == "" && query.MinDurationMillis == 0 && query.MaxDurationMillis == 0 {
		return true
	}
	if inspection == nil {
		return false
	}
	if query.VideoCodec != "" {
		matched := false
		for _, stream := range inspection.VideoStreams {
			if stream.Codec == query.VideoCodec {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if query.MinDurationMillis > 0 && inspection.DurationMillis < query.MinDurationMillis {
		return false
	}
	if query.MaxDurationMillis > 0 && inspection.DurationMillis > query.MaxDurationMillis {
		return false
	}
	return true
}

func (store *Store) appendImmutable(ctx context.Context, insert, selectPayload, id string, payload []byte, indexedValues ...any) error {
	arguments := []any{id}
	arguments = append(arguments, indexedValues...)
	arguments = append(arguments, payload)
	if _, err := store.db.ExecContext(ctx, insert, arguments...); err == nil {
		return nil
	} else {
		var existing []byte
		lookupErr := store.db.QueryRowContext(ctx, selectPayload, id).Scan(&existing)
		if lookupErr == nil {
			if bytes.Equal(existing, payload) {
				return nil
			}
			return ErrRecordConflict
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		return err
	}
}
