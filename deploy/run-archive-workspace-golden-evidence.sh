#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
started_at="$(date -u +%Y%m%dT%H%M%SZ)"
run_id="archive-workspace-golden-${started_at}"
evidence_dir="${repository_root}/evidence/${run_id}"
raw_log="${evidence_dir}/go-test.raw.jsonl"
image_dir="${evidence_dir}/artifacts"

mkdir -p "${image_dir}"
cd "${repository_root}"

revision="$(git rev-parse HEAD 2>/dev/null || printf 'working-tree')"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  revision="${revision}+dirty.$( { git diff HEAD; git ls-files --others --exclude-standard; } | shasum -a 256 | cut -c1-12)"
fi
export ARCHIVE_WORKSPACE_GOLDEN_EVIDENCE_DIR="${image_dir}"

go test -json -count=1 \
  -run '^(TestArchiveOrganizationWorkspaceMatchesOperatingSystemGolden|TestArchiveWorkspaceJobManagementWithArchivistPaneMatchesOperatingSystemGolden|TestWorkspaceRoutesJobListsAndCompletedResultsToPersistentJobDetail|TestArchiveWorkspaceKeepsLayoutAcrossViewportWidths|TestJobManagementSelectsArchiveDriveAndFolderScopeAndDriveDetailDoesNotCreateWork|TestDriveDetailShowsFolderStructureAndVideoRecordsWithoutWorkCreation|TestArchivistPaneShowsRecallToolsSelectionAndDisablesBusySend|TestArchivistAddSettingsShowsOnlyNameHostAndDetectedCLI|TestWorkspaceSeparatesStoredAndCommittedArchiveRecords|TestWorkspaceShowsReservationAdjustedWorkerCapacity|TestNewArchiveWorkShowsExpectedRelativeOutputLayoutBeforeStart)$' \
  ./internal/adapter/httpcontrol | tee "${raw_log}"

go run ./cmd/testevidence \
  -input "${raw_log}" \
  -catalog requirements/catalog.json \
  -output "${evidence_dir}" \
  -run-id "${run_id}" \
  -revision "${revision}" \
  -repository archive

testdata=internal/adapter/httpcontrol/testdata
for platform in darwin linux linux_arm64 windows; do
  for scene in pre-run stage-one stt-ready complete jobs job-detail; do
    cp "${testdata}/archive_workspace_${scene}_${platform}.golden.png" "${image_dir}/"
  done
done
cp "${testdata}/archive_workspace_output-preview_darwin.golden.png" "${image_dir}/"

(
  cd "${image_dir}"
  shasum -a 256 ./*.png > sha256.txt
)

printf '%s\n' "${evidence_dir}"
