[CmdletBinding()]
param(
    [string]$BinaryDirectory = 'C:\Archive\bin',
    [switch]$CheckOnly
)

$ErrorActionPreference = 'Stop'

$root = 'C:\Archive'
$bin = $BinaryDirectory
foreach ($name in @('archive-control.exe', 'archive-media.exe', 'archivist-host.exe', 'archive-secret.exe')) {
    $required = Join-Path $bin $name
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) {
        throw "Missing integration executable: $required"
    }
}
if ($CheckOnly) {
    return
}
$config = Join-Path $root 'config'
$logs = Join-Path $root 'logs'
$secrets = Join-Path $root 'secrets'
$ready = Join-Path $root 'integration.ready.json'
New-Item -ItemType Directory -Force -Path (Join-Path $root 'workspace') | Out-Null
New-Item -ItemType Directory -Force -Path $secrets | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $logs 'operations') | Out-Null

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

$secretTool = Join-Path $bin 'archive-secret.exe'
foreach ($name in @('worker-windows.dpapi', 'archivist-windows.dpapi')) {
    $path = Join-Path $secrets $name
    if (Test-Path -LiteralPath $path -PathType Leaf) {
        continue
    }
    $credentialBytes = New-Object byte[] 24
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $generator.GetBytes($credentialBytes)
        $credential = ([BitConverter]::ToString($credentialBytes)).Replace('-', '')
        $credential | & $secretTool -dpapi-reference ("dpapi://" + $path.Replace('\', '/')) | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Windows DPAPI bootstrap failed for $name"
        }
    } finally {
        $generator.Dispose()
        [Array]::Clear($credentialBytes, 0, $credentialBytes.Length)
        $credential = $null
    }
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

$worker = Start-Process -FilePath (Join-Path $bin 'archive-media.exe') `
	-ArgumentList @('runner', '-config', (Join-Path $config 'worker.yaml')) `
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
