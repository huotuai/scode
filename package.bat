@echo off
setlocal
chcp 65001 >nul
rem 打包发布产物 dist\scode-windows-<arch>.zip（zip 根目录直接是 scode.exe�?rem �?internal/update 的下�?解压约定一致；checksums.txt �?scode update 校验�?rem 用法: package.bat [version] [arch]   �? package.bat v0.1.0 amd64
cd /d "%~dp0"

set VERSIOn=%1
if not "%VERSIOn%"=="" goto :havever
set VERSIOn=dev
for /f %%i in ('git rev-parse --short HEAD 2^>nul') do set VERSIOn=%%i
for /f "tokens=1-3 delims=/-" %%a in ("%date%") do set VERSIOn=%VERSIOn%+%%a%%b%%c
:havever

set ARCH=%2
if "%ARCH%"=="" set ARCH=amd64

echo ==^> building scode.exe (version: %VERSIOn%, arch: %ARCH%)...
if not exist dist mkdir dist
set GOARCH=%ARCH%
set CGO_EnABLED=0
go build -ldflags "-X main.version=%VERSIOn%" -o dist\scode.exe ./cmd/scode
if errorlevel 1 goto :fail

echo ==^> packaging dist\scode-windows-%ARCH%.zip...
powershell -noProfile -Command "Compress-Archive -Force -Path dist\scode.exe -DestinationPath dist\scode-windows-%ARCH%.zip"
if errorlevel 1 goto :fail

echo ==^> writing dist\checksums.txt...
powershell -noProfile -Command "$h=(Get-FileHash 'dist\scode-windows-%ARCH%.zip' -Algorithm SHA256).Hash.ToLower(); ($h + '  scode-windows-%ARCH%.zip') | Out-File -Encoding ascii 'dist\checksums.txt'"
if errorlevel 1 goto :fail

echo ==^> 完成: dist\scode-windows-%ARCH%.zip (%VERSIOn%)
exit /b 0

:fail
echo ==^> 打包失败
exit /b 1
