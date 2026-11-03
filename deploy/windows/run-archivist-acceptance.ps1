param(
    [string]$Revision = 'windows-acceptance-workspace'
)

$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$runID = 'windows-archivist-' + [DateTimeOffset]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$evidenceDirectory = Join-Path $repository (Join-Path 'evidence' $runID)
$binaryDirectory = Join-Path $evidenceDirectory 'bin'
$rawLog = Join-Path $evidenceDirectory 'go-test.jsonl'
$archivistHost = Join-Path $binaryDirectory 'archivist-host.exe'
$archivistFixture = Join-Path $binaryDirectory 'archivist-fixture.exe'
New-Item -ItemType Directory -Force -Path $binaryDirectory | Out-Null

Push-Location $repository
try {
    & go build -o $archivistHost ./cmd/archivist-host
    if ($LASTEXITCODE -ne 0) {
        throw "Archivist Host build failed with exit code $LASTEXITCODE"
    }
    & go build -o $archivistFixture ./cmd/archivist-fixture
    if ($LASTEXITCODE -ne 0) {
        throw "Archivist fixture build failed with exit code $LASTEXITCODE"
    }

    $env:ARCHIVE_WINDOWS_DPAPI_ACCEPTANCE = '1'
    $env:ARCHIVE_WINDOWS_ARCHIVIST_ACCEPTANCE = '1'
    $env:ARCHIVE_ARCHIVIST_HOST_PATH = $archivistHost
    $env:ARCHIVE_ARCHIVIST_FIXTURE_PATH = $archivistFixture

    $utf8WithoutBOM = [System.Text.UTF8Encoding]::new($false)
    $logWriter = [System.IO.StreamWriter]::new($rawLog, $false, $utf8WithoutBOM)
    try {
        & go test -json -count=1 `
            -run '^(TestWindowsDPAPISecretSurvivesProcessBoundaryWithoutPlaintextAtRest|TestWindowsArchivistHostResumesCollaborationTurnWithoutDuplicateEffects)$' `
            ./internal/adapter/ossecret `
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
        throw "Windows Archivist acceptance failed with exit code $testExitCode"
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
