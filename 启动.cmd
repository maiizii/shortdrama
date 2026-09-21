@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
"dist\juku_windows_amd64.exe" -listen 127.0.0.1:8998
if errorlevel 1 pause
