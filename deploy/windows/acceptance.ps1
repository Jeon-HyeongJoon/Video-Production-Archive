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

$sessionId = "SESSION-WINDOWS-$suffix"
$turnId = "TURN-WINDOWS-$suffix"
$requestId = "REQUEST-WINDOWS-$suffix"
$archivistSession = @{
    session_id = $sessionId
    archivist_host_id = 'archivist-windows'
    cli_id = 'fixture-archivist'
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
    worker_state = $worker.state
    worker_snapshot_id = $worker.snapshot_id
    archivist_host_state = $archivistStatus.state
    session_id = $sessionId
    turn_id = $turnId
    tool_call_id = $archivistEvidence.tool_call_id
    tool_name = $archivistEvidence.tool_name
    tool_state = $archivistEvidence.state
} | ConvertTo-Json -Compress
