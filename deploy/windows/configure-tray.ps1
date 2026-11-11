[CmdletBinding()]
param(
    [ValidateSet("Probe", "Install", "Remove")]
    [string]$Mode = "Probe",
    [string]$Root = "C:\Archive",
    [string]$TaskName = "Archive Tray",
    [string]$InteractiveUser = "",
    [string]$DashboardURL = "",
    [string]$ChromeExecutable = "",
    [switch]$NoStart
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Get-ArchiveInteractiveUser {
    param([string]$ConfiguredUser)
    if (-not [string]::IsNullOrWhiteSpace($ConfiguredUser)) {
        return $ConfiguredUser
    }
    $user = (Get-CimInstance Win32_ComputerSystem).UserName
    if ([string]::IsNullOrWhiteSpace($user)) {
        throw "No interactive Windows user is signed in."
    }
    return $user
}

function Get-ArchiveAccountSid {
    param([string]$Account)
    return (New-Object System.Security.Principal.NTAccount($Account)).Translate(
        [System.Security.Principal.SecurityIdentifier]
    ).Value
}

function Get-ArchiveChromeExecutable {
    param([string]$ConfiguredPath)
    $candidates = @()
    if (-not [string]::IsNullOrWhiteSpace($ConfiguredPath)) {
        $candidates += $ConfiguredPath
    }
    foreach ($registryPath in @(
        "HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe",
        "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe"
    )) {
        $registryKey = Get-Item -LiteralPath $registryPath -ErrorAction SilentlyContinue
        $value = if ($null -eq $registryKey) { "" } else { [string]$registryKey.GetValue("") }
        if (-not [string]::IsNullOrWhiteSpace($value)) {
            $candidates += $value
        }
    }
    foreach ($base in @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:LOCALAPPDATA)) {
        if (-not [string]::IsNullOrWhiteSpace($base)) {
            $candidates += (Join-Path $base "Google\Chrome\Application\chrome.exe")
        }
    }
    foreach ($candidate in $candidates) {
        if ([System.IO.Path]::IsPathRooted($candidate) -and (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            return [System.IO.Path]::GetFullPath($candidate)
        }
    }
    throw "Chrome executable was not found."
}

function Assert-ArchiveDashboardURL {
    param([string]$Value)
    $uri = $null
    $created = [System.Uri]::TryCreate($Value, [System.UriKind]::Absolute, [ref]$uri)
    if (-not $created -or
        ($uri.Scheme -ne "http" -and $uri.Scheme -ne "https") -or
        -not [string]::IsNullOrWhiteSpace($uri.UserInfo) -or
        -not [string]::IsNullOrWhiteSpace($uri.Fragment)) {
        throw "DashboardURL must be an absolute http or https URL without user information or fragment."
    }
    return $uri.AbsoluteUri
}

function Stop-ArchiveTrayProcess {
    param([string]$Executable)
    Get-CimInstance Win32_Process |
        Where-Object { $_.Name -eq "archive-tray.exe" -and
            [string]::Equals($_.ExecutablePath, $Executable, [System.StringComparison]::OrdinalIgnoreCase) } |
        ForEach-Object { Invoke-CimMethod -InputObject $_ -MethodName Terminate | Out-Null }
}

$resolvedRoot = [System.IO.Path]::GetFullPath($Root)
$trayExecutable = [System.IO.Path]::GetFullPath((Join-Path $resolvedRoot "runtime\current\archive-tray.exe"))

if ($Mode -eq "Remove") {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Stop-ArchiveTrayProcess -Executable $trayExecutable
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    [pscustomobject]@{ mode = "Remove"; task_name = $TaskName } | ConvertTo-Json -Compress
    exit 0
}

if (-not (Test-Path -LiteralPath $trayExecutable -PathType Leaf)) {
    throw "Archive Tray executable was not found in runtime\current."
}
$user = Get-ArchiveInteractiveUser -ConfiguredUser $InteractiveUser
$userSid = Get-ArchiveAccountSid -Account $user
$dashboard = Assert-ArchiveDashboardURL -Value $DashboardURL
$chrome = Get-ArchiveChromeExecutable -ConfiguredPath $ChromeExecutable

if ($Mode -eq "Probe") {
    [pscustomobject]@{
        mode = "Probe"
        task_name = $TaskName
        user = $user
        user_sid = $userSid
        dashboard_url = $dashboard
        chrome_executable = $chrome
        tray_executable = $trayExecutable
    } | ConvertTo-Json -Compress
    exit 0
}

$evidenceDirectory = Join-Path $resolvedRoot ("evidence\tray-" + (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ"))
New-Item -ItemType Directory -Path $evidenceDirectory -Force | Out-Null
$existing = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($null -ne $existing) {
    [System.IO.File]::WriteAllText(
        (Join-Path $evidenceDirectory "previous-task.xml"),
        (Export-ScheduledTask -TaskName $TaskName),
        (New-Object System.Text.UTF8Encoding($false))
    )
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
}
Stop-ArchiveTrayProcess -Executable $trayExecutable

$arguments = '--dashboard-url "' + $dashboard + '" --chrome "' + $chrome + '"'
$action = New-ScheduledTaskAction -Execute $trayExecutable -Argument $arguments -WorkingDirectory (Split-Path -Parent $trayExecutable)
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -StartWhenAvailable
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null

$processRecord = $null
if (-not $NoStart) {
    Start-ScheduledTask -TaskName $TaskName
    $deadline = (Get-Date).AddSeconds(30)
    do {
        Start-Sleep -Milliseconds 500
        $processRecord = Get-CimInstance Win32_Process |
            Where-Object { $_.Name -eq "archive-tray.exe" -and
                [string]::Equals($_.ExecutablePath, $trayExecutable, [System.StringComparison]::OrdinalIgnoreCase) } |
            Select-Object -First 1
    } while ($null -eq $processRecord -and (Get-Date) -lt $deadline)
    if ($null -eq $processRecord) {
        throw "Archive Tray did not start."
    }
    $owner = Invoke-CimMethod -InputObject $processRecord -MethodName GetOwner
    $ownerSid = Get-ArchiveAccountSid -Account ($owner.Domain + "\" + $owner.User)
    if ($ownerSid -ne $userSid -or $processRecord.SessionId -eq 0) {
        throw "Archive Tray did not start in the configured interactive user session."
    }
}

$registered = Get-ScheduledTask -TaskName $TaskName
[pscustomobject]@{
    mode = "Install"
    task_name = $TaskName
    task_logon_type = [string]$registered.Principal.LogonType
    task_run_level = [string]$registered.Principal.RunLevel
    user_sid = $userSid
    dashboard_url = $dashboard
    chrome_executable = $chrome
    process_id = if ($null -eq $processRecord) { 0 } else { $processRecord.ProcessId }
    process_session_id = if ($null -eq $processRecord) { 0 } else { $processRecord.SessionId }
    evidence_directory = $evidenceDirectory
} | ConvertTo-Json -Compress
