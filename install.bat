@echo off
REM Installer for the MAX<->Telegram bridge (user-session autostart, no admin).
cd /d "%~dp0"
echo Installing MAX-TG bridge autostart...
maxtg.exe install
echo.
echo Done. Open the "MAX-TG Console" desktop shortcut to set your phone and
echo Telegram bot token, then log into MAX in the Edge window that opens.
echo The bridge starts automatically at your next logon.
pause
