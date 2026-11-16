# Removes xu526 from Windows (installed by install-windows.ps1).
#   -Purge   also delete apps, data, logs, the build cache, your xu login,
#            and the containers, volumes and xu/ images xu526 made
#   -Yes     don't ask
param([switch]$Purge, [switch]$Yes)
$ErrorActionPreference = "Stop"
$dir = Join-Path $env:LOCALAPPDATA "Programs\xu526"
$data = Join-Path $env:LOCALAPPDATA "xu526"
$menu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\xu526 dashboard.url"
$extra = @((Join-Path $env:LOCALAPPDATA "xu"), (Join-Path $env:APPDATA "xu")) # build cache, xu login

Write-Host "This removes the xu526 service, $dir, the Start-menu shortcut and the PATH entry."
if ($Purge) { Write-Host "With -Purge it also deletes $data (apps and data), the build cache, your xu login, and xu526's containers, volumes and xu/ images (Podman or Docker)." -ForegroundColor Yellow }
if (-not $Yes) {
  $a = Read-Host "Continue? [y/N]"
  if ($a -notmatch '^(y|yes)$') { Write-Host "nothing removed"; exit 0 }
}

Stop-ScheduledTask -TaskName "xu526" -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName "xu526" -Confirm:$false -ErrorAction SilentlyContinue
Get-Process -Name "xu526-service" -ErrorAction SilentlyContinue | Stop-Process -Force

# Docker Desktop's docker, or Podman's podman: both take the same commands.
$cli = (Get-Command docker -ErrorAction SilentlyContinue).Source
if (-not $cli) { $cli = (Get-Command podman -ErrorAction SilentlyContinue).Source }
if (-not $cli -and (Test-Path "$env:ProgramFiles\RedHat\Podman\podman.exe")) { $cli = "$env:ProgramFiles\RedHat\Podman\podman.exe" }
function docker { & $cli @args }
if ($Purge -and $cli) {
  try {
    docker info *> $null
    if ($LASTEXITCODE -ne 0) { throw "engine not running" }
    Write-Host "removing xu526's containers, networks, volumes and images"
    foreach ($id in (docker ps -aq --filter label=xu.managed=true)) { docker rm -f $id *> $null }
    foreach ($id in (docker network ls -q --filter label=xu.managed=true)) { docker network rm $id *> $null }
    foreach ($id in (docker volume ls -q --filter label=xu.managed=true)) { docker volume rm -f $id *> $null }
    foreach ($ref in (docker images --format "{{.Repository}}:{{.Tag}}" | Where-Object { $_ -like "xu/*" })) { docker rmi $ref *> $null }
  } catch { Write-Host "The container engine isn't running (start Podman with: podman machine start, or start Docker Desktop), then run this again with -Purge to remove xu526's containers." -ForegroundColor Yellow }
}

Start-Sleep -Milliseconds 500
if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
if (Test-Path $menu) { Remove-Item -Force $menu }
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath) {
  $kept = ($userPath -split ";") | Where-Object { $_ -and $_ -ne $dir }
  [Environment]::SetEnvironmentVariable("Path", ($kept -join ";"), "User")
}
if ($Purge) {
  foreach ($p in @($data) + $extra) { if (Test-Path $p) { Remove-Item -Recurse -Force $p } }
  Write-Host "xu526 and its data are removed."
} else {
  Write-Host "xu526 is removed. Your apps' data is kept in $data; run with -Purge to delete it."
}
Write-Host "The container engine (Podman or Docker Desktop) stays installed. To remove Podman too: podman machine rm -f; winget uninstall RedHat.Podman"
