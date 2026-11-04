package archivepackage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/command"
)

var (
	ErrPackageNotFound = errors.New("archive package not found")
	ErrPackageConflict = errors.New("archive package conflicts with existing record")
	ErrInvalidPackage  = errors.New("archive package is invalid")
)

type Relationship struct {
	Type   string `json:"type"`
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
}

type FileRecord struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Checksum string `json:"sha256"`
}

type Package struct {
	ID            string                  `json:"package_id"`
	Snapshot      archive.ArchiveSnapshot `json:"snapshot"`
	Relationships []Relationship          `json:"relationships"`
	Events        []command.Event         `json:"events"`
	Files         []FileRecord            `json:"files"`
	Readme        string                  `json:"readme"`
}

type metadataDocument struct {
	ID            string                  `json:"package_id"`
	Snapshot      archive.ArchiveSnapshot `json:"snapshot"`
	Relationships []Relationship          `json:"relationships"`
}

type Repository struct {
	db *sql.DB
}

func OpenRepository(path string) (*Repository, error) {
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
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS archive_packages (
			package_id TEXT PRIMARY KEY,
			content_digest TEXT NOT NULL,
			readme TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS archive_original_assets (
			package_id TEXT NOT NULL REFERENCES archive_packages(package_id) ON DELETE CASCADE,
			asset_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY(package_id, asset_id)
		)`,
		`CREATE TABLE IF NOT EXISTS archive_artifacts (
			package_id TEXT NOT NULL REFERENCES archive_packages(package_id) ON DELETE CASCADE,
			artifact_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY(package_id, artifact_id)
		)`,
		`CREATE TABLE IF NOT EXISTS archive_relationships (
			package_id TEXT NOT NULL REFERENCES archive_packages(package_id) ON DELETE CASCADE,
			relation_type TEXT NOT NULL,
			from_id TEXT NOT NULL,
			to_id TEXT NOT NULL,
			PRIMARY KEY(package_id, relation_type, from_id, to_id)
		)`,
		`CREATE TABLE IF NOT EXISTS archive_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			package_id TEXT NOT NULL REFERENCES archive_packages(package_id) ON DELETE CASCADE,
			event_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			UNIQUE(package_id, event_id)
		)`,
		`CREATE TABLE IF NOT EXISTS archive_files (
			package_id TEXT NOT NULL REFERENCES archive_packages(package_id) ON DELETE CASCADE,
			path TEXT NOT NULL,
			size INTEGER NOT NULL,
			checksum TEXT NOT NULL,
			PRIMARY KEY(package_id, path)
		)`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize archive repository: %w", err)
		}
	}
	return nil
}

func (repository *Repository) Save(ctx context.Context, value Package) error {
	value, digest, err := normalizePackage(value)
	if err != nil {
		return err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT content_digest FROM archive_packages WHERE package_id = ?`, value.ID).Scan(&existingDigest)
	if err == nil {
		if existingDigest != digest {
			return ErrPackageConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO archive_packages(package_id, content_digest, readme) VALUES (?, ?, ?)`, value.ID, digest, value.Readme); err != nil {
		return err
	}
	for _, asset := range value.Snapshot.OriginalAssets {
		payload, err := json.Marshal(asset)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_original_assets(package_id, asset_id, payload) VALUES (?, ?, ?)
		`, value.ID, asset.ID, payload); err != nil {
			return err
		}
	}
	for _, artifact := range value.Snapshot.Artifacts {
		payload, err := json.Marshal(artifact)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_artifacts(package_id, artifact_id, payload) VALUES (?, ?, ?)
		`, value.ID, artifact.ID, payload); err != nil {
			return err
		}
	}
	for _, relationship := range value.Relationships {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_relationships(package_id, relation_type, from_id, to_id) VALUES (?, ?, ?, ?)
		`, value.ID, relationship.Type, relationship.FromID, relationship.ToID); err != nil {
			return err
		}
	}
	for _, event := range value.Events {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_events(package_id, event_id, payload) VALUES (?, ?, ?)
		`, value.ID, event.EventID, payload); err != nil {
			return err
		}
	}
	for _, file := range value.Files {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO archive_files(package_id, path, size, checksum) VALUES (?, ?, ?, ?)
		`, value.ID, file.Path, file.Size, file.Checksum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (repository *Repository) Load(ctx context.Context, packageID string) (Package, error) {
	var value Package
	value.ID = packageID
	if err := repository.db.QueryRowContext(ctx, `SELECT readme FROM archive_packages WHERE package_id = ?`, packageID).Scan(&value.Readme); errors.Is(err, sql.ErrNoRows) {
		return Package{}, ErrPackageNotFound
	} else if err != nil {
		return Package{}, err
	}

	assetRows, err := repository.db.QueryContext(ctx, `
		SELECT payload FROM archive_original_assets WHERE package_id = ? ORDER BY asset_id
	`, packageID)
	if err != nil {
		return Package{}, err
	}
	for assetRows.Next() {
		var payload []byte
		var asset archive.OriginalAsset
		if err := assetRows.Scan(&payload); err != nil {
			assetRows.Close()
			return Package{}, err
		}
		if err := json.Unmarshal(payload, &asset); err != nil {
			assetRows.Close()
			return Package{}, err
		}
		value.Snapshot.OriginalAssets = append(value.Snapshot.OriginalAssets, asset)
	}
	if err := closeRows(assetRows); err != nil {
		return Package{}, err
	}

	artifactRows, err := repository.db.QueryContext(ctx, `
		SELECT payload FROM archive_artifacts WHERE package_id = ? ORDER BY artifact_id
	`, packageID)
	if err != nil {
		return Package{}, err
	}
	for artifactRows.Next() {
		var payload []byte
		var artifact archive.Artifact
		if err := artifactRows.Scan(&payload); err != nil {
			artifactRows.Close()
			return Package{}, err
		}
		if err := json.Unmarshal(payload, &artifact); err != nil {
			artifactRows.Close()
			return Package{}, err
		}
		value.Snapshot.Artifacts = append(value.Snapshot.Artifacts, artifact)
	}
	if err := closeRows(artifactRows); err != nil {
		return Package{}, err
	}

	relationshipRows, err := repository.db.QueryContext(ctx, `
		SELECT relation_type, from_id, to_id FROM archive_relationships
		WHERE package_id = ? ORDER BY relation_type, from_id, to_id
	`, packageID)
	if err != nil {
		return Package{}, err
	}
	for relationshipRows.Next() {
		var relationship Relationship
		if err := relationshipRows.Scan(&relationship.Type, &relationship.FromID, &relationship.ToID); err != nil {
			relationshipRows.Close()
			return Package{}, err
		}
		value.Relationships = append(value.Relationships, relationship)
	}
	if err := closeRows(relationshipRows); err != nil {
		return Package{}, err
	}

	eventRows, err := repository.db.QueryContext(ctx, `
		SELECT payload FROM archive_events WHERE package_id = ? ORDER BY sequence
	`, packageID)
	if err != nil {
		return Package{}, err
	}
	for eventRows.Next() {
		var payload []byte
		var event command.Event
		if err := eventRows.Scan(&payload); err != nil {
			eventRows.Close()
			return Package{}, err
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			eventRows.Close()
			return Package{}, err
		}
		value.Events = append(value.Events, event)
	}
	if err := closeRows(eventRows); err != nil {
		return Package{}, err
	}

	fileRows, err := repository.db.QueryContext(ctx, `
		SELECT path, size, checksum FROM archive_files WHERE package_id = ? ORDER BY path
	`, packageID)
	if err != nil {
		return Package{}, err
	}
	for fileRows.Next() {
		var file FileRecord
		if err := fileRows.Scan(&file.Path, &file.Size, &file.Checksum); err != nil {
			fileRows.Close()
			return Package{}, err
		}
		value.Files = append(value.Files, file)
	}
	if err := closeRows(fileRows); err != nil {
		return Package{}, err
	}
	return value, nil
}

func closeRows(rows *sql.Rows) error {
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	return closeErr
}

func Export(ctx context.Context, repository *Repository, packageID, directory string) error {
	value, err := repository.Load(ctx, packageID)
	if err != nil {
		return err
	}
	if err := verifyPackageFiles(directory, value.Files); err != nil {
		return err
	}
	metadata, err := json.MarshalIndent(metadataDocument{ID: value.ID, Snapshot: value.Snapshot, Relationships: value.Relationships}, "", "  ")
	if err != nil {
		return err
	}
	metadata = append(metadata, '\n')
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0o600); err != nil {
		return err
	}
	if err := writeFilesCSV(filepath.Join(directory, "files.csv"), value.Files); err != nil {
		return err
	}
	if err := writeEvents(filepath.Join(directory, "events.jsonl"), value.Events); err != nil {
		return err
	}
	manifest, err := manifestFor(value.Files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest-sha256.txt"), []byte(manifest), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "README.md"), []byte(value.Readme), 0o600)
}

func Import(ctx context.Context, repository *Repository, directory string) error {
	metadataPayload, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata metadataDocument
	decoder := json.NewDecoder(strings.NewReader(string(metadataPayload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return ErrInvalidPackage
	}
	files, err := readFilesCSV(filepath.Join(directory, "files.csv"))
	if err != nil {
		return err
	}
	wantManifest, err := manifestFor(files)
	if err != nil {
		return err
	}
	storedManifest, err := os.ReadFile(filepath.Join(directory, "manifest-sha256.txt"))
	if err != nil || string(storedManifest) != wantManifest {
		return ErrInvalidPackage
	}
	if err := verifyPackageFiles(directory, files); err != nil {
		return err
	}
	events, err := readEvents(filepath.Join(directory, "events.jsonl"))
	if err != nil {
		return err
	}
	readme, err := os.ReadFile(filepath.Join(directory, "README.md"))
	if err != nil {
		return err
	}
	return repository.Save(ctx, Package{
		ID: metadata.ID, Snapshot: metadata.Snapshot, Relationships: metadata.Relationships,
		Events: events, Files: files, Readme: string(readme),
	})
}

func normalizePackage(value Package) (Package, string, error) {
	if value.ID == "" || value.Readme == "" {
		return Package{}, "", ErrInvalidPackage
	}
	if _, err := archive.ExportSnapshot(value.Snapshot); err != nil {
		return Package{}, "", ErrInvalidPackage
	}
	for _, relationship := range value.Relationships {
		if relationship.Type == "" || relationship.FromID == "" || relationship.ToID == "" {
			return Package{}, "", ErrInvalidPackage
		}
	}
	for _, event := range value.Events {
		if err := event.Validate(); err != nil {
			return Package{}, "", ErrInvalidPackage
		}
	}
	if _, err := manifestFor(value.Files); err != nil {
		return Package{}, "", ErrInvalidPackage
	}
	sort.Slice(value.Snapshot.OriginalAssets, func(i, j int) bool { return value.Snapshot.OriginalAssets[i].ID < value.Snapshot.OriginalAssets[j].ID })
	sort.Slice(value.Snapshot.Artifacts, func(i, j int) bool { return value.Snapshot.Artifacts[i].ID < value.Snapshot.Artifacts[j].ID })
	sort.Slice(value.Relationships, func(i, j int) bool {
		left := value.Relationships[i]
		right := value.Relationships[j]
		return left.Type+"\x00"+left.FromID+"\x00"+left.ToID < right.Type+"\x00"+right.FromID+"\x00"+right.ToID
	})
	sort.Slice(value.Files, func(i, j int) bool { return value.Files[i].Path < value.Files[j].Path })
	payload, err := json.Marshal(value)
	if err != nil {
		return Package{}, "", err
	}
	digest := sha256.Sum256(payload)
	return value, hex.EncodeToString(digest[:]), nil
}

func manifestFor(files []FileRecord) (string, error) {
	entries := make([]archive.ManifestEntry, 0, len(files))
	for _, file := range files {
		if file.Size < 0 {
			return "", ErrInvalidPackage
		}
		entries = append(entries, archive.ManifestEntry{Path: file.Path, Checksum: file.Checksum})
	}
	return archive.BuildManifest(entries)
}

func verifyPackageFiles(directory string, files []FileRecord) error {
	root, err := archive.OpenStorageRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, expected := range files {
		file, err := root.Open(expected.Path)
		if err != nil {
			return ErrInvalidPackage
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || size != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.Checksum {
			return ErrInvalidPackage
		}
	}
	return nil
}

func writeFilesCSV(path string, files []FileRecord) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if err := writer.Write([]string{"path", "size", "sha256"}); err != nil {
		file.Close()
		return err
	}
	for _, record := range files {
		if err := writer.Write([]string{record.Path, strconv.FormatInt(record.Size, 10), record.Checksum}); err != nil {
			file.Close()
			return err
		}
	}
	writer.Flush()
	writerErr := writer.Error()
	closeErr := file.Close()
	if writerErr != nil {
		return writerErr
	}
	return closeErr
}

func readFilesCSV(path string) ([]FileRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil || len(rows) == 0 || !reflect.DeepEqual(rows[0], []string{"path", "size", "sha256"}) {
		return nil, ErrInvalidPackage
	}
	result := make([]FileRecord, 0, len(rows)-1)
	for _, row := range rows[1:] {
		if len(row) != 3 {
			return nil, ErrInvalidPackage
		}
		size, err := strconv.ParseInt(row[1], 10, 64)
		if err != nil {
			return nil, ErrInvalidPackage
		}
		result = append(result, FileRecord{Path: row[0], Size: size, Checksum: row[2]})
	}
	return result, nil
}

func writeEvents(path string, events []command.Event) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			file.Close()
			return err
		}
	}
	return file.Close()
}

func readEvents(path string) ([]command.Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []command.Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event command.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Validate() != nil {
			return nil, ErrInvalidPackage
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
