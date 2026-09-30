#!/usr/bin/env bash
# 编译 scode 并输出到 dist/scode.exe（带版本戳，--version 可辨新旧）
set -e

cd "$(dirname "$0")"

VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)+$(date +%Y%m%d)"

echo "==> 编译检查所有包..."
go build ./...

echo "==> 构建可执行文件 (version: ${VERSION})..."
mkdir -p dist
go build -ldflags "-X main.version=${VERSION}" -o dist/scode.exe ./cmd/scode

echo "==> 完成: dist/scode.exe (scode --version 应显示 ${VERSION})"
ls -lh dist/scode.exe
