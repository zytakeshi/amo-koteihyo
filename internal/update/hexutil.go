package update

import "strings"

// parseSha256 はサイドカー本文から sha256 ダイジェストを厳密に取り出す（finding 15）。
//
// 受理するのは標準の sha256sum 形式のみ:
//   - "<64hex>"（ファイル名なし。build/build-windows.sh が出力する形）
//   - "<64hex>  <filename>"（ダイジェスト＋単一のファイル名フィールド）
//
// 先頭トークンが 64 桁 16 進であることを必須とし、フィールドが 3 つ以上（無関係な
// 追記や、ファイル名に空白を含む形など）は拒否する。無効なら空文字。
func parseSha256(body string) string {
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) == 0 || len(fields) > 2 {
		return ""
	}
	if !isHex64(fields[0]) {
		return ""
	}
	return strings.ToLower(fields[0])
}

// isHex64 はちょうど 64 桁の 16 進かどうか。
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
