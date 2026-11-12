@echo off
echo.
echo  Icebath Installer
echo  Launching PowerShell installer...
echo.
powershell -ExecutionPolicy ByPass -NoProfile -Command "iex (irm https://icebath.app/install.ps1)"
if %ERRORLEVEL% NEQ 0 (
    echo.
    echo  Installation failed. Please try running PowerShell directly:
    echo    powershell -ExecutionPolicy ByPass -c "iex (irm https://icebath.app/install.ps1)"
    echo.
    pause
    exit /b 1
)
