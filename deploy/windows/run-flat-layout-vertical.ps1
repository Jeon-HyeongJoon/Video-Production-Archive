param(
    [Parameter(Mandatory = $true)]
    [string]$ArchiveMedia,
    [Parameter(Mandatory = $true)]
    [string]$SourceVideo,
    [Parameter(Mandatory = $true)]
    [string]$TestRoot,
    [string]$PackageID = ('VP-' + [DateTimeOffset]::UtcNow.ToString('yyyyMMdd') + '-WINVERTICAL')
)

$ErrorActionPreference = 'Stop'
$utf8 = [System.Text.UTF8Encoding]::new($false)
$archiveMediaPath = (Resolve-Path -LiteralPath $ArchiveMedia).Path
$sourceVideoPath = (Resolve-Path -LiteralPath $SourceVideo).Path
if (Test-Path -LiteralPath $TestRoot) {
    throw "TestRoot already exists: $TestRoot"
}

$sourceRoot = Join-Path $TestRoot 'source'
$archiveRoot = Join-Path $TestRoot 'archive'
$reportRoot = Join-Path $TestRoot 'report'
$specRoot = Join-Path $TestRoot 'spec'
@($sourceRoot, $archiveRoot, $reportRoot, $specRoot) | ForEach-Object {
    New-Item -ItemType Directory -Path $_ | Out-Null
}
$sourceFolder = Join-Path $sourceRoot 'recordings'
New-Item -ItemType Directory -Path $sourceFolder | Out-Null
$sourceTarget = Join-Path $sourceFolder 'windows-vertical.mp4'
Copy-Item -LiteralPath $sourceVideoPath -Destination $sourceTarget

$configurationPath = Join-Path $TestRoot 'media.yaml'
$configuration = @"
schema_version: 1
media:
  id: windows-flat-layout-vertical
  storage_roots:
    - storage_id: source
      root: '$sourceRoot'
    - storage_id: archive
      root: '$archiveRoot'
    - storage_id: report
      root: '$reportRoot'
  tools:
    ffmpeg: {executable: '$archiveMediaPath', version: vertical-fixture-v1}
    ffprobe: {executable: '$archiveMediaPath', version: vertical-fixture-v1}
"@
[System.IO.File]::WriteAllText($configurationPath, $configuration, $utf8)

$sourceChecksum = (Get-FileHash -LiteralPath $sourceTarget -Algorithm SHA256).Hash.ToLowerInvariant()
$sourceBytes = (Get-Item -LiteralPath $sourceTarget).Length
$archiveDate = $PackageID.Substring(3, 8)
$archiveDate = $archiveDate.Substring(0, 4) + '-' + $archiveDate.Substring(4, 2) + '-' + $archiveDate.Substring(6, 2)
$packageSuffix = $PackageID.Substring(12)
$resultFolder = $archiveDate + '_windows-vertical__' + $packageSuffix
$metadataRelative = $resultFolder + '/.archive-package.json'
$sourceReference = [ordered]@{
    original_asset_id = 'ASSET-WINDOWS-VERTICAL'
    original_name = 'windows-vertical.mp4'
    storage_id = 'source'
    relative_path = 'recordings/windows-vertical.mp4'
    checksum = $sourceChecksum
    duration_millis = 2000
    source_size_bytes = $sourceBytes
    source_container = 'mov,mp4,m4a,3gp,3g2,mj2'
    video_codec = 'h264'
    width = 320
    height = 180
    frame_rate_numerator = 25
    frame_rate_denominator = 1
    video_pixel_format = 'yuv420p'
    has_audio = $false
}

function Write-Specification([string]$Name, [System.Collections.IDictionary]$Specification) {
    $path = Join-Path $specRoot ($Name + '.json')
    [System.IO.File]::WriteAllText($path, ($Specification | ConvertTo-Json -Depth 12), $utf8)
    return $path
}

function Invoke-Media([string]$Command, [string]$SpecificationPath, [uint64]$Token) {
    $output = & $archiveMediaPath $Command --config $configurationPath --spec $SpecificationPath --execution-token $Token
    if ($LASTEXITCODE -ne 0) {
        throw "archive-media $Command failed with exit code $LASTEXITCODE"
    }
    $result = $output | ConvertFrom-Json
    if (-not $result.success) {
        throw "archive-media $Command returned an unsuccessful result"
    }
    return $result
}

