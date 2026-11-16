@echo off
rem Installs xu526 for this Windows user. Double-click, or run from a terminal.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-windows.ps1" %*
if errorlevel 1 pause
