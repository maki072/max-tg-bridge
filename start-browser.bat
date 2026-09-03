@echo off
REM Launches Edge/Chrome with remote debugging + a dedicated persistent profile,
REM opened at web.max.ru. Log in ONCE in this window; the bridge service then
REM auto-reads a fresh MAX token from it whenever needed (browser_cdp_url).
setlocal
set "PROFILE=%LOCALAPPDATA%\MaxTgBrowserProfile"
set "PORT=9222"
set "EDGE=%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe"
set "CHROME=%ProgramFiles%\Google\Chrome\Application\chrome.exe"
set "CHROME86=%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe"

if exist "%EDGE%" (
    start "" "%EDGE%" --remote-debugging-port=%PORT% --user-data-dir="%PROFILE%" https://web.max.ru
) else if exist "%CHROME%" (
    start "" "%CHROME%" --remote-debugging-port=%PORT% --user-data-dir="%PROFILE%" https://web.max.ru
) else if exist "%CHROME86%" (
    start "" "%CHROME86%" --remote-debugging-port=%PORT% --user-data-dir="%PROFILE%" https://web.max.ru
) else (
    echo Edge/Chrome not found. Install one, or edit this script with the browser path.
    pause
)