$packageSpec = [ordered]@{
    version = 0
    digest = ''
    job_id = 'JOB-WINDOWS-VERTICAL'
    step_id = 'package'
    template_name = 'archive-intake-as-is'
    operation = 'ORGANIZE_MEDIA'
    inputs = @($sourceReference)
    outputs = @([ordered]@{
        id = $PackageID
        kind = 'ORGANIZED_MEDIA'
        storage_id = 'archive'
        relative_path = $metadataRelative
        original_asset_id = 'ASSET-WINDOWS-VERTICAL'
    })
    parameters = [ordered]@{
        organization_decision_id = 'DECISION-WINDOWS-VERTICAL'
        workspace_drive_id = 'VOLUME-WINDOWS-VERTICAL'
        archive_storage_id = 'archive'
        media_folder = 'recordings'
        input_revision = 1
        disposition = 'ORGANIZE_AS_IS'
        selected_profile_reference = 'archive-1080p-6mbps@v1'
        source_inputs = @($sourceReference)
    }
}
$packageResult = Invoke-Media 'package' (Write-Specification 'package' $packageSpec) 101
$metadataOutput = @($packageResult.outputs)[0]
$resultDirectory = Join-Path $archiveRoot $resultFolder
$initialChildren = @(Get-ChildItem -LiteralPath $resultDirectory -Force)
if ($initialChildren.Count -ne 3) {
    throw "compact initial result directory must contain exactly three files, got $($initialChildren.Count)"
}

$stagingRoot = Join-Path $archiveRoot 'staging\windows-vertical'
New-Item -ItemType Directory -Path $stagingRoot -Force | Out-Null
$transcriptRelative = 'staging/windows-vertical/transcript.json'
$transcriptTextRelative = 'staging/windows-vertical/transcript.txt'
$subtitleRelative = 'staging/windows-vertical/subtitle.srt'
[System.IO.File]::WriteAllText((Join-Path $archiveRoot $transcriptRelative), '{"segments":[{"text":"windows vertical"}]}', $utf8)
[System.IO.File]::WriteAllText((Join-Path $archiveRoot $transcriptTextRelative), "windows vertical`n", $utf8)
[System.IO.File]::WriteAllText((Join-Path $archiveRoot $subtitleRelative), "1`n00:00:00,000 --> 00:00:01,000`nwindows vertical`n", $utf8)
$transcriptChecksum = (Get-FileHash -LiteralPath (Join-Path $archiveRoot $transcriptRelative) -Algorithm SHA256).Hash.ToLowerInvariant()
$transcriptTextChecksum = (Get-FileHash -LiteralPath (Join-Path $archiveRoot $transcriptTextRelative) -Algorithm SHA256).Hash.ToLowerInvariant()
$subtitleChecksum = (Get-FileHash -LiteralPath (Join-Path $archiveRoot $subtitleRelative) -Algorithm SHA256).Hash.ToLowerInvariant()
$revisionSpec = [ordered]@{
    version = 0
    digest = ''
    job_id = 'JOB-WINDOWS-VERTICAL-STT'
    step_id = 'package-revision'
    template_name = 'archive-transcription'
    operation = 'REVISE_ARCHIVE_PACKAGE'
    inputs = @(
        [ordered]@{ original_asset_id = 'ASSET-WINDOWS-VERTICAL'; artifact_id = 'TRANSCRIPT-WINDOWS-VERTICAL'; storage_id = 'archive'; relative_path = $transcriptRelative; checksum = $transcriptChecksum; kind = 'TRANSCRIPT' },
        [ordered]@{ original_asset_id = 'ASSET-WINDOWS-VERTICAL'; artifact_id = 'TEXT-WINDOWS-VERTICAL'; storage_id = 'archive'; relative_path = $transcriptTextRelative; checksum = $transcriptTextChecksum; kind = 'TRANSCRIPT_TEXT' },
        [ordered]@{ original_asset_id = 'ASSET-WINDOWS-VERTICAL'; artifact_id = 'SUBTITLE-WINDOWS-VERTICAL'; storage_id = 'archive'; relative_path = $subtitleRelative; checksum = $subtitleChecksum; kind = 'SUBTITLE' }
    )
    outputs = @([ordered]@{
        id = $PackageID
        kind = 'ARCHIVE_PACKAGE_REVISION'
        storage_id = 'archive'
        relative_path = $metadataRelative
    })
    parameters = [ordered]@{
        organization_decision_id = 'DECISION-WINDOWS-VERTICAL'
        workspace_drive_id = 'VOLUME-WINDOWS-VERTICAL'
        archive_storage_id = 'archive'
        media_folder = 'recordings'
        input_revision = 1
        archive_package_id = $PackageID
        archive_package_revision = 1
        disposition = 'ORGANIZE_AS_IS'
        selected_profile_reference = 'archive-1080p-6mbps@v1'
        transcription_eligible = $true
        source_inputs = @($sourceReference)
    }
}
$revisionResult = Invoke-Media 'package-revise' (Write-Specification 'package-revision' $revisionSpec) 102
$metadataOutput = @($revisionResult.outputs)[0]

