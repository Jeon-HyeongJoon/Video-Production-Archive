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

$env:ARCHIVE_WINDOWS_ACCEPTANCE = '1'
$env:ARCHIVE_WINDOWS_MEDIA_ACCEPTANCE = '1'
$env:ARCHIVE_FFMPEG_PATH = $ffmpeg
$env:ARCHIVE_FFPROBE_PATH = $ffprobe

$runID = 'windows-media-' + [DateTimeOffset]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$evidenceDirectory = Join-Path $repository (Join-Path 'evidence' $runID)
$rawLog = Join-Path $evidenceDirectory 'go-test.jsonl'
New-Item -ItemType Directory -Force -Path $evidenceDirectory | Out-Null

Push-Location $repository
try {
    $utf8WithoutBOM = [System.Text.UTF8Encoding]::new($false)
    $logWriter = [System.IO.StreamWriter]::new($rawLog, $false, $utf8WithoutBOM)
    try {
        & go test -json -count=1 `
            ./internal/core/archive `
            ./internal/adapter/ffprobe `
            ./internal/adapter/ffmpeg 2>&1 | ForEach-Object {
                $line = $_.ToString()
                $logWriter.WriteLine($line)
                Write-Output $line
            }
        $testExitCode = $LASTEXITCODE
    } finally {
        $logWriter.Dispose()
    }
    if ($testExitCode -ne 0) {
        throw "Windows media acceptance failed with exit code $testExitCode"
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
