@echo off
REM Uninstaller for the MAX<->Telegram bridge (removes autostart shortcuts).
cd /d "%~dp0"
echo Removing MAX-TG bridge autostart...
maxtg.exe uninstall
pause
