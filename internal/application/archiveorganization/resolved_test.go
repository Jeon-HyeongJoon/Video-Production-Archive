package archiveorganization

import (
	"context"
	"testing"
	"time"

	"videoarchive/internal/core/archiverun"
	"videoarchive/internal/core/command"
	"videoarchive/internal/core/execution"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/organization"
	"videoarchive/internal/core/vocabulary"
)

func TestResolvedFolderServiceCreatesEveryIndependentRenditionInFixedArchiveRunContext(t *testing.T) {
	bus := &resolvedFolderJobBus{}
	service, err := NewResolvedFolderService(bus)
	if err != nil {
		t.Fatal(err)
	}
	inspection := func(id string) organization.Input {
		return organization.Input{
			OriginalAssetID: id, RelativePath: "hold/" + id + ".mov",
			Inspection: mediaanalysis.MediaInspection{
				ID: "INSPECTION-" + id, OriginalAssetID: id,
				OriginalChecksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				DurationMillis:   60_000, VideoStreams: []mediaanalysis.VideoStream{{Codec: "h264", Width: 3840, Height: 2160, BitRate: 50_000_000}},
			},
		}
	}
	decision := organization.OrganizationDecision{
		ID: "DECISION-2", Sequence: 2, WorkspaceDriveID: "VOLUME-1", Folder: "hold", InputRevision: 7,
		Disposition: vocabulary.OrganizationRenderRecordings, ProfileReference: "archive-1080p-20mbps@v1",
		Inputs: []organization.Input{inspection("ASSET-A"), inspection("ASSET-B")}, DecidedAt: time.Now().UTC(),
	}
	archiveRun := archiverun.State{
		ID: "ARCHIVE-RUN-1", WorkerID: "WORKER-1", SourceStorageID: "SOURCE-1", SourceVolumeID: "VOLUME-1",
		SourcePhysicalDeviceID: "DISK-1", ArchiveStorageID: "ARCHIVE-1", FolderDiscoveryRevision: 7,
		FolderIDs: []string{"hold"},
	}
	jobID, err := service.StartResolvedFolder(context.Background(), "INTAKE-1", "CLASSIFY-1", archiveRun, decision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	specs, ok := bus.envelope.Parameters["step_execution_specs"].(map[string]execution.StepExecutionSpec)
	spec := specs["render"]
	storageIDs, _ := bus.envelope.Parameters["required_storage_ids"].([]string)
	deviceIDs, _ := bus.envelope.Parameters["physical_device_ids"].([]string)
	if !ok || jobID == "" || bus.envelope.TargetID != jobID || len(spec.Inputs) != 2 || len(spec.Outputs) != 2 ||
		len(storageIDs) != 2 || storageIDs[0] != "SOURCE-1" || storageIDs[1] != "ARCHIVE-1" ||
		len(deviceIDs) != 1 || deviceIDs[0] != "DISK-1" || spec.Parameters.ArchiveRunID != "ARCHIVE-RUN-1" ||
		spec.Parameters.OrganizationDecisionID != "DECISION-2" {
		t.Fatalf("resolved folder job lost fixed context or independent outputs: job=%s envelope=%+v spec=%+v", jobID, bus.envelope, spec)
	}
}

type resolvedFolderJobBus struct {
	envelope command.Envelope
}

func (bus *resolvedFolderJobBus) Execute(envelope command.Envelope) (command.Result, error) {
	bus.envelope = envelope
	return command.Result{TargetID: envelope.TargetID}, nil
}
