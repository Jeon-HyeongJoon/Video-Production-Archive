package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"videoarchive/internal/core/archive"
	coreintake "videoarchive/internal/core/intake"
	"videoarchive/internal/core/storage"
)

var ErrInputFileUnreadable = errors.New("input file cannot be inspected")

type Request struct {
	ObservationID string
	StorageID     string
	RelativePath  string
	WorkerID      string
}

type Inspector struct {
	storages storage.AccessResolver
	clock    func() time.Time
}

func NewInspector(storages storage.AccessResolver) (*Inspector, error) {
	if storages == nil {
		return nil, ErrInputFileUnreadable
	}
	return &Inspector{storages: storages, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func (inspector *Inspector) Inspect(ctx context.Context, request Request) (coreintake.InputFileObservation, error) {
	if strings.TrimSpace(request.ObservationID) == "" || strings.TrimSpace(request.StorageID) == "" || strings.TrimSpace(request.RelativePath) == "" || strings.TrimSpace(request.WorkerID) == "" {
		return coreintake.InputFileObservation{}, ErrInputFileUnreadable
	}
	access, err := inspector.storages.ResolveAccess(ctx, request.StorageID)
	if err != nil {
		return coreintake.InputFileObservation{}, err
	}
	root, err := archive.OpenStorageRoot(access.Root)
	if err != nil {
		return coreintake.InputFileObservation{}, err
	}
	defer root.Close()
	file, err := root.Open(request.RelativePath)
	if err != nil {
		return coreintake.InputFileObservation{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return coreintake.InputFileObservation{}, ErrInputFileUnreadable
	}

	digest := sha256.New()
	size, err := copyWithContext(ctx, digest, file)
	if err != nil {
		return coreintake.InputFileObservation{}, err
	}
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || size != info.Size() || after.ModTime() != info.ModTime() {
		return coreintake.InputFileObservation{}, ErrInputFileUnreadable
	}
	normalizedPath := path.Clean(strings.ReplaceAll(request.RelativePath, "\\", "/"))
	return coreintake.NewInputFileObservation(coreintake.InputFileObservation{
		ID:        request.ObservationID,
		StorageID: access.StorageID, WorkspaceDriveID: access.WorkspaceDriveID,
		RelativePath: normalizedPath, OriginalName: path.Base(normalizedPath),
		SizeBytes: size, Checksum: hex.EncodeToString(digest.Sum(nil)),
		ObservedBy: request.WorkerID, ObservedAt: inspector.clock(),
	})
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 1024*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			stored, writeErr := destination.Write(buffer[:count])
			written += int64(stored)
			if writeErr != nil {
				return written, writeErr
			}
			if stored != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
