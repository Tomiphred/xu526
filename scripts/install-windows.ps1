# Installs xu526 on Windows 10/11 for the current user: the xu526 program
# (also as `xu`), a background service that starts when you sign in, a
# Start-menu shortcut to the dashboard, and a container engine.
#
# Container engine: Docker Desktop if it's already installed, otherwise
# Podman, which this script installs (winget) and sets up. Podman needs
# WSL2; Windows asks for permission once while Podman installs.
#
#   Double-click install.cmd, or in PowerShell:
#   powershell -ExecutionPolicy Bypass -File .\install-windows.ps1
#
# Options:
#   -Engine auto|podman|docker   which engine (auto: Docker Desktop if present, else Podman)
#   -Flags "..."                 adds `xu526 daemon` flags, e.g. -Flags "-host-builds"
param([string]$Engine = "auto", [string]$Flags = "")
$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$src = Join-Path $here "bin\windows-$arch"
if (-not (Test-Path (Join-Path $src "xu526.exe"))) {
  # A copy of the source (git clone): build it, if Go 1.24+ is installed.
  $root = Split-Path -Parent $here
  $go = Get-Command go -ErrorAction SilentlyContinue
  if ((Test-Path (Join-Path $root "go.mod")) -and $go) {
    Write-Host "==> building xu526 from source" -ForegroundColor Cyan
    New-Item -ItemType Directory -Force -Path $src | Out-Null
    $ver = (Get-Content (Join-Path $root "VERSION") -ErrorAction SilentlyContinue | Select-Object -First 1)
    if (-not $ver) { $ver = "dev" }
    Push-Location $root
    try {
      $env:CGO_ENABLED = "0"
      & go build -trimpath -ldflags "-s -w -X xu526/internal/version.Version=$ver" -o (Join-Path $src "xu526.exe") ./cmd/xu526
      if ($LASTEXITCODE -ne 0) { throw "go build failed" }
      & go build -trimpath -ldflags "-s -w -H=windowsgui -X xu526/internal/version.Version=$ver" -o (Join-Path $src "xu526-service.exe") ./cmd/xu526
      if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    } finally { Pop-Location }
  } elseif (Test-Path (Join-Path $root "go.mod")) {
    throw "Building xu526 from source needs Go 1.24 or newer (winget install GoLang.Go), or use the ready-built download from the Releases page."
  } else {
    throw "This download has no xu526 for Windows ($arch). Unzip the whole download and run the installer from inside it."
  }
}
$dir = Join-Path $env:LOCALAPPDATA "Programs\xu526"
$data = Join-Path $env:LOCALAPPDATA "xu526"
$task = "xu526"
New-Item -ItemType Directory -Force -Path $dir, $data | Out-Null

function Say($m) { Write-Host "==> $m" -ForegroundColor Cyan }

Say "installing xu526 into $dir"
# Stop a running copy so its files can be replaced.
Stop-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue
Get-Process -Name "xu526-service" -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500
Copy-Item (Join-Path $src "xu526.exe") (Join-Path $dir "xu526.exe") -Force
Copy-Item (Join-Path $src "xu526-service.exe") (Join-Path $dir "xu526-service.exe") -Force
Copy-Item (Join-Path $src "xu526.exe") (Join-Path $dir "xu.exe") -Force # the short name the commands use
# Files from a browser download are marked as coming from the internet.
Get-ChildItem $dir -Filter *.exe | Unblock-File
& (Join-Path $dir "xu526.exe") version

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
if (-not (($userPath -split ";") -contains $dir)) {
  Say "adding $dir to your PATH"
  [Environment]::SetEnvironmentVariable("Path", (($userPath.TrimEnd(";") + ";" + $dir).TrimStart(";")), "User")
}

