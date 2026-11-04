//go:build windows

package windowsvolume

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlStorageQueryProperty     = 0x002d1400
	ioctlVolumeGetDiskExtents     = 0x00560000
	ioctlDiskGetLengthInformation = 0x0007405c
	storageDeviceProperty         = 0
	propertyStandardQuery         = 0
)

type nativeProbe struct{}

func NewSystemObserver() (*Observer, error) {
	return NewObserver(nativeProbe{})
}

func (nativeProbe) Volumes(ctx context.Context) ([]ProbeVolume, error) {
	roots, err := logicalDriveRoots()
	if err != nil {
		return nil, err
	}
	windowsDirectory, err := windows.GetWindowsDirectory()
	if err != nil || len(windowsDirectory) < 2 {
		return nil, ErrObservationFailed
	}
	systemRoot := strings.ToUpper(windowsDirectory[:2]) + `\`
	volumes := make([]ProbeVolume, 0, len(roots))
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rootPointer, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		driveType := windows.GetDriveType(rootPointer)
		if driveType != windows.DRIVE_FIXED && driveType != windows.DRIVE_REMOVABLE {
			continue
		}
		if strings.EqualFold(root, systemRoot) {
			continue
		}
		descriptor, err := queryStorageDescriptor(root)
		if err != nil || !attachedDataBus(descriptor.busType, descriptor.removable) {
			continue
		}
		volume, err := volumeMetadata(root)
		if err != nil {
			continue
		}
		if descriptor.capacity == 0 {
			descriptor.capacity = volume.CapacityBytes
		}
		serial := descriptor.serial
		if serial == "" {
			serial = volume.VolumeUniqueID
		}
		model := strings.TrimSpace(strings.Join([]string{descriptor.vendor, descriptor.product}, " "))
		if model == "" {
			model = "ATTACHED DATA DEVICE"
		}
		volume.Serial = serial
		volume.Model = model
		volume.PhysicalCapacityBytes = descriptor.capacity
		volume.BusType = descriptor.busType
		volume.RemovableMedia = descriptor.removable || driveType == windows.DRIVE_REMOVABLE
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

type storageDescriptor struct {
	vendor    string
	product   string
	serial    string
	busType   uint32
	removable bool
	capacity  uint64
}

func queryStorageDescriptor(root string) (storageDescriptor, error) {
	devicePath, err := windows.UTF16PtrFromString(`\\.\` + root[:2])
	if err != nil {
		return storageDescriptor{}, err
	}
	handle, err := windows.CreateFile(
		devicePath,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return storageDescriptor{}, err
	}
	defer windows.CloseHandle(handle)
	diskNumber, err := volumeDiskNumber(handle)
	if err != nil {
		return descriptorForHandle(handle)
	}
	physicalPath, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, diskNumber))
	if err != nil {
		return descriptorForHandle(handle)
	}
	physicalHandle, err := windows.CreateFile(
		physicalPath,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return descriptorForHandle(handle)
	}
	defer windows.CloseHandle(physicalHandle)
	descriptor, err := descriptorForHandle(physicalHandle)
	if err != nil {
		return descriptorForHandle(handle)
	}
	descriptor.capacity, err = physicalLength(physicalHandle)
	if err != nil || descriptor.capacity == 0 {
		return descriptorForHandle(handle)
	}
	return descriptor, nil
}

func descriptorForHandle(handle windows.Handle) (storageDescriptor, error) {
	query := make([]byte, 12)
	binary.LittleEndian.PutUint32(query[0:4], storageDeviceProperty)
	binary.LittleEndian.PutUint32(query[4:8], propertyStandardQuery)
	output := make([]byte, 4096)
	var returned uint32
	if err := windows.DeviceIoControl(
		handle,
		ioctlStorageQueryProperty,
		&query[0],
		uint32(len(query)),
		&output[0],
		uint32(len(output)),
		&returned,
		nil,
	); err != nil {
		return storageDescriptor{}, err
	}
	if returned < 36 {
		return storageDescriptor{}, ErrObservationFailed
	}
	output = output[:returned]
	return storageDescriptor{
		vendor:    descriptorString(output, binary.LittleEndian.Uint32(output[12:16])),
		product:   descriptorString(output, binary.LittleEndian.Uint32(output[16:20])),
		serial:    descriptorString(output, binary.LittleEndian.Uint32(output[24:28])),
		busType:   binary.LittleEndian.Uint32(output[28:32]),
		removable: output[10] != 0,
	}, nil
}

type diskExtent struct {
	DiskNumber     uint32
	_              uint32
	StartingOffset int64
	ExtentLength   int64
}

type volumeDiskExtents struct {
	NumberOfDiskExtents uint32
	_                   uint32
	Extents             [1]diskExtent
}

func volumeDiskNumber(handle windows.Handle) (uint32, error) {
	var extents volumeDiskExtents
	var returned uint32
	if err := windows.DeviceIoControl(
		handle,
		ioctlVolumeGetDiskExtents,
		nil,
		0,
		(*byte)(unsafe.Pointer(&extents)),
		uint32(unsafe.Sizeof(extents)),
		&returned,
		nil,
	); err != nil {
		return 0, err
	}
	if extents.NumberOfDiskExtents != 1 || returned < uint32(unsafe.Offsetof(extents.Extents))+uint32(unsafe.Sizeof(extents.Extents[0])) {
		return 0, ErrObservationFailed
	}
	return extents.Extents[0].DiskNumber, nil
}

func physicalLength(handle windows.Handle) (uint64, error) {
	var length int64
	var returned uint32
	if err := windows.DeviceIoControl(
		handle,
		ioctlDiskGetLengthInformation,
		nil,
		0,
		(*byte)(unsafe.Pointer(&length)),
		uint32(unsafe.Sizeof(length)),
		&returned,
		nil,
	); err != nil {
		return 0, err
	}
	if returned < uint32(unsafe.Sizeof(length)) || length <= 0 {
		return 0, ErrObservationFailed
	}
	return uint64(length), nil
}

func descriptorString(payload []byte, offset uint32) string {
	if offset == 0 || offset >= uint32(len(payload)) {
		return ""
	}
	end := offset
	for end < uint32(len(payload)) && payload[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(payload[offset:end]))
}

func volumeMetadata(root string) (ProbeVolume, error) {
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return ProbeVolume{}, err
	}
	var available, capacity, free uint64
	if err := windows.GetDiskFreeSpaceEx(rootPointer, &available, &capacity, &free); err != nil || capacity == 0 {
		return ProbeVolume{}, errors.Join(ErrObservationFailed, err)
	}
	labelBuffer := make([]uint16, windows.MAX_PATH+1)
	fileSystemBuffer := make([]uint16, windows.MAX_PATH+1)
	var serial, maximumComponentLength, flags uint32
	if err := windows.GetVolumeInformation(
		rootPointer,
		&labelBuffer[0],
		uint32(len(labelBuffer)),
		&serial,
		&maximumComponentLength,
		&flags,
		&fileSystemBuffer[0],
		uint32(len(fileSystemBuffer)),
	); err != nil {
		return ProbeVolume{}, errors.Join(ErrObservationFailed, err)
	}
	volumeNameBuffer := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeNameForVolumeMountPoint(rootPointer, &volumeNameBuffer[0], uint32(len(volumeNameBuffer))); err != nil {
		return ProbeVolume{}, errors.Join(ErrObservationFailed, err)
	}
	volumeUniqueID := windows.UTF16ToString(volumeNameBuffer)
	if volumeUniqueID == "" {
		volumeUniqueID = fmt.Sprintf("%08X", serial)
	}
	return ProbeVolume{
		CapacityBytes:  capacity,
		FreeBytes:      free,
		VolumeUniqueID: volumeUniqueID,
		FileSystem:     windows.UTF16ToString(fileSystemBuffer),
		Label:          windows.UTF16ToString(labelBuffer),
		Root:           root,
	}, nil
}

func logicalDriveRoots() ([]string, error) {
	required, err := windows.GetLogicalDriveStrings(0, nil)
	if err != nil || required == 0 {
		return nil, errors.Join(ErrObservationFailed, err)
	}
	buffer := make([]uint16, required+1)
	written, err := windows.GetLogicalDriveStrings(uint32(len(buffer)), &buffer[0])
	if err != nil || written == 0 || written > uint32(len(buffer)) {
		return nil, errors.Join(ErrObservationFailed, err)
	}
	values := make([]string, 0)
	for start := 0; start < int(written); {
		end := start
		for end < int(written) && buffer[end] != 0 {
			end++
		}
		if end == start {
			break
		}
		value := windows.UTF16ToString(buffer[start:end])
		if validDriveRoot(value) {
			values = append(values, strings.ToUpper(value[:1])+`:\`)
		}
		start = end + 1
	}
	return values, nil
}
