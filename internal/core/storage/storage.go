package storage

import (
	"context"
	"errors"
)

var (
	ErrInvalidTopology          = errors.New("storage topology is incomplete")
	ErrWorkspaceDriveUnresolved = errors.New("workspace drive cannot be resolved")
)

// Topology records the stable identity chain used to resolve a Storage to the
// PhysicalDevice that owns its workspace. Mount paths are deliberately absent.
type Topology struct {
	PhysicalDeviceID string
	PartitionID      string
	VolumeID         string
	StorageID        string
	Root             string
}

func (topology Topology) Validate() error {
	if topology.PhysicalDeviceID == "" || topology.PartitionID == "" || topology.VolumeID == "" ||
		topology.StorageID == "" || topology.Root == "" {
		return ErrInvalidTopology
	}
	return nil
}

// WorkspaceDrive is a role of a PhysicalDevice, not a separate persisted
// device identity. ID is therefore always the resolved PhysicalDevice ID.
type WorkspaceDrive struct {
	ID string
}

type WorkspaceResolver interface {
	ResolveWorkspaceDrive(ctx context.Context, storageID string) (WorkspaceDrive, error)
}
