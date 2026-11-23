[CmdletBinding(DefaultParameterSetName = 'Detail')]
param(
    [Parameter(ParameterSetName = 'Detail')][string] $RunId,
    [Parameter(ParameterSetName = 'Detail')][string] $SourceFolder,
    [Parameter(ParameterSetName = 'History', Mandatory)][switch] $ListRuns
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$evidence = 'C:\Archive\scratch\source-classification-rehearsal\evidence'
function Read-Record([string] $file) {
    Get-Content -LiteralPath $file -Raw -Encoding UTF8 | ConvertFrom-Json
}
if ($ListRuns) {
    $history = @()
    if (Test-Path -LiteralPath $evidence) {
        $history = @(foreach ($dir in Get-ChildItem -LiteralPath $evidence -Directory | Sort-Object Name) {
            if ($dir.Name -cnotmatch '^\d{8}T\d{6}Z-(sample|full)$') { continue }
            $journal = Join-Path $dir.FullName 'journal.jsonl'
            if (!(Test-Path -LiteralPath $journal)) { continue }
            $last = Get-Content -LiteralPath $journal -Encoding UTF8 -Tail 1 | ConvertFrom-Json
            $secondary = Join-Path $dir.FullName 'stt-status.json'
            if ($last.stage -eq 'COMPLETE' -and (Test-Path -LiteralPath $secondary)) { $last = Read-Record $secondary }
            [ordered]@{ run_id = $dir.Name; stage = $last.stage; observed_at = $last.observed_at }
        })
    }
    [ordered]@{ runs = $history } | ConvertTo-Json -Depth 4
    return
}
$currentPath = Join-Path $evidence 'current.json'
$probePath = Join-Path $evidence 'probe-status.json'
if (!(Test-Path -LiteralPath $currentPath) -and !$RunId) {
    if ($SourceFolder) { throw 'No classified folder evidence is available yet' }
    if (Test-Path -LiteralPath $probePath) {
        $scan = Read-Record $probePath
        [ordered]@{ stage = $scan.phase; input_revision = $scan.input_revision; header_scan = $scan } | ConvertTo-Json -Depth 4
    } else {
        [ordered]@{ stage = 'NOT_STARTED' } | ConvertTo-Json
    }
    return
}
$current = if (Test-Path -LiteralPath $currentPath) { Read-Record $currentPath }
if (!$RunId) { $RunId = $current.run_id }
if ($RunId -cnotmatch '^\d{8}T\d{6}Z-(sample|full)$') { throw 'Invalid rehearsal run ID' }
$dir = Join-Path $evidence $RunId
$journal = Join-Path $dir 'journal.jsonl'
$entries = @(Get-Content -LiteralPath $journal -Encoding UTF8 | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json })
if (!$entries.Count -or $entries[-1].run_id -ne $RunId) { throw 'Missing run evidence' }
$last = $entries[-1]
$receipt = 'destination.json'
if (Test-Path -LiteralPath (Join-Path $dir 'stage2-destination.json')) { $receipt = 'stage2-destination.json' }
$manifest = Join-Path $dir $receipt
$diagnostic = Join-Path $dir 'encoder-diagnostic.txt'
$result = [ordered]@{
    run_id = $RunId
    is_current = ($RunId -eq $current.run_id)
    input_revision = $last.input_revision
    header_scan = (Read-Record (Join-Path $evidence 'probe-status.json'))
    observed_at = $last.observed_at
    stage = $last.stage
    video_stage = $last.stage
    latest_observation_age_seconds = [int](([DateTimeOffset]::UtcNow - [DateTimeOffset]::Parse($last.observed_at)).TotalSeconds)
    progress = ($last | Select-Object role, encoded_s, total_s, percent, log_bytes)
    planned = @($entries | Where-Object stage -eq 'PLANNED' | Select-Object channels,count,free_bytes,estimated_full_bytes)
    classified = @($entries | Where-Object stage -eq 'CLASSIFIED' | Select-Object role,source_files,source_duration_s,expected_output_s,bitrate_mbps,cut_markers,mode)
    verified_videos = @($entries | Where-Object stage -eq 'VERIFIED_VIDEO' | Select-Object role,duration_s,video_bitrate,markers,relative_path)
    transcription = @($entries | Where-Object stage -eq 'CHANNEL_COMPLETE' | Select-Object role,subtitle_status)
    journal = "$RunId/journal.jsonl"
    held = @($entries | Where-Object stage -eq 'HOLD' | Select-Object folder,reason)
    failure = $null
    verified_destination = $null
}
if ($last.stage -eq 'FAILED') {
    $result.failure = if (Test-Path -LiteralPath $diagnostic) {
        (Get-Content -LiteralPath $diagnostic -Raw -Encoding UTF8).Trim()
    } else { [ordered]@{ cause = $last.cause; journal = "$RunId/journal.jsonl" } }
}
if (Test-Path -LiteralPath $manifest) {
    $data = Read-Record $manifest
    $result.verified_destination = [ordered]@{ receipt = "$RunId/$receipt"; entries = $data.entries; outputs = $data.channels.Count }
}
$stageTwo = Join-Path $dir 'stt-status.json'
if (Test-Path -LiteralPath $stageTwo) {
    $second = Read-Record $stageTwo
    if ($second.run_id -ne $RunId) { throw 'Subtitle evidence belongs to another run' }
    $result.subtitle_stage = $second | Select-Object run_id, stage, role, completed_roles, percent, log_bytes, observed_at, backend_release, evidence, native_ref, receipt
    if ($second.stage -eq 'TRANSCRIBING') {
        $metric = $second
        if ($null -eq $metric.percent) {
            $metric = Get-Content -LiteralPath (Join-Path $dir 'stt-journal.jsonl') -Tail 32 -Encoding UTF8 |
                ForEach-Object { $_ | ConvertFrom-Json } |
                Where-Object { $_.stage -eq 'TRANSCRIBING' -and $_.role -eq $second.role -and $null -ne $_.percent } |
                Select-Object -Last 1
        }
        if ($null -ne $metric.percent) {
            $result.subtitle_stage.percent = $metric.percent
            $result.subtitle_stage | Add-Member -NotePropertyName percent_observed_at -NotePropertyValue $metric.observed_at
        }
    }
    if ($second.stage -eq 'FAILED') {
        $message = [string]$second.cause
        if ($message.StartsWith('UnicodeDecodeError')) { $message = 'INVALID_UTF8 in native subtitle' }
        $result.failure = [ordered]@{ cause = $message.Substring(0, [Math]::Min(320, $message.Length)); evidence = $second.evidence }
    }
    if ($last.stage -eq 'COMPLETE') {
        $result.stage = $second.stage
        $result.progress = ($result.subtitle_stage | Select-Object role, percent, percent_observed_at, log_bytes)
        $result.observed_at = $second.observed_at
        $result.latest_observation_age_seconds = [int](([DateTimeOffset]::UtcNow - [DateTimeOffset]::Parse($second.observed_at)).TotalSeconds)
    }
    if ($receipt -eq 'stage2-destination.json') {
        $result.transcription = @(foreach ($channel in $data.channels) {
            $facts = $channel.subtitle_evidence
            [ordered]@{
                role = $channel.role; subtitle = $channel.subtitle; status = $channel.transcript_status
                encoding_review = $facts.text_quality
                invalid_spans = @($facts.invalid_utf8 | Where-Object { $null -ne $_ }).Count
                time_adjustments = @($facts.boundary_edits | Where-Object { $null -ne $_ }).Count
                native_evidence = $facts.native_ref
            }
        })
    }
}
$qcPath = Join-Path $dir 'independent-destination-qc.json'
if (Test-Path -LiteralPath $qcPath) {
    $qc = Read-Record $qcPath
    if ($qc.run_id -ne $RunId) { throw 'Destination QC belongs to another run' }
    $result.destination_qc = [ordered]@{
        receipt = "$RunId/independent-destination-qc.json"; events = $qc.events
        max_folder_depth = $qc.max_folder_depth
        videos = @($qc.videos | Select-Object video,duration_s,bitrate_bps,markers,subtitle_segments,invalid_utf8_spans,time_adjustments,sha256,subtitle_sha256)
    }
}
$decisions = Join-Path $evidence 'drive-decisions.json'
if (Test-Path -LiteralPath $decisions) {
    $drive = Read-Record $decisions
    if ([string]$drive.summary.revision -eq [string]$last.input_revision) {
        $result.drive_replay = $drive.summary
        $result.replay_evidence = 'drive-decisions.json'
        if ($SourceFolder) {
            $matchesForFolder = @($drive.folders | Where-Object source_folder -ceq $SourceFolder)
            if ($matchesForFolder.Count -ne 1) { throw 'Folder is outside this inventory' }
            $result.folder_decision = $matchesForFolder[0]
        }
    } elseif ($SourceFolder) { throw 'Source revision differs from run evidence' }
} elseif ($SourceFolder) { throw 'Drive replay evidence is unavailable' }
$result | ConvertTo-Json -Depth 7