$catalogSpec = [ordered]@{
    version = 0
    digest = ''
    job_id = 'JOB-WINDOWS-VERTICAL'
    step_id = 'catalog'
    template_name = 'archive-intake-as-is'
    operation = 'WRITE_ARCHIVE_CATALOG'
    inputs = @([ordered]@{
        original_asset_id = 'ASSET-WINDOWS-VERTICAL'
        artifact_id = $PackageID
        storage_id = 'archive'
        relative_path = $metadataOutput.relative_path
        checksum = $metadataOutput.checksum
        kind = 'ARCHIVE_PACKAGE_REVISION'
    })
    outputs = @([ordered]@{
        id = 'CATALOG-WINDOWS-VERTICAL'
        kind = 'ARCHIVE_CATALOG'
        storage_id = 'archive'
        relative_path = 'archive-catalog.csv'
    })
    parameters = [ordered]@{
        organization_decision_id = 'DECISION-WINDOWS-VERTICAL'
        workspace_drive_id = 'VOLUME-WINDOWS-VERTICAL'
        archive_storage_id = 'archive'
        media_folder = 'recordings'
        input_revision = 1
        disposition = 'ORGANIZE_AS_IS'
        selected_profile_reference = 'archive-1080p-6mbps@v1'
        source_inputs = @($sourceReference)
    }
}
$catalogResult = Invoke-Media 'catalog' (Write-Specification 'catalog' $catalogSpec) 103

$verifySpec = [ordered]@{
    version = 0
    digest = ''
    job_id = 'JOB-WINDOWS-VERTICAL-VERIFY'
    step_id = 'verify'
    template_name = 'archive-catalog-maintenance'
    operation = 'VERIFY_ARCHIVE_CATALOG'
    inputs = @()
    outputs = @([ordered]@{
        id = 'CATALOG-VERIFY-WINDOWS-VERTICAL'
        kind = 'ARCHIVE_CATALOG_VERIFICATION'
        storage_id = 'report'
        relative_path = 'catalog-verify.json'
    })
    parameters = [ordered]@{
        organization_decision_id = ''
        workspace_drive_id = 'VOLUME-WINDOWS-VERTICAL'
        archive_storage_id = 'archive'
        input_revision = 1
    }
}
$verifyResult = Invoke-Media 'catalog-verify' (Write-Specification 'verify' $verifySpec) 104

$children = @(Get-ChildItem -LiteralPath $resultDirectory)
if (@($children | Where-Object { $_.PSIsContainer }).Count -ne 0) {
    throw 'flat result directory contains a child directory'
}
$metadata = Get-Content -LiteralPath (Join-Path $resultDirectory '.archive-package.json') -Raw | ConvertFrom-Json
$requiredFiles = @(
    '.archive-package.json', 'video-info.csv', 'transcript.txt', 'subtitle.srt', $metadata.video_file_name
)
foreach ($requiredFile in $requiredFiles) {
    if (-not (Test-Path -LiteralPath (Join-Path $resultDirectory $requiredFile) -PathType Leaf)) {
        throw "flat result file is missing: $requiredFile"
    }
}
if (@(Get-ChildItem -LiteralPath $resultDirectory -Filter '*.mp4' -File).Count -ne 1) {
    throw 'flat result directory must contain exactly one result video'
}
if ($children.Count -ne 5) {
    throw "compact STT result directory must contain exactly five files, got $($children.Count)"
}
if ($metadata.revision -ne 2 -or @($metadata.revisions).Count -ne 2 -or
    (Test-Path -LiteralPath (Join-Path $resultDirectory 'transcript.json'))) {
    throw 'compact recovery document did not preserve two revisions or exposed structured transcript'
}
$catalogRows = @(Import-Csv -LiteralPath (Join-Path $archiveRoot 'archive-catalog.csv'))
if ($catalogRows.Count -ne 1 -or $catalogRows[0].archive_package_id -ne $PackageID -or
    $catalogRows[0].result_folder -ne $resultFolder) {
    throw 'root archive catalog is not mapped one-to-one to the result directory'
}
$verification = Get-Content -LiteralPath (Join-Path $reportRoot 'catalog-verify.json') -Raw | ConvertFrom-Json
$badEntries = @($verification.entries | Where-Object { $_.state -ne 'CURRENT' })
if ($badEntries.Count -ne 0 -or @($verification.entries).Count -ne 1) {
    throw 'archive catalog verification did not report exactly one CURRENT package'
}

[ordered]@{
    success = $true
    test_root = $TestRoot
    source_bytes = $sourceBytes
    result_folder = $resultFolder
    result_file_count = $children.Count
    result_video = $metadata.video_file_name
    catalog_rows = $catalogRows.Count
    catalog_state = @($verification.entries)[0].state
    package_checksum = $metadataOutput.checksum
    catalog_checksum = @($catalogResult.outputs)[0].checksum
    verification_checksum = @($verifyResult.outputs)[0].checksum
} | ConvertTo-Json -Compress
