$ErrorActionPreference = 'Stop'

$control = 'http://127.0.0.1:18080'
$origin = 'http://127.0.0.1:18080'
$web = New-Object Microsoft.PowerShell.Commands.WebRequestSession
$session = Invoke-RestMethod -Uri "$control/api/v1/session" -WebSession $web
$headers = @{
    Origin = $origin
    'X-CSRF-Token' = $session.data.csrf_token
}
$suffix = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
$jobId = "JOB-WINDOWS-CANCEL-$suffix"

$create = @{
    job_id = $jobId
    command_id = "COMMAND-CREATE-$suffix"
    idempotency_key = "$jobId`:create"
    reason = 'Windows Worker cancellation acceptance'
    template_name = 'occasion-recording'
    target_title = 'Windows cancellation acceptance'
    control_mode = 'ARCHIVIST'
    expected_revision = 0
} | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "$control/api/v1/jobs" -WebSession $web -Headers $headers -ContentType 'application/json' -Body $create | Out-Null

$running = $null
for ($attempt = 0; $attempt -lt 80; $attempt++) {
    $detail = Invoke-RestMethod -Uri "$control/api/v1/jobs/$jobId"
    if ($detail.data.summary.step_state -eq 'RUNNING') {
        $running = $detail
        break
    }
    Start-Sleep -Milliseconds 100
}
if ($null -eq $running) {
    throw 'Windows Worker did not claim the acceptance Step'
}

$cancel = @{
    command_id = "COMMAND-CANCEL-$suffix"
    idempotency_key = "$jobId`:cancel"
    reason = 'Windows acceptance stops running fixture process'
    expected_revision = $running.data.summary.revision
} | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "$control/api/v1/jobs/$jobId/cancel" -WebSession $web -Headers $headers -ContentType 'application/json' -Body $cancel | Out-Null

$cancelled = $null
for ($attempt = 0; $attempt -lt 80; $attempt++) {
    $detail = Invoke-RestMethod -Uri "$control/api/v1/jobs/$jobId"
    if ($detail.data.summary.step_state -eq 'CANCELLED') {
        $cancelled = $detail
        break
    }
    Start-Sleep -Milliseconds 100
}
if ($null -eq $cancelled) {
    throw 'Windows Worker did not settle cancellation cleanup'
}
$cancelledStep = $cancelled.data.steps | Where-Object { $_.id -eq $running.data.summary.current_step_id }
if ($cancelledStep.state -ne 'CANCELLED' -or $cancelledStep.log_reference -ne 'fixture://worker-cancelled-clean') {
    throw 'Windows cancellation evidence is incomplete'
}

$sessionId = "SESSION-WINDOWS-$suffix"
$turnId = "TURN-WINDOWS-$suffix"
$requestId = "REQUEST-WINDOWS-$suffix"
$archivistSession = @{
    session_id = $sessionId
    archivist_host_id = 'archivist-windows'
    backend = 'EMBEDDED_CORE'
    cli_id = 'fixture-archivist'
    workspace_root_id = 'video-project'
    job_id = $jobId
} | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "$control/api/v1/archivist-sessions" -WebSession $web -Headers $headers -ContentType 'application/json' -Body $archivistSession | Out-Null
$turn = @{
    turn_id = $turnId
    request_id = $requestId
    prompt = 'Windows Archivist Host MCP acceptance'
} | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "$control/api/v1/archivist-sessions/$sessionId/turns" -WebSession $web -Headers $headers -ContentType 'application/json' -Body $turn | Out-Null

$archivistEvidence = $null
for ($attempt = 0; $attempt -lt 100; $attempt++) {
    $sessions = Invoke-RestMethod -Uri "$control/api/v1/archivist-sessions"
    $candidate = $sessions.data | Where-Object { $_.session_id -eq $sessionId }
    $turnView = $candidate.turns | Where-Object { $_.turn_id -eq $turnId }
    $completedCall = $turnView.tool_calls | Where-Object { $_.state -eq 'COMPLETED' -and $_.tool_name -eq 'archive.assets.search.v1' }
    if ($null -ne $completedCall) {
        $archivistEvidence = $completedCall
        break
    }
    Start-Sleep -Milliseconds 100
}
if ($null -eq $archivistEvidence) {
    throw 'Windows Archivist Host did not complete its MCP Tool Call'
}

$workers = Invoke-RestMethod -Uri "$control/api/v1/workers"
$hosts = Invoke-RestMethod -Uri "$control/api/v1/archivist-hosts"
$worker = $workers.data | Where-Object { $_.worker_id -eq 'worker-windows' }
$archivistStatus = $hosts.data | Where-Object { $_.archivist_host_id -eq 'archivist-windows' }
if ($worker.state -notin @('ONLINE', 'BUSY') -or $archivistStatus.state -ne 'ONLINE') {
    throw 'Windows Worker or Archivist Host is not online'
}

[ordered]@{
    job_id = $jobId
    cancelled_step_id = $cancelled.data.summary.current_step_id
    cancellation_log_reference = $cancelledStep.log_reference
    worker_state = $worker.state
    worker_snapshot_id = $worker.snapshot_id
    archivist_host_state = $runtimeStatus.state
    session_id = $sessionId
    turn_id = $turnId
    tool_call_id = $archivistEvidence.tool_call_id
    tool_name = $archivistEvidence.tool_name
    tool_state = $archivistEvidence.state
} | ConvertTo-Json -Compress
