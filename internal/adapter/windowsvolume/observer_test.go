package windowsvolume

import (
	"context"
	"testing"
	"time"

	"videoarchive/internal/core/device"
)

// Requirement: DEV-007
func TestObserverReportsAttachedDataVolumesIncludingFixedUSBWithoutUsingMountAsIdentity(t *testing.T) {
	probe := &fixtureProbe{snapshots: [][]ProbeVolume{
		{
			{Serial: "USB-A-001", Model: "CAM DRIVE", PhysicalCapacityBytes: 4_000_000_000, CapacityBytes: 2_000_000_000, FreeBytes: 400_000, VolumeUniqueID: "VOL-A", FileSystem: "exFAT", Root: `E:\`, BusType: 7},
			{Serial: "SD-B-002", Model: "CARD", PhysicalCapacityBytes: 4_000_000_000, CapacityBytes: 2_000_000_000, FreeBytes: 500_000, VolumeUniqueID: "VOL-B", FileSystem: "NTFS", Root: `F:\`, BusType: 12, RemovableMedia: true},
			{Serial: "SYSTEM-001", Model: "NVME", PhysicalCapacityBytes: 4_000_000_000, CapacityBytes: 4_000_000_000, FreeBytes: 1_000_000, VolumeUniqueID: "VOL-C", FileSystem: "NTFS", Root: `C:\`, BusType: 17, SystemVolume: true},
		},
		{
			{Serial: "USB-A-001", Model: "CAM DRIVE", PhysicalCapacityBytes: 4_000_000_000, CapacityBytes: 2_000_000_000, FreeBytes: 390_000, VolumeUniqueID: "VOL-A", FileSystem: "exFAT", Root: `G:\`, BusType: 7},
			{Serial: "SYSTEM-001", Model: "NVME", PhysicalCapacityBytes: 4_000_000_000, CapacityBytes: 4_000_000_000, FreeBytes: 1_000_000, VolumeUniqueID: "VOL-C", FileSystem: "NTFS", Root: `C:\`, BusType: 17, SystemVolume: true},
		},
	}}
	observer, err := NewObserver(probe)
	if err != nil {
		t.Fatal(err)
	}
	first, err := observer.Poll(context.Background(), "WORKER-WIN-1", time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Kind != device.VolumeAttached || first[1].Kind != device.VolumeAttached ||
		first[0].PhysicalDeviceID == first[1].PhysicalDeviceID || first[0].VolumeID == first[1].VolumeID {
		t.Fatalf("attached data volumes were not uniquely observed: %+v", first)
	}
	var deviceAID, volumeAID, storageAID, archiveStorageAID string
	for _, event := range first {
		if event.Serial == "USB-A-001" {
			deviceAID, volumeAID, storageAID, archiveStorageAID = event.PhysicalDeviceID, event.VolumeID, event.StorageID, event.ArchiveStorageID
		}
	}
	if archiveStorageAID == "" || archiveStorageAID == storageAID || archiveStorageAID != device.AttachedArchiveStorageID(volumeAID) {
		t.Fatalf("attached volume did not receive distinct stable source and archive Storage identities: source=%s archive=%s", storageAID, archiveStorageAID)
	}
	second, err := observer.Poll(context.Background(), "WORKER-WIN-1", time.Date(2026, 10, 30, 15, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Kind != device.VolumeDetached || second[0].Serial != "SD-B-002" {
		t.Fatalf("detached volume was not reported once: %+v", second)
	}
	connected := observer.Connected()
	if len(connected) != 1 || connected[0].PhysicalDeviceID != deviceAID || connected[0].VolumeID != volumeAID ||
		connected[0].StorageID != storageAID || connected[0].ArchiveStorageID != archiveStorageAID || connected[0].Root != `G:\` {
		t.Fatalf("drive letter changed identity or root was not refreshed: %+v", connected)
	}
	for _, event := range append(first, second...) {
		if event.Root == `C:\` || event.PhysicalDeviceID == event.Root || event.VolumeID == event.Root ||
			event.StorageID == event.Root || event.ArchiveStorageID == event.Root {
			t.Fatalf("system volume or mount path became an identity: %+v", event)
		}
	}
}

type fixtureProbe struct {
	snapshots [][]ProbeVolume
}

func (probe *fixtureProbe) Volumes(context.Context) ([]ProbeVolume, error) {
	values := append([]ProbeVolume(nil), probe.snapshots[0]...)
	probe.snapshots = probe.snapshots[1:]
	return values, nil
}
