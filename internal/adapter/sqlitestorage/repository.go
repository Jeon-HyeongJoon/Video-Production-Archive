package sqlitestorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/storage"
)

type Repository struct {
	db *sql.DB
}

var _ storage.WorkspaceResolver = (*Repository)(nil)
var _ storage.AccessResolver = (*Repository)(nil)

func Open(path string) (*Repository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	repository := &Repository{db: db}
	if err := repository.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Close() error {
	return repository.db.Close()
}

func (repository *Repository) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS physical_devices (
			physical_device_id TEXT PRIMARY KEY
		)`,
		`CREATE TABLE IF NOT EXISTS partitions (
			partition_id TEXT PRIMARY KEY,
			physical_device_id TEXT NOT NULL REFERENCES physical_devices(physical_device_id)
		)`,
		`CREATE TABLE IF NOT EXISTS volumes (
			volume_id TEXT PRIMARY KEY,
			partition_id TEXT NOT NULL REFERENCES partitions(partition_id)
		)`,
		`CREATE TABLE IF NOT EXISTS storages (
			storage_id TEXT PRIMARY KEY,
			volume_id TEXT NOT NULL REFERENCES volumes(volume_id),
			root TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite storage repository: %w", err)
		}
	}
	return nil
}

func (repository *Repository) Register(ctx context.Context, topology storage.Topology) error {
	if err := topology.Validate(); err != nil {
		return err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT OR IGNORE INTO physical_devices(physical_device_id) VALUES (?)`, []any{topology.PhysicalDeviceID}},
		{`INSERT OR IGNORE INTO partitions(partition_id, physical_device_id) VALUES (?, ?)`, []any{topology.PartitionID, topology.PhysicalDeviceID}},
		{`INSERT OR IGNORE INTO volumes(volume_id, partition_id) VALUES (?, ?)`, []any{topology.VolumeID, topology.PartitionID}},
		{`INSERT INTO storages(storage_id, volume_id, root) VALUES (?, ?, ?)`, []any{topology.StorageID, topology.VolumeID, topology.Root}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("register storage topology: %w", err)
		}
	}
	return tx.Commit()
}

func (repository *Repository) ResolveWorkspaceDrive(ctx context.Context, storageID string) (storage.WorkspaceDrive, error) {
	access, err := repository.ResolveAccess(ctx, storageID)
	if errors.Is(err, storage.ErrStorageUnresolved) {
		return storage.WorkspaceDrive{}, storage.ErrWorkspaceDriveUnresolved
	}
	if err != nil {
		return storage.WorkspaceDrive{}, err
	}
	return storage.WorkspaceDrive{ID: access.WorkspaceDriveID}, nil
}

func (repository *Repository) ResolveAccess(ctx context.Context, storageID string) (storage.Access, error) {
	var access storage.Access
	err := repository.db.QueryRowContext(ctx, `
		SELECT partitions.physical_device_id
		     , storages.storage_id
		     , storages.root
		FROM storages
		JOIN volumes ON volumes.volume_id = storages.volume_id
		JOIN partitions ON partitions.partition_id = volumes.partition_id
		WHERE storages.storage_id = ?
	`, storageID).Scan(&access.WorkspaceDriveID, &access.StorageID, &access.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Access{}, storage.ErrStorageUnresolved
	}
	if err != nil {
		return storage.Access{}, err
	}
	if access.StorageID == "" || access.Root == "" || access.WorkspaceDriveID == "" {
		return storage.Access{}, storage.ErrStorageUnresolved
	}
	return access, nil
}
