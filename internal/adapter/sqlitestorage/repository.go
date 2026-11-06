package sqlitestorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"videoarchive/internal/adapter/sqlitebase"
	"videoarchive/internal/core/storage"
)

type Repository struct {
	db      *sql.DB
	release func() error
}

var ErrTopologyConflict = errors.New("storage topology conflicts with an existing definition")

var _ storage.WorkspaceResolver = (*Repository)(nil)
var _ storage.AccessResolver = (*Repository)(nil)

func Open(path string) (*Repository, error) {
	db, release, err := sqlitebase.Acquire(path)
	if err != nil {
		return nil, err
	}
	repository := &Repository{db: db, release: release}
	if err := repository.migrate(context.Background()); err != nil {
		release()
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Close() error {
	return repository.release()
}

func (repository *Repository) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
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
			role TEXT NOT NULL,
			root TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite storage repository: %w", err)
		}
	}
	return repository.ensureStorageRoleColumn(ctx)
}

func (repository *Repository) ensureStorageRoleColumn(ctx context.Context) error {
	rows, err := repository.db.QueryContext(ctx, `PRAGMA table_info(storages)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite storage schema: %w", err)
	}
	hasRole := false
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&id, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("inspect sqlite storage schema: %w", err)
		}
		if name == "role" {
			hasRole = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect sqlite storage schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect sqlite storage schema: %w", err)
	}
	if hasRole {
		return nil
	}
	if _, err := repository.db.ExecContext(ctx, `ALTER TABLE storages ADD COLUMN role TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate sqlite storage schema: %w", err)
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

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO physical_devices(physical_device_id) VALUES (?)`, topology.PhysicalDeviceID); err != nil {
		return fmt.Errorf("register storage topology: %w", err)
	}
	if err := ensureParent(ctx, tx,
		`INSERT OR IGNORE INTO partitions(partition_id, physical_device_id) VALUES (?, ?)`,
		`SELECT physical_device_id FROM partitions WHERE partition_id = ?`,
		topology.PartitionID, topology.PhysicalDeviceID,
	); err != nil {
		return err
	}
	if err := ensureParent(ctx, tx,
		`INSERT OR IGNORE INTO volumes(volume_id, partition_id) VALUES (?, ?)`,
		`SELECT partition_id FROM volumes WHERE volume_id = ?`,
		topology.VolumeID, topology.PartitionID,
	); err != nil {
		return err
	}
	var existingVolume, existingRole, existingRoot string
	err = tx.QueryRowContext(ctx, `SELECT volume_id, role, root FROM storages WHERE storage_id = ?`, topology.StorageID).Scan(&existingVolume, &existingRole, &existingRoot)
	if err == nil {
		if existingVolume != topology.VolumeID || existingRoot != topology.Root ||
			(existingRole != "" && existingRole != string(topology.Role)) {
			return ErrTopologyConflict
		}
		if existingRole == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE storages SET role = ? WHERE storage_id = ?`, topology.Role, topology.StorageID); err != nil {
				return fmt.Errorf("register storage topology: %w", err)
			}
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO storages(storage_id, volume_id, role, root) VALUES (?, ?, ?, ?)`, topology.StorageID, topology.VolumeID, topology.Role, topology.Root); err != nil {
		return fmt.Errorf("register storage topology: %w", err)
	}
	return tx.Commit()
}

func ensureParent(ctx context.Context, tx *sql.Tx, insert, selectParent, id, parentID string) error {
	if _, err := tx.ExecContext(ctx, insert, id, parentID); err != nil {
		return fmt.Errorf("register storage topology: %w", err)
	}
	var existingParent string
	if err := tx.QueryRowContext(ctx, selectParent, id).Scan(&existingParent); err != nil {
		return fmt.Errorf("register storage topology: %w", err)
	}
	if existingParent != parentID {
		return ErrTopologyConflict
	}
	return nil
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
		SELECT volumes.volume_id
		     , partitions.physical_device_id
		     , storages.storage_id
		     , storages.role
		     , storages.root
		FROM storages
		JOIN volumes ON volumes.volume_id = storages.volume_id
		JOIN partitions ON partitions.partition_id = volumes.partition_id
		WHERE storages.storage_id = ?
	`, storageID).Scan(&access.WorkspaceDriveID, &access.PhysicalDeviceID, &access.StorageID, &access.Role, &access.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Access{}, storage.ErrStorageUnresolved
	}
	if err != nil {
		return storage.Access{}, err
	}
	if access.StorageID == "" || !access.Role.Valid() || access.Root == "" || access.WorkspaceDriveID == "" || access.PhysicalDeviceID == "" {
		return storage.Access{}, storage.ErrStorageUnresolved
	}
	return access, nil
}

func (repository *Repository) ListAccess(ctx context.Context) ([]storage.Access, error) {
	rows, err := repository.db.QueryContext(ctx, `
		SELECT volumes.volume_id, partitions.physical_device_id, storages.storage_id, storages.role, storages.root
		FROM storages
		JOIN volumes ON volumes.volume_id = storages.volume_id
		JOIN partitions ON partitions.partition_id = volumes.partition_id
		ORDER BY storages.storage_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storage.Access
	for rows.Next() {
		var access storage.Access
		if err := rows.Scan(&access.WorkspaceDriveID, &access.PhysicalDeviceID, &access.StorageID, &access.Role, &access.Root); err != nil {
			return nil, err
		}
		if access.StorageID == "" || !access.Role.Valid() || access.Root == "" || access.WorkspaceDriveID == "" || access.PhysicalDeviceID == "" {
			return nil, storage.ErrStorageUnresolved
		}
		result = append(result, access)
	}
	return result, rows.Err()
}
