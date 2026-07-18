package update

import "regexp"

// semverRe は厳格な canonical semver（先頭 v、3成分、余分なし）にだけマッチする。
// dev / dirty / ハッシュ / 成分欠落 / 先頭ゴミ / prerelease サフィックスは不一致 →
// canonical でないと判定する（§B1）。
var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// parseSemver は canonical な "vMAJOR.MINOR.PATCH" を数値3成分に分解する。
// canonical でなければ ok=false（呼び出し側はこれで「比較不能」を判断する）。
func parseSemver(s string) (major, minor, patch int, ok bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, false
	}
	// 正規表現が \d+ を保証しているので、桁あふれ以外で失敗しない。
	// 桁あふれ（非現実的）も安全側に倒して比較不能扱いにする。
	a, ok1 := atoiSafe(m[1])
	b, ok2 := atoiSafe(m[2])
	c, ok3 := atoiSafe(m[3])
	if !ok1 || !ok2 || !ok3 {
		return 0, 0, 0, false
	}
	return a, b, c, true
}

// atoiSafe は非負整数を int へ。桁あふれや不正は ok=false。
func atoiSafe(s string) (int, bool) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000_000 { // 非現実的な巨大値は比較不能に倒す。
			return 0, false
		}
	}
	return n, true
}

// compareSemver は canonical 同士を数値比較する。どちらかが非 canonical なら
// ok=false（比較不能）。cmp は a<b で -1、a==b で 0、a>b で +1。
func compareSemver(a, b string) (cmp int, ok bool) {
	am, an, ap, oka := parseSemver(a)
	bm, bn, bp, okb := parseSemver(b)
	if !oka || !okb {
		return 0, false
	}
	switch {
	case am != bm:
		return sign(am - bm), true
	case an != bn:
		return sign(an - bn), true
	case ap != bp:
		return sign(ap - bp), true
	default:
		return 0, true
	}
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// newerAvailable は latest を current より新しいとみなすか。
//   - latest が非 canonical（不正な tag_name）→ 常に false（新しくない扱い、§B1）。
//   - current が非 canonical（dev/dirty/hash）→ true（最新を「あり」として提示し、
//     自動適用はしない＝手動更新リンク、§B2）。
//   - どちらも canonical → 数値比較で latest>current のときだけ true。
func newerAvailable(latest, current string) bool {
	if _, _, _, ok := parseSemver(latest); !ok {
		return false
	}
	if _, _, _, ok := parseSemver(current); !ok {
		return true
	}
	cmp, ok := compareSemver(latest, current)
	return ok && cmp > 0
}
