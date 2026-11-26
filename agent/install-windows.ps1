<#
.SYNOPSIS
  Install LanSentinel Agent on Windows (scheduled task, starts at boot).
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install-windows.ps1 -Server http://192.168.1.102:8110 -Token <token>
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install-windows.ps1 -Uninstall
#>
param(
    [string]$Server = "http://192.168.1.102:8110",
    [string]$Token = "",
    [string]$HostId = $env:COMPUTERNAME,
    [string]$Source = "",
    [string]$InstallDir = "$env:ProgramData\LanSentinel",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$TaskName = "LanSentinelAgent"

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $p = New-Object Security.Principal.WindowsPrincipal($id)
    return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-Admin)) {
    throw "Run PowerShell as Administrator"
}

if ($Uninstall) {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Get-Process sentinel-agent -ErrorAction SilentlyContinue | Stop-Process -Force
    Write-Host "Uninstalled $TaskName"
    exit 0
}

if (-not $Source) {
    $Source = Join-Path $PSScriptRoot "..\bin\sentinel-agent-windows-amd64.exe"
}
$Source = (Resolve-Path $Source).Path

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$Exe = Join-Path $InstallDir "sentinel-agent.exe"
$Conf = Join-Path $InstallDir "agent.json"
Copy-Item -Force $Source $Exe

$cfg = [ordered]@{
    server     = $Server
    token      = $Token
    host_id    = $HostId
    interval_s = 15
    mounts     = @("C:\")
    data_dir   = $InstallDir
}
$json = $cfg | ConvertTo-Json -Depth 4
[IO.File]::WriteAllText($Conf, $json, (New-Object System.Text.UTF8Encoding($false)))
# Config contains a token: only SYSTEM and Administrators can read it.
icacls $Conf /inheritance:r /grant:r "*S-1-5-18:(R)" "*S-1-5-32-544:(R)" | Out-Null

$action = New-ScheduledTaskAction -Execute $Exe -Argument "-config `"$Conf`"" -WorkingDirectory $InstallDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -RestartCount 999 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero)
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Principal $principal -Force `
    -Description "LanSentinel Agent (metrics + controlled actions)" | Out-Null
Start-ScheduledTask -TaskName $TaskName

Start-Sleep -Seconds 3
Get-ScheduledTask -TaskName $TaskName | Select-Object TaskName, State | Format-Table -AutoSize
Get-Process sentinel-agent -ErrorAction SilentlyContinue | Select-Object Id, ProcessName, WS | Format-Table -AutoSize
Write-Host "Installed: $Exe -> $Server (host_id=$HostId)"