# ---------- container engine ----------
function Find-Podman {
  $c = Get-Command podman -ErrorAction SilentlyContinue
  if ($c) { return $c.Source }
  foreach ($p in @("$env:ProgramFiles\RedHat\Podman\podman.exe", "$env:LOCALAPPDATA\Programs\RedHat\Podman\podman.exe")) {
    if (Test-Path $p) { return $p }
  }
  return $null
}
$dockerDesktop = (Test-Path "$env:ProgramFiles\Docker\Docker\Docker Desktop.exe") -or (Get-Command docker -ErrorAction SilentlyContinue)
if ($Engine -eq "auto") { $Engine = if ($dockerDesktop) { "docker" } else { "podman" } }
$engineFlag = ""
if ($Engine -eq "docker") {
  Say "using Docker Desktop"
  if (-not (Test-Path "\\.\pipe\docker_engine")) {
    Write-Host "    Docker Desktop isn't running yet; start it. xu526 waits for it after you sign in." -ForegroundColor Yellow
  }
} elseif ($Engine -eq "podman") {
  Say "setting up Podman (the container engine)"
  # Podman runs Linux containers in WSL2.
  & wsl.exe --status *> $null
  if ($LASTEXITCODE -ne 0) {
    Write-Host @"

    Podman needs WSL2, which isn't enabled on this computer yet. Do this once:
      1. Open PowerShell as administrator (right-click Start -> Terminal (Admin)).
      2. Run:  wsl --install --no-distribution
      3. Restart the computer, then run install.cmd again.
"@ -ForegroundColor Yellow
    exit 1
  }
  $podman = Find-Podman
  if (-not $podman) {
    if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
      throw "winget isn't available to install Podman. Install Podman from https://podman.io/ (Windows installer), then run install.cmd again."
    }
    Write-Host "    installing Podman with winget (Windows will ask for permission) ..."
    & winget install -e --id RedHat.Podman --accept-source-agreements --accept-package-agreements --silent
    $podman = Find-Podman
    if (-not $podman) { throw "Podman didn't install. Install it from https://podman.io/, then run install.cmd again." }
  }
  Write-Host "    $(& $podman --version)"
  $machines = & $podman machine list --format "{{.Name}}" 2>$null
  if (-not $machines) {
    Write-Host "    creating Podman's Linux machine (downloads about 400 MB, the first time only) ..."
    & $podman machine init
    if ($LASTEXITCODE -ne 0) { throw "podman machine init failed; see the lines above" }
  }
  $state = & $podman machine inspect --format "{{.State}}" 2>$null
  if ("$state" -notmatch "running") {
    Write-Host "    starting the Podman machine ..."
    & $podman machine start
    if ($LASTEXITCODE -ne 0) { throw "podman machine start failed; see the lines above" }
  }
  # xu526 starts the machine at sign-in from now on and finds its API itself.
  $engineFlag = "-podman `"$podman`""
} else {
  throw "-Engine must be auto, podman or docker"
}

Say "installing the xu526 service (starts when you sign in)"
# Local addresses only: apps answer at http://NAME.localhost:8080 on this
# computer, and get public addresses through xu526 tunnel. Nothing listens
# on the network, so Windows Firewall has nothing to ask about.
$daemonArgs = "daemon -state `"$data\apps.json`" -log `"$data\xu526.log`" -wait-docker 10m " +
  "-http 127.0.0.1:8080 -domain localhost -collector 127.0.0.1:4318 $engineFlag $Flags"
$user = "$env:USERDOMAIN\$env:USERNAME"
$action = New-ScheduledTaskAction -Execute (Join-Path $dir "xu526-service.exe") -Argument $daemonArgs -WorkingDirectory $data
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName $task -Action $action -Trigger $trigger -Settings $settings -Principal $principal `
  -Description "xu526: runs and shares your apps (http://127.0.0.1:7070)" -Force | Out-Null
Start-ScheduledTask -TaskName $task

$menu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\xu526 dashboard.url"
Set-Content -Path $menu -Value "[InternetShortcut]`r`nURL=http://127.0.0.1:7070/`r`n" -Encoding ASCII

Say "waiting for xu526"
$ok = $false
for ($i = 0; $i -lt 20; $i++) {
  try { Invoke-RestMethod -Uri "http://127.0.0.1:7070/v1/node" -TimeoutSec 2 | Out-Null; $ok = $true; break } catch { Start-Sleep -Seconds 1 }
}
Write-Host ""
if ($ok) { Write-Host "xu526 is running." -ForegroundColor Green }
else { Write-Host "xu526 didn't answer yet. Its log: $data\xu526.log" -ForegroundColor Yellow }
Write-Host @"

  Dashboard   http://127.0.0.1:7070   (Start menu: xu526 dashboard)
  Go public   Dashboard -> Settings -> Connect with xu526 tunnel   (or: xu tunnel login)
  Your app    cd C:\path\to\app ; xu build . --deploy myapp
  Log         $data\xu526.log
  Uninstall   uninstall.cmd   (or uninstall-windows.ps1 -Purge to delete apps and data)

Open a new terminal so the xu command is found.
"@
if ($ok) { Start-Process "http://127.0.0.1:7070/" }
