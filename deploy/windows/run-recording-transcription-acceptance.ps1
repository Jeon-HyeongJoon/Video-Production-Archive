param(
    [Parameter(Mandatory = $true)]
    [string]$FFmpegDirectory,
    [Parameter(Mandatory = $true)]
    [string]$WhisperExecutable,
    [Parameter(Mandatory = $true)]
    [string]$WhisperModel,
    [string]$Revision = 'windows-acceptance-workspace'
)

$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$ffmpeg = Join-Path $FFmpegDirectory 'ffmpeg.exe'
$ffprobe = Join-Path $FFmpegDirectory 'ffprobe.exe'
foreach ($requiredFile in @($ffmpeg, $ffprobe, $WhisperExecutable, $WhisperModel)) {
    if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
        throw "Required acceptance file is unavailable: $requiredFile"
    }
}

$runID = 'windows-recording-transcription-' + [DateTimeOffset]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$evidenceDirectory = Join-Path $repository (Join-Path 'evidence' $runID)
$binaryDirectory = Join-Path $evidenceDirectory 'bin'
$fixtureDirectory = Join-Path $evidenceDirectory 'fixture'
$rawLog = Join-Path $evidenceDirectory 'go-test.jsonl'
$media = Join-Path $binaryDirectory 'archive-media.exe'
$koreanAudio = Join-Path $fixtureDirectory 'synthetic-ko.wav'
New-Item -ItemType Directory -Force -Path $binaryDirectory | Out-Null
New-Item -ItemType Directory -Force -Path $fixtureDirectory | Out-Null

$referenceBase64 = '7Jik64qYIOyYgeyDgSDsoJzsnpEg7ZaJ7IKs7JeQ7ISc64qUIOy0rOyYge2VnCDrhbntmZQg7JiB7IOB7J2EIOygleumrO2VmOqzoCDsnpDrp4nsnYQg66eM65Ot64uI64ukLg=='
$reference = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($referenceBase64))
Add-Type -AssemblyName System.Speech
$synthesizer = New-Object System.Speech.Synthesis.SpeechSynthesizer
try {
    $voice = $synthesizer.GetInstalledVoices() |
        Where-Object { $_.Enabled -and $_.VoiceInfo.Culture.Name -eq 'ko-KR' } |
        Select-Object -First 1
    if ($null -eq $voice) {
        throw 'Korean speech synthesis voice is unavailable'
    }
    $synthesizer.SelectVoice($voice.VoiceInfo.Name)
    $synthesizer.SetOutputToWaveFile($koreanAudio)
    $synthesizer.Speak($reference)
}
finally {
    $synthesizer.Dispose()
}

Push-Location $repository
try {
    & go build -o $media ./cmd/archive-media
    if ($LASTEXITCODE -ne 0) {
        throw "Archive Media build failed with exit code $LASTEXITCODE"
    }

    $env:ARCHIVE_WINDOWS_RECORDING_ACCEPTANCE = '1'
    $env:ARCHIVE_MEDIA_PATH = $media
    $env:ARCHIVE_FFMPEG_PATH = $ffmpeg
    $env:ARCHIVE_FFPROBE_PATH = $ffprobe
    $env:ARCHIVE_WHISPER_PATH = $WhisperExecutable
    $env:ARCHIVE_WHISPER_MODEL_PATH = $WhisperModel
    $env:ARCHIVE_KOREAN_AUDIO_PATH = $koreanAudio

    $utf8WithoutBOM = [System.Text.UTF8Encoding]::new($false)
    $logWriter = [System.IO.StreamWriter]::new($rawLog, $false, $utf8WithoutBOM)
    try {
        & go test -json -count=1 `
            -run '^TestWindowsRecordingCompletesRenderingTranscriptionAndOrganization$' `
            ./internal/application/controlruntime 2>&1 | ForEach-Object {
                $line = $_.ToString()
                $logWriter.WriteLine($line)
                Write-Output $line
            }
        $testExitCode = $LASTEXITCODE
    }
    finally {
        $logWriter.Dispose()
    }
    if ($testExitCode -ne 0) {
        throw "Windows recording transcription acceptance failed with exit code $testExitCode"
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
}
finally {
    Pop-Location
}
