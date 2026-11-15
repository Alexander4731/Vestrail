@echo off
chcp 65001 >nul 2>&1
setlocal
cd /d "%~dp0"
"%~dp0vestrail-launcher.exe" %*
if errorlevel 1 pause
