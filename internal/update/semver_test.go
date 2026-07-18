package update

import "testing"

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in         string
		maj, mi, p int
		ok         bool
	}{
		{"v1.0.0", 1, 0, 0, true},
		{"v1.10.0", 1, 10, 0, true},
		{"v0.0.1", 0, 0, 1, true},
		{"v12.34.56", 12, 34, 56, true},
		// 非 canonical はすべて ok=false。
		{"1.0.0", 0, 0, 0, false},    // 先頭 v 無し
		{"v1.0", 0, 0, 0, false},     // 成分欠落
		{"v1", 0, 0, 0, false},       // 成分欠落
		{"v1.0.0.0", 0, 0, 0, false}, // 成分過多
		{"dev", 0, 0, 0, false},
		{"dev-abc123", 0, 0, 0, false},
		{"v1.0.0-rc1", 0, 0, 0, false}, // prerelease サフィックス
		{"xv1.0.0", 0, 0, 0, false},    // 先頭ゴミ
		{"v1.0.0 ", 0, 0, 0, false},    // 末尾空白
		{"", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, mi, p, ok := parseSemver(c.in)
		if ok != c.ok || (ok && (maj != c.maj || mi != c.mi || p != c.p)) {
			t.Errorf("parseSemver(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, maj, mi, p, ok, c.maj, c.mi, c.p, c.ok)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.0.0", "v1.0.0", 0, true},
		{"v1.0.1", "v1.0.0", 1, true},
		{"v1.0.0", "v1.0.1", -1, true},
		{"v1.9.0", "v1.10.0", -1, true}, // 数値比較（文字列比較ではない）
		{"v1.10.0", "v1.9.0", 1, true},
		{"v2.0.0", "v1.99.99", 1, true},
		{"dev", "v1.0.0", 0, false}, // 比較不能
		{"v1.0.0", "dev", 0, false},
	}
	for _, c := range cases {
		cmp, ok := compareSemver(c.a, c.b)
		if ok != c.ok || (ok && cmp != c.cmp) {
			t.Errorf("compareSemver(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
	}
}

func TestNewerAvailable(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.1.0", "v1.0.0", true},   // 通常の新版
		{"v1.0.0", "v1.0.0", false},  // 同一
		{"v1.0.0", "v1.1.0", false},  // ダウングレード
		{"v1.10.0", "v1.9.0", true},  // 数値比較
		{"v1.1.0", "dev", true},      // current 非 canonical → 提示（手動）
		{"v1.1.0", "dev-abc", true},  // dirty/hash → 提示
		{"garbage", "v1.0.0", false}, // remote 不正 → 新しくない扱い
		{"v1.0", "v1.0.0", false},    // remote 成分欠落 → 新しくない扱い
		{"garbage", "dev", false},    // どちらも不正 → false
	}
	for _, c := range cases {
		if got := newerAvailable(c.latest, c.current); got != c.want {
			t.Errorf("newerAvailable(%q,%q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestMatchAssets(t *testing.T) {
	// 両方揃う。
	a := []asset{
		{Name: "AMO-koteihyo.exe", URL: "u-exe", Size: 5_000_000},
		{Name: "AMO-koteihyo.exe.sha256", URL: "u-sha", Size: 64},
		{Name: "README.md", URL: "u-readme"},
	}
	exeURL, shaURL, size, ok := matchAssets(a)
	if !ok || exeURL != "u-exe" || shaURL != "u-sha" || size != 5_000_000 {
		t.Fatalf("matchAssets both: got (%q,%q,%d,%v)", exeURL, shaURL, size, ok)
	}
	// exe だけ → 不成立。
	if _, _, _, ok := matchAssets([]asset{{Name: "AMO-koteihyo.exe", URL: "u"}}); ok {
		t.Error("matchAssets exe-only should be false")
	}
	// sha だけ → 不成立。
	if _, _, _, ok := matchAssets([]asset{{Name: "AMO-koteihyo.exe.sha256", URL: "u"}}); ok {
		t.Error("matchAssets sha-only should be false")
	}
	// 空 → 不成立。
	if _, _, _, ok := matchAssets(nil); ok {
		t.Error("matchAssets empty should be false")
	}
}

func TestParseSha256(t *testing.T) {
	const h = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	cases := []struct {
		in, want string
	}{
		{h, h},
		{h + "  AMO-koteihyo.exe\n", h},
		{"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", h}, // 大文字→小文字
		{"", ""},
		{"nothex", ""},
		{"tooShort", ""},
	}
	for _, c := range cases {
		if got := parseSha256(c.in); got != c.want {
			t.Errorf("parseSha256(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
