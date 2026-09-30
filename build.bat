@echo off
chcp 65001 >nul
rem 编译 scode 并输出到 dist/scode.exe（带版本戳，--version 可辨新旧）
cd /d "%~dp0"

set VERSION=dev
for /f %%i in ('git rev-parse --short HEAD 2^>nul') do set VERSION=%%i
for /f "tokens=1-3 delims=/-" %%a in ("%date%") do set VERSION=%VERSION%+%%a%%b%%c

echo ==^> building...
go build ./...
if errorlevel 1 goto :fail

echo ==^> 构建可执行文件 (version: %VERSION%)...
if not exist dist mkdir dist
go build -ldflags "-X main.version=%VERSION%" -o dist/scode.exe ./cmd/scode
if errorlevel 1 goto :fail

echo ==^> 完成: dist/scode.exe (scode --version 应显示 %VERSION%)
exit /b 0

:fail
echo ==^> 编译失败
exit /b 1
