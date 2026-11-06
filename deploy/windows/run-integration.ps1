$ErrorActionPreference = 'Stop'

$root = 'C:\Archive'
$bin = Join-Path $root 'bin'
$config = Join-Path $root 'config'
$logs = Join-Path $root 'logs'
$secrets = Join-Path $root 'secrets'
$ready = Join-Path $root 'integration.ready.json'
New-Item -ItemType Directory -Force -Path (Join-Path $root 'workspace') | Out-Null
New-Item -ItemType Directory -Force -Path $secrets | Out-Null

try {
    $existing = Invoke-RestMethod -Uri 'http://127.0.0.1:18080/healthz' -TimeoutSec 2
    if ($existing.status -eq 'ok') {
        throw 'Archive integration is already running'
    }
} catch {
    if ($_.Exception.Message -eq 'Archive integration is already running') {
        throw
    }
}

$credentialBytes = New-Object byte[] 24
$generator = [Security.Cryptography.RandomNumberGenerator]::Create()
$generator.GetBytes($credentialBytes)
$generator.Dispose()
$credential = ([BitConverter]::ToString($credentialBytes)).Replace('-', '')
$secretTool = Join-Path $bin 'archive-secret.exe'
$workerReference = 'dpapi://C:/Archive/secrets/worker-windows.dpapi'
$archivistReference = 'dpapi://C:/Archive/secrets/archivist-windows.dpapi'
$credential | & $secretTool -dpapi-reference $workerReference | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Windows Worker DPAPI bootstrap failed'
}
$credential | & $secretTool -dpapi-reference $archivistReference | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Windows Archivist Host DPAPI bootstrap failed'
}

$control = Start-Process -FilePath (Join-Path $bin 'archive-control.exe') `
    -ArgumentList @('-config', (Join-Path $config 'control.yaml')) `
    -RedirectStandardOutput (Join-Path $logs 'control.stdout.log') `
    -RedirectStandardError (Join-Path $logs 'control.stderr.log') `
    -PassThru

$healthy = $false
for ($attempt = 0; $attempt -lt 30; $attempt++) {
    if ($control.HasExited) {
        throw "Control exited with code $($control.ExitCode)"
    }
    try {
        $health = Invoke-RestMethod -Uri 'http://127.0.0.1:18080/healthz' -TimeoutSec 2
        if ($health.status -eq 'ok') {
            $healthy = $true
            break
        }
    } catch {
        Start-Sleep -Milliseconds 500
    }
}
if (-not $healthy) {
    throw 'Control health check timed out'
}

$worker = Start-Process -FilePath (Join-Path $bin 'archive-worker.exe') `
	-ArgumentList @('-config', (Join-Path $config 'worker.yaml')) `
    -RedirectStandardOutput (Join-Path $logs 'worker.stdout.log') `
    -RedirectStandardError (Join-Path $logs 'worker.stderr.log') `
    -PassThru

$archivistHost = Start-Process -FilePath (Join-Path $bin 'archivist-host.exe') `
    -ArgumentList @('-config', (Join-Path $config 'archivist-host.yaml')) `
    -RedirectStandardOutput (Join-Path $logs 'archivist-host.stdout.log') `
    -RedirectStandardError (Join-Path $logs 'archivist-host.stderr.log') `
    -PassThru

[ordered]@{
    started_at = [DateTimeOffset]::UtcNow.ToString('o')
    control_pid = $control.Id
    worker_pid = $worker.Id
    archivist_host_pid = $archivistHost.Id
} | ConvertTo-Json -Compress | Set-Content -LiteralPath $ready -Encoding UTF8

while (-not $control.HasExited -and -not $worker.HasExited -and -not $archivistHost.HasExited) {
    Start-Sleep -Seconds 2
}
if (-not $control.HasExited) {
    Stop-Process -Id $control.Id -Force
}
if (-not $worker.HasExited) {
    Stop-Process -Id $worker.Id -Force
}
if (-not $archivistHost.HasExited) {
    Stop-Process -Id $archivistHost.Id -Force
}
throw "Integration process stopped: control=$($control.ExitCode) worker=$($worker.ExitCode) archivist_host=$($archivistHost.ExitCode)"
