@echo off
rem Removes xu526 from this Windows user (add -Purge to delete apps and data).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0uninstall-windows.ps1" %*
pause
