<#
.SYNOPSIS
Builds and installs the CX4300 scanner web UI on Windows.

.DESCRIPTION
On Windows the scanner is driven through WIA, using Epson's own driver, so
nothing here replaces or rebinds a driver. Run from the repository root:

    powershell -ExecutionPolicy Bypass -File .\install.ps1
#>
[CmdletBinding()]
param(
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\cx4300')
)

$ErrorActionPreference = 'Stop'

function Step($msg) { Write-Host "`n== $msg" -ForegroundColor Cyan }
function Note($msg) { Write-Host "   $msg" }

if (-not (Test-Path 'go.mod')) {
    throw 'Run this from the repository root (go.mod not found).'
}

Step 'Checking Go'
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    Note 'Go is not installed. Trying winget...'
    $winget = Get-Command winget -ErrorAction SilentlyContinue
    if (-not $winget) { throw 'Go is required. Install it from https://go.dev/dl/ and re-run.' }
    winget install --id GoLang.Go --accept-package-agreements --accept-source-agreements
    $env:Path = "$env:Path;$env:ProgramFiles\Go\bin"
    $go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $go) { throw 'Go still not on PATH. Open a new shell and re-run.' }
}
Note (& go version)

Step 'Checking for the scanner'
$dev = Get-PnpDevice -Class Image -ErrorAction SilentlyContinue |
       Where-Object { $_.InstanceId -like '*VID_04B8&PID_083F*' }
if ($dev) {
    Note "found: $($dev.FriendlyName) [$($dev.Status)]"
} else {
    Note 'No CX4300 WIA device found. Install the Epson driver from the'
    Note 'product CD/ISO, and if the scanner is attached to WSL run'
    Note '"usbipd detach --busid <id>" so Windows can see it again.'
}

Step 'Building escan.exe'
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exe = Join-Path $InstallDir 'escan.exe'
& go build -trimpath -o $exe ./cmd/escan
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
Note "installed $exe"

Step 'Adding to PATH'
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$InstallDir", 'User')
    Note 'added to your user PATH (open a new shell to pick it up)'
} else {
    Note 'already on PATH'
}

Step 'Done'
Note 'Start it with:   escan          then open http://127.0.0.1:8080/'
Note 'Save scans elsewhere with:   escan --out $HOME\scans'
Note ''
Note 'If a scan fails with "device busy", another application holds the'
Note 'scanner - close Epson Scan (watch for a stuck escndv.exe), or use the'
Note 'Reset device button, which needs an elevated shell.'
