@echo off
setlocal
cd /d "%~dp0"
title SCode Package

rem ------------------------------------------------------------
rem  SCode Desktop one-click packaging launcher.
rem
rem  Why this file is ASCII-only:
rem  cmd.exe parses IF/FOR parenthesized BLOCKS byte-wise; any
rem  non-ASCII (Chinese) text inside a block can break parsing
rem  even after `chcp 65001`. So this .bat contains NO Chinese at
rem  all -- every Chinese message is printed by the Node script
rem  (scripts\package.mjs), which writes via WriteConsoleW and is
rem  codepage-independent. Chinese paths are safe: "%~dp0" etc.
rem  are handled as Unicode by cmd internally.
rem
rem  Usage:
rem    double-click            full package (dir + zip + installer)
rem    package.bat --skip-build         skip vite build
rem    package.bat --only=zip           zip only
rem    package.bat --scode-bin=<path>   custom backend binary
rem ------------------------------------------------------------

where node >nul 2>nul
if errorlevel 1 (
  echo [ERROR] Node.js not found in PATH. Please install Node.js first.
  pause
  exit /b 1
)

if not exist "node_modules\electron\dist\electron.exe" (
  echo [INFO] Dependencies missing, running npm install ...
  call npm install
  if errorlevel 1 (
    echo [ERROR] npm install failed.
    pause
    exit /b 1
  )
)

node scripts\package.mjs %*
if errorlevel 1 (
  echo.
  echo [FAILED] Packaging error, see log above.
  pause
  exit /b 1
)

echo.
echo [DONE] Artifacts are in the release\ directory.
pause
