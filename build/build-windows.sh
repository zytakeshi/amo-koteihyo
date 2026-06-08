#!/usr/bin/env bash
# AMO BARBER 工程表 を Windows 用 (amd64) にクロスコンパイルする。
# 生成物: build/AMO工程表.exe （コンソール表示あり= -H windowsgui は付けない）
set -euo pipefail

# このスクリプトの場所からプロジェクトルートへ移動する。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

OUT="build/AMO工程表.exe"

echo "Windows 用にビルドしています (GOOS=windows GOARCH=amd64) ..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "${OUT}" .

echo "ビルドに成功しました: ${ROOT_DIR}/${OUT}"
echo "この .exe を Windows PC にコピーし、ダブルクリックで起動してください。"
