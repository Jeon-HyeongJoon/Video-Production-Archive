param(
    [Parameter(Mandatory = $true)]
    [string]$FFmpegDirectory,
    [string]$Revision = 'windows-acceptance-workspace'
)

$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$ffmpeg = Join-Path $FFmpegDirectory 'ffmpeg.exe'
$ffprobe = Join-Path $FFmpegDirectory 'ffprobe.exe'
if (-not (Test-Path -LiteralPath $ffmpeg -PathType Leaf) -or
    -not (Test-Path -LiteralPath $ffprobe -PathType Leaf)) {
    throw 'FFmpegDirectory must contain ffmpeg.exe and ffprobe.exe'
}

$runID = 'windows-archive-organization-' + [DateTimeOffset]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$evidenceDirectory = Join-Path $repository (Join-Path 'evidence' $runID)
$binaryDirectory = Join-Path $evidenceDirectory 'bin'
$rawLog = Join-Path $evidenceDirectory 'go-test.jsonl'
$media = Join-Path $binaryDirectory 'archive-media.exe'
New-Item -ItemType Directory -Force -Path $binaryDirectory | Out-Null

Push-Location $repository
try {
    & go build -o $media ./cmd/archive-media
    if ($LASTEXITCODE -ne 0) {
        throw "Archive Media build failed with exit code $LASTEXITCODE"
    }

    $env:ARCHIVE_WINDOWS_DRIVE_ACCEPTANCE = '1'
    $env:ARCHIVE_MEDIA_PATH = $media
    $env:ARCHIVE_FFMPEG_PATH = $ffmpeg
    $env:ARCHIVE_FFPROBE_PATH = $ffprobe

    $utf8WithoutBOM = [System.Text.UTF8Encoding]::new($false)
    $logWriter = [System.IO.StreamWriter]::new($rawLog, $false, $utf8WithoutBOM)
    try {
        & go test -json -count=1 `
            -run '^TestWindowsArchiveRunCompletesSourceSequenceAcrossWorkerRestart$' `
            ./internal/application/controlruntime 2>&1 | ForEach-Object {
                $line = $_.ToString()
                $logWriter.WriteLine($line)
                Write-Output $line
            }
        $testExitCode = $LASTEXITCODE
    } finally {
        $logWriter.Dispose()
    }
    if ($testExitCode -ne 0) {
        throw "Windows Archive Organization acceptance failed with exit code $testExitCode"
    }
    & go run ./cmd/testevidence `
        -input $rawLog `
        -output $evidenceDirectory `
        -run-id $runID `
        -revision $Revision `
        -repository 'Archive'
    if ($LASTEXITCODE -ne 0) {
        throw "Acceptance evidence normalization failed with exit code $LASTEXITCODE"
    }
    Write-Output $evidenceDirectory
} finally {
    Pop-Location
}
