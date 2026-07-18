#!/usr/bin/env bash
# AMO BARBER 工程表 を Windows 用 (amd64) にクロスコンパイルする。
# 生成物: build/AMO-koteihyo.exe （コンソール表示あり= -H windowsgui は付けない）
#         build/AMO-koteihyo.exe.sha256 （ファイル名なしの 64 桁 sha256、自動更新の照合用）
# 配布時は同じフォルダの build/ファイアウォール許可.bat も一緒に配ること。
#
# 使い方:
#   bash build/build-windows.sh          リリースビルド（クリーンツリー + vX.Y.Z タグ必須）
#   bash build/build-windows.sh --dev    開発ビルド（version=dev-<shorthash>、検査を緩める）
set -euo pipefail

# このスクリプトの場所からプロジェクトルートへ移動する。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

DEV=0
for arg in "$@"; do
	case "${arg}" in
	--dev) DEV=1 ;;
	*)
		echo "不明な引数です: ${arg}（使えるのは --dev のみ）" >&2
		exit 2
		;;
	esac
done

# 配布する exe 名（GitHub は非ASCII名を落とすため ASCII。.bat もこの名前を参照）。
OUT="build/AMO-koteihyo.exe"
SHA="${OUT}.sha256"

if [ "${DEV}" -eq 1 ]; then
	# 開発ビルド: 版番号は dev-<shorthash>（非 canonical → アプリ側で自動適用は無効）。
	HASH="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
	VERSION="dev-${HASH}"
	echo "開発ビルド: version=${VERSION}"
else
	# リリースビルド: 作業ツリーがクリーンで、現在のコミットに正確な vX.Y.Z タグが
	# 必須（さもなくば中止）。半端な版番号を配らないためのゲート（§B1）。
	if [ -n "$(git status --porcelain)" ]; then
		echo "エラー: 作業ツリーがクリーンではありません。" >&2
		echo "  変更をコミットしてから再実行するか、開発ビルドなら --dev を付けてください。" >&2
		exit 1
	fi
	if ! TAG="$(git describe --tags --exact-match 2>/dev/null)"; then
		echo "エラー: 現在のコミットに正確なタグがありません。" >&2
		echo "  例: git tag v1.1.0 && bash build/build-windows.sh" >&2
		echo "  開発ビルドなら --dev を付けてください。" >&2
		exit 1
	fi
	if ! printf '%s' "${TAG}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
		echo "エラー: タグ '${TAG}' は vX.Y.Z 形式ではありません。" >&2
		exit 1
	fi
	VERSION="${TAG}"
	echo "リリースビルド: version=${VERSION}"
fi

echo "Windows 用にビルドしています (GOOS=windows GOARCH=amd64) ..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
	go build -ldflags "-X main.version=${VERSION}" -o "${OUT}" .

# sha256 サイドカー（ファイル名なしの 64 桁 16 進）。sha256sum / shasum のどちらでも。
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum "${OUT}" | awk '{print $1}' >"${SHA}"
elif command -v shasum >/dev/null 2>&1; then
	shasum -a 256 "${OUT}" | awk '{print $1}' >"${SHA}"
else
	echo "エラー: sha256sum も shasum も見つかりません（サイドカーを作成できません）。" >&2
	exit 1
fi

echo "ビルドに成功しました: ${ROOT_DIR}/${OUT}"
echo "  バージョン : ${VERSION}"
echo "  SHA256     : $(cat "${SHA}")"
echo "  サイドカー : ${ROOT_DIR}/${SHA}"
echo "この .exe を Windows PC にコピーし、ダブルクリックで起動してください。"
echo "GitHub リリースには .exe と .exe.sha256 の両方を添付してください（§B4）。"
