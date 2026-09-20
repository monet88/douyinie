@echo off
setlocal enabledelayedexpansion

echo ========================================================
echo  Douyinie Windows Desktop Build Script
echo ========================================================

set DIST_DIR=dist\douyinie-windows-x64
if exist "%DIST_DIR%" (
    echo [1/5] Cleaning existing build directory...
    rmdir /s /q "%DIST_DIR%"
)
mkdir "%DIST_DIR%"
mkdir "%DIST_DIR%\adapters"
mkdir "%DIST_DIR%\bin"
mkdir "%DIST_DIR%\models"

echo [2/5] Compiling Douyinie Desktop (Go + Wails)...
go build -ldflags="-s -w" -o "%DIST_DIR%\douyinie.exe" ./cmd/desktop
if %errorlevel% neq 0 (
    echo [ERROR] Failed to compile douyinie.exe
    exit /b %errorlevel%
)

echo [3/5] Compiling StageWorker...
go build -ldflags="-s -w" -o "%DIST_DIR%\stageworker.exe" ./cmd/stageworker
if %errorlevel% neq 0 (
    echo [ERROR] Failed to compile stageworker.exe
    exit /b %errorlevel%
)

echo [4/5] Copying runtime adapters and assets...
copy /y cmd\stageworker\adapters\*.py "%DIST_DIR%\adapters\" >nul 2>&1
copy /y wails.json "%DIST_DIR%\" >nul 2>&1

echo [5/5] Checking for Inno Setup compiler...
where ISCC >nul 2>&1
if %errorlevel% equ 0 (
    echo Compiling Windows Installer Douyinie_Setup.exe...
    if not exist "dist_installer" mkdir dist_installer
    ISCC installer\douyinie_setup.iss
    echo [SUCCESS] Windows Installer created in dist_installer\
) else (
    echo [INFO] Inno Setup compiler ISCC not found in PATH.
    echo Standalone portable distribution is ready at: %DIST_DIR%\
)

echo ========================================================
echo  Build complete!
echo ========================================================
