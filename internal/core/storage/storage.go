package storage

import (
	"context"
	"errors"

	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidTopology          = errors.New("storage topology is incomplete")
	ErrStorageUnresolved        = errors.New("storage cannot be resolved")
	ErrWorkspaceDriveUnresolved = errors.New("workspace drive cannot be resolved")
)

// Topology records the stable identity chain used to resolve a Storage to its
// logical Volume workspace and the PhysicalDevice used only for I/O conflict
// protection. Mount paths are deliberately absent from either identity.
type Topology struct {
	PhysicalDeviceID string
	PartitionID      string
	VolumeID         string
	StorageID        string
	Role             vocabulary.StorageRole
	Root             string
}

func (topology Topology) Validate() error {
	if topology.PhysicalDeviceID == "" || topology.PartitionID == "" || topology.VolumeID == "" ||
		topology.StorageID == "" || !topology.Role.Valid() || topology.Root == "" {
		return ErrInvalidTopology
	}
	return nil
}

// WorkspaceDrive is the logical Volume selected by the operator. ID is always
// the resolved Volume ID, never a mount path or PhysicalDevice ID.
type WorkspaceDrive struct {
	ID string
}

type WorkspaceResolver interface {
	ResolveWorkspaceDrive(ctx context.Context, storageID string) (WorkspaceDrive, error)
}

// Access contains the boot-configured root and the PhysicalDevice identity
// required by a Worker to open and reserve one Storage.
type Access struct {
	StorageID        string
	Role             vocabulary.StorageRole
	Root             string
	WorkspaceDriveID string
	PhysicalDeviceID string
}

type AccessResolver interface {
	ResolveAccess(ctx context.Context, storageID string) (Access, error)
}
