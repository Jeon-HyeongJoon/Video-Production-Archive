param(
    [string]$ProjectRoot = 'C:\Archive',
    [string]$ReferenceTextUTF8Base64 = '7Jik64qYIOyYgeyDgSDsoJzsnpEg7ZaJ7IKs7JeQ7ISc64qUIOy0rOyYge2VnCDrhbntmZQg7JiB7IOB7J2EIOygleumrO2VmOqzoCDsnpDrp4nsnYQg66eM65Ot64uI64ukLg=='
)

$ErrorActionPreference = 'Stop'
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
$evidence = Join-Path $ProjectRoot ('evidence\windows-whisper-smoke-' + $stamp)
New-Item -ItemType Directory -Force -Path $evidence | Out-Null

$reference = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($ReferenceTextUTF8Base64))
$rawAudio = Join-Path $evidence 'synthetic-ko-raw.wav'
$audio = Join-Path $evidence 'synthetic-ko-16k.wav'

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
    $synthesizer.SetOutputToWaveFile($rawAudio)
    $synthesizer.Speak($reference)
}
finally {
    $synthesizer.Dispose()
}

$ffmpeg = Join-Path $ProjectRoot 'tools\ffmpeg-release-essentials\ffmpeg-9.0.2-essentials_build\bin\ffmpeg.exe'
$ffmpegLog = & $ffmpeg -hide_banner -loglevel error -y -i $rawAudio -ac 1 -ar 16000 -c:a pcm_s16le $audio 2>&1
if ($LASTEXITCODE -ne 0) {
    throw ($ffmpegLog -join "`n")
}
$inputHashBefore = (Get-FileHash -Algorithm SHA256 $audio).Hash.ToLowerInvariant()

$whisper = Join-Path $ProjectRoot 'tools\whispercpp-b5454\Release\whisper-cli.exe'
$model = Join-Path $ProjectRoot 'models\ggml-large-v3-turbo-q5_0.bin'
$outputPrefix = Join-Path $evidence 'transcript'
$stdoutPath = Join-Path $evidence 'whisper.stdout.log'
$stderrPath = Join-Path $evidence 'whisper.stderr.log'
$arguments = @('-m', $model, '-f', $audio, '-oj', '-of', $outputPrefix, '-l', 'ko', '-t', '4', '-np')
$argumentLine = ($arguments | ForEach-Object {
    if ($_ -match '[\s"]') {
        if ($_.Contains('"')) {
            throw 'whisper.cpp argument contains a quote'
        }
        '"' + $_ + '"'
    }
    else {
        $_
    }
}) -join ' '

$startInfo = New-Object Diagnostics.ProcessStartInfo
$startInfo.FileName = $whisper
$startInfo.Arguments = $argumentLine
$startInfo.UseShellExecute = $false
$startInfo.CreateNoWindow = $true
$startInfo.RedirectStandardOutput = $true
$startInfo.RedirectStandardError = $true
$process = New-Object Diagnostics.Process
$process.StartInfo = $startInfo
$timer = [Diagnostics.Stopwatch]::StartNew()
$started = $process.Start()
if (-not $started) {
    throw 'whisper.cpp process did not start'
}
$stdoutTask = $process.StandardOutput.ReadToEndAsync()
$stderrTask = $process.StandardError.ReadToEndAsync()
$peakWorkingSetBytes = 0L
while (-not $process.WaitForExit(100)) {
    $process.Refresh()
    $peakWorkingSetBytes = [Math]::Max($peakWorkingSetBytes, $process.PeakWorkingSet64)
}
$process.WaitForExit()
$timer.Stop()
$process.Refresh()
$peakWorkingSetBytes = [Math]::Max($peakWorkingSetBytes, $process.PeakWorkingSet64)
$exitCode = $process.ExitCode
[IO.File]::WriteAllText($stdoutPath, $stdoutTask.Result, (New-Object Text.UTF8Encoding($false)))
[IO.File]::WriteAllText($stderrPath, $stderrTask.Result, (New-Object Text.UTF8Encoding($false)))
$process.Dispose()
if ($exitCode -ne 0) {
    throw "whisper.cpp failed with exit code $exitCode"
}

$inputHashAfter = (Get-FileHash -Algorithm SHA256 $audio).Hash.ToLowerInvariant()
if ($inputHashBefore -ne $inputHashAfter) {
    throw 'speech audio input changed during transcription'
}
$transcript = Get-Content -Raw -Encoding UTF8 ($outputPrefix + '.json') | ConvertFrom-Json
$segments = @($transcript.transcription)
$texts = @($segments | ForEach-Object { [string]$_.text } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
if ([string]$transcript.result.language -ne 'ko' -or $texts.Count -eq 0) {
    throw 'non-empty Korean transcript was not produced'
}

$record = [ordered]@{
    schema_version = 1
    evidence_kind = 'smoke'
    requirement_ids = @('CFG-010', 'TRN-003')
    requirement_status = [ordered]@{ 'CFG-010' = 'implemented'; 'TRN-003' = 'planned' }
    representative_acceptance = $false
    generated_at = (Get-Date).ToUniversalTime().ToString('o')
    worker_id = 'worker-windows'
    processor_version = 'whisper.cpp-1.9.5-b5454'
    model_id = 'ggml-large-v3-turbo-q5_0'
    model_sha1 = (Get-FileHash -Algorithm SHA1 $model).Hash.ToLowerInvariant()
    profile = 'event-ko@v1'
    language = [string]$transcript.result.language
    segment_count = $segments.Count
    processing_millis = $timer.ElapsedMilliseconds
    peak_working_set_bytes = $peakWorkingSetBytes
    input_sha256 = $inputHashBefore
    input_unchanged = $true
    synthetic_reference = $reference
    recognized_text = ($texts -join ' ').Trim()
    result = 'passed'
}
$payload = $record | ConvertTo-Json -Depth 6
[IO.File]::WriteAllText(
    (Join-Path $evidence 'run.json'),
    $payload + "`n",
    (New-Object Text.UTF8Encoding($false))
)

Write-Output ('EVIDENCE=' + $evidence)
Write-Output ('LANGUAGE=' + $record.language)
Write-Output ('SEGMENTS=' + $record.segment_count)
Write-Output ('PROCESSING_MILLIS=' + $record.processing_millis)
Write-Output ('PEAK_WORKING_SET_BYTES=' + $record.peak_working_set_bytes)
Write-Output ('RECOGNIZED=' + $record.recognized_text)
