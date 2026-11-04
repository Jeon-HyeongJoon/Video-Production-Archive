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
  -run '^(TestArchiveOrganizationWorkspaceMatchesOperatingSystemGolden|TestArchiveWorkspaceStatusWithArchivistPaneMatchesOperatingSystemGolden|TestArchiveWorkspaceKeepsLayoutAcrossViewportWidths|TestDrivePreparationSelectsArchiveAndFolderScopeAndLocksRunningScope|TestArchivistPaneShowsRecallToolsSelectionAndDisablesBusySend|TestArchivistAddSettingsShowsOnlyNameHostDetectedCLIAndControlMode|TestWorkspaceSeparatesStoredAndCommittedArchiveRecords|TestWorkspaceShowsReservationAdjustedWorkerCapacity)$' \
  ./internal/adapter/httpcontrol | tee "${raw_log}"

go run ./cmd/testevidence \
  -input "${raw_log}" \
  -catalog requirements/catalog.json \
  -output "${evidence_dir}" \
  -run-id "${run_id}" \
  -revision "${revision}" \
  -repository archive

approved_goldens=(internal/adapter/httpcontrol/testdata/archive_workspace_*_*.golden.png)
if [ "${#approved_goldens[@]}" -ne 15 ]; then
  printf 'expected 15 operating-system golden files, found %s\n' "${#approved_goldens[@]}" >&2
  exit 1
fi
for approved in "${approved_goldens[@]}"; do
  cp "${approved}" "${image_dir}/$(basename "${approved}")"
done

(
  cd "${image_dir}"
  shasum -a 256 ./*.png > sha256.txt
)

printf '%s\n' "${evidence_dir}"
