@echo off
setlocal
chcp 65001 >nul
title Gwifi download switch
set "gwifi_action=%~1"
if /I "%gwifi_action%"=="dual" goto run
if /I "%gwifi_action%"=="off" goto run
if /I "%gwifi_action%"=="status" goto run
echo Invalid action. Use the launchers in the project folder.
pause
exit /b 2

:run
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0system-download.ps1" -Action "%gwifi_action%"
set "gwifi_result=%errorlevel%"
echo.
if not "%gwifi_result%"=="0" echo Operation failed. Read the error above; no success is assumed.
if "%gwifi_result%"=="0" echo Done.
echo This window can now be closed.
pause
exit /b %gwifi_result%
