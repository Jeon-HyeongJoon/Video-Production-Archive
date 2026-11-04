[CmdletBinding()]
param(
    [ValidateSet("Plan", "Stage", "Inventory")]
    [string]$Mode = "Plan",
    [string]$Root = "C:\Archive",
    [string]$Revision = "",
    [string]$SourceDirectory = "",
    [string[]]$LegacyRoots = @("C:\Video-Production-Archive", "C:\Archive\deploy")
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Get-ArchiveRuntimeLayout {
    param([string]$RuntimeRoot)
    $resolvedRoot = [System.IO.Path]::GetFullPath($RuntimeRoot)
    [ordered]@{
        root = $resolvedRoot
        current = Join-Path $resolvedRoot "runtime\current"
        releases = Join-Path $resolvedRoot "runtime\releases"
        config = Join-Path $resolvedRoot "config"
        data = Join-Path $resolvedRoot "data"
        logs = Join-Path $resolvedRoot "logs"
        scratch = Join-Path $resolvedRoot "scratch"
        evidence = Join-Path $resolvedRoot "evidence"
        legacy = Join-Path $resolvedRoot "legacy"
    }
}

$layout = Get-ArchiveRuntimeLayout -RuntimeRoot $Root
if ($Mode -eq "Plan") {
    [pscustomobject]@{
        mode = $Mode
        layout = $layout
        legacy_roots = $LegacyRoots
        policy = "stage releases and inventory legacy paths; never delete ArchivePackage or catalog content"
    } | ConvertTo-Json -Depth 4
    exit 0
}

foreach ($directory in $layout.Values) {
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
}

if ($Mode -eq "Inventory") {
    $items = foreach ($legacyRoot in $LegacyRoots) {
        $fullPath = [System.IO.Path]::GetFullPath($legacyRoot)
        $exists = Test-Path -LiteralPath $fullPath
        $children = @()
        $lastWriteTimeUtc = $null
        if ($exists) {
            $item = Get-Item -LiteralPath $fullPath -Force
            $lastWriteTimeUtc = $item.LastWriteTimeUtc.ToString("o")
            $children = @(Get-ChildItem -LiteralPath $fullPath -Force -Directory -ErrorAction Stop |
                Sort-Object Name |
                Select-Object -ExpandProperty Name)
        }
        [pscustomobject]@{
            path = $fullPath
            exists = $exists
            last_write_time_utc = $lastWriteTimeUtc
            top_level_directories = $children
            action = "retain until service paths, configuration, database and package manifests are verified"
        }
    }
    $stamp = (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ")
    $target = Join-Path $layout.legacy "inventory-$stamp.json"
    [pscustomobject]@{
        recorded_at = (Get-Date).ToUniversalTime().ToString("o")
        runtime_root = $layout.root
        legacy = @($items)
    } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $target -Encoding utf8NoBOM
    Write-Output $target
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Revision) -or $Revision -notmatch '^[A-Za-z0-9._-]+$') {
    throw "Stage requires a safe -Revision value."
}
if ([string]::IsNullOrWhiteSpace($SourceDirectory) -or -not (Test-Path -LiteralPath $SourceDirectory -PathType Container)) {
    throw "Stage requires an existing -SourceDirectory."
}

$binaryNames = @("archive-control.exe", "archive-worker.exe", "archivist-host.exe")
$releaseDirectory = Join-Path $layout.releases $Revision
$stagingDirectory = "$releaseDirectory.partial"
if ((Test-Path -LiteralPath $releaseDirectory) -or (Test-Path -LiteralPath $stagingDirectory)) {
    throw "Release or partial release already exists: $releaseDirectory"
}
foreach ($binaryName in $binaryNames) {
    $source = Join-Path $SourceDirectory $binaryName
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "Missing release binary: $source"
    }
}
New-Item -ItemType Directory -Path $stagingDirectory | Out-Null

$files = foreach ($binaryName in $binaryNames) {
    $source = Join-Path $SourceDirectory $binaryName
    $target = Join-Path $stagingDirectory $binaryName
    Copy-Item -LiteralPath $source -Destination $target
    $sourceHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    $targetHash = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($sourceHash -ne $targetHash) {
        throw "Release copy verification failed: $binaryName"
    }
    [pscustomobject]@{ name = $binaryName; sha256 = $targetHash }
}

$stagedManifest = Join-Path $stagingDirectory "release.json"
[pscustomobject]@{
    revision = $Revision
    staged_at = (Get-Date).ToUniversalTime().ToString("o")
    files = @($files)
    activation = "pending Windows acceptance; services continue to use runtime/current"
} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $stagedManifest -Encoding utf8NoBOM
Move-Item -LiteralPath $stagingDirectory -Destination $releaseDirectory
$manifest = Join-Path $releaseDirectory "release.json"
Write-Output $manifest
