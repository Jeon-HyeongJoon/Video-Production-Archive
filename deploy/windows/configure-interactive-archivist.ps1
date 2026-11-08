[CmdletBinding()]
param(
    [ValidateSet("Probe", "Install")]
    [string]$Mode = "Probe",
    [string]$Root = "C:\Archive",
    [string]$TaskName = "Archive Archivist Host",
    [string]$InteractiveUser = "",
    [string]$CodexExecutable = "",
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

function Assert-ArchiveRootFile {
    param(
        [string]$RuntimeRoot,
        [string]$Path,
        [string]$Label
    )
    $resolvedRoot = [System.IO.Path]::GetFullPath($RuntimeRoot)
    $resolvedPath = [System.IO.Path]::GetFullPath($Path)
    $prefix = $resolvedRoot.TrimEnd('\') + '\'
    if (-not $resolvedPath.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase) -or
        -not (Test-Path -LiteralPath $resolvedPath -PathType Leaf)) {
        throw "$Label must be an existing file below the Archive runtime root."
    }
    return $resolvedPath
}

function Invoke-ArchiveCodexLoginProbe {
    param(
        [string]$User,
        [string]$Executable,
        [string]$WorkingDirectory
    )
    $probeName = "Archive Codex Login Probe $PID"
    $action = New-ScheduledTaskAction -Execute $Executable -Argument "login status" -WorkingDirectory $WorkingDirectory
    $principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
    Register-ScheduledTask -TaskName $probeName -Action $action -Principal $principal -Settings $settings -Force | Out-Null
    try {
        Start-ScheduledTask -TaskName $probeName
        $deadline = (Get-Date).AddSeconds(30)
        do {
            Start-Sleep -Milliseconds 500
            $task = Get-ScheduledTask -TaskName $probeName
        } while ($task.State -eq "Running" -and (Get-Date) -lt $deadline)
        $info = Get-ScheduledTaskInfo -TaskName $probeName
        if ($task.State -eq "Running") {
            throw "The interactive Codex login probe did not finish."
        }
        if ($info.LastTaskResult -ne 0) {
            throw "Codex is not logged in for the interactive Windows user."
        }
        return [pscustomobject]@{
            result = $info.LastTaskResult
            user_sid = Get-ArchiveAccountSid -Account $User
        }
    }
    finally {
        Stop-ScheduledTask -TaskName $probeName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $probeName -Confirm:$false -ErrorAction SilentlyContinue
    }
}

$resolvedRoot = [System.IO.Path]::GetFullPath($Root)
$user = Get-ArchiveInteractiveUser -ConfiguredUser $InteractiveUser
if ([string]::IsNullOrWhiteSpace($CodexExecutable)) {
    $candidate = Get-ChildItem -LiteralPath (Join-Path $resolvedRoot "tools\codex") -Filter "codex.exe" -File -Recurse |
        Sort-Object FullName -Descending |
        Select-Object -First 1
    if ($null -eq $candidate) {
        throw "Codex executable was not found below the Archive runtime root."
    }
    $CodexExecutable = $candidate.FullName
}
$codex = Assert-ArchiveRootFile -RuntimeRoot $resolvedRoot -Path $CodexExecutable -Label "Codex executable"
$probe = Invoke-ArchiveCodexLoginProbe -User $user -Executable $codex -WorkingDirectory $resolvedRoot

if ($Mode -eq "Probe") {
    [pscustomobject]@{
        mode = "Probe"
        user_sid = $probe.user_sid
        codex_login_result = $probe.result
    } | ConvertTo-Json -Compress
    exit 0
}

$hostExecutable = Assert-ArchiveRootFile -RuntimeRoot $resolvedRoot `
    -Path (Join-Path $resolvedRoot "runtime\current\archivist-host.exe") -Label "Archivist Host executable"
$runScript = Assert-ArchiveRootFile -RuntimeRoot $resolvedRoot `
    -Path (Join-Path $resolvedRoot "runtime\current\run-archivist-host.cmd") -Label "Archivist Host launcher"
$evidenceDirectory = Join-Path $resolvedRoot ("evidence\interactive-archivist-" + (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ"))
New-Item -ItemType Directory -Path $evidenceDirectory -Force | Out-Null

foreach ($name in @($TaskName, "Archive-Archivist")) {
    $existing = Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
    if ($null -eq $existing) {
        continue
    }
    $safeName = $name -replace '[^A-Za-z0-9._-]', '_'
    $xml = Export-ScheduledTask -TaskName $name
    [System.IO.File]::WriteAllText(
        (Join-Path $evidenceDirectory "$safeName.xml"),
        $xml,
        (New-Object System.Text.UTF8Encoding($false))
    )
    Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
    if ($name -ne $TaskName) {
        Disable-ScheduledTask -TaskName $name | Out-Null
    }
}

Get-CimInstance Win32_Process |
    Where-Object { $_.Name -eq "archivist-host.exe" -and
        [string]::Equals($_.ExecutablePath, $hostExecutable, [System.StringComparison]::OrdinalIgnoreCase) } |
    ForEach-Object { Invoke-CimMethod -InputObject $_ -MethodName Terminate | Out-Null }

$action = New-ScheduledTaskAction -Execute $runScript -WorkingDirectory $resolvedRoot
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
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
            Where-Object { $_.Name -eq "archivist-host.exe" -and
                [string]::Equals($_.ExecutablePath, $hostExecutable, [System.StringComparison]::OrdinalIgnoreCase) } |
            Select-Object -First 1
    } while ($null -eq $processRecord -and (Get-Date) -lt $deadline)
    if ($null -eq $processRecord) {
        throw "The interactive Archivist Host did not start."
    }
    $owner = Invoke-CimMethod -InputObject $processRecord -MethodName GetOwner
    $ownerSid = Get-ArchiveAccountSid -Account ($owner.Domain + "\" + $owner.User)
    if ($ownerSid -ne $probe.user_sid -or $processRecord.SessionId -eq 0) {
        throw "The Archivist Host did not start in the authenticated interactive user session."
    }
}

$registered = Get-ScheduledTask -TaskName $TaskName
[pscustomobject]@{
    mode = "Install"
    task_name = $TaskName
    task_logon_type = [string]$registered.Principal.LogonType
    user_sid = $probe.user_sid
    codex_login_result = $probe.result
    process_id = if ($null -eq $processRecord) { 0 } else { $processRecord.ProcessId }
    process_session_id = if ($null -eq $processRecord) { 0 } else { $processRecord.SessionId }
    evidence_directory = $evidenceDirectory
} | ConvertTo-Json -Compress
