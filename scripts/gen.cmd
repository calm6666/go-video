@echo off
setlocal
rem Windows CMD wrapper. The generator itself is maintained in gen.ps1.
rem Run from the repository root: scripts\gen.cmd [service]
set "SERVICE=%~1"
if "%SERVICE%"=="" (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0gen.ps1"
) else (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0gen.ps1" -Service "%SERVICE%"
)
if errorlevel 1 exit /b %errorlevel%
endlocal
