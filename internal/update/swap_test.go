package update

import (
	"fmt"
	"testing"
)

// mockFS は swap 状態機の dry-run 用。存在集合を持ち、Rename/Remove で更新する。
// 失敗注入は Remove がパス単位、Rename が "src->dst" 単位。
type mockFS struct {
	existing  map[string]bool
	removeErr map[string]error
	renameErr map[string]error // key = old + "->" + new
	ops       []string
}

func newMockFS(present ...string) *mockFS {
	m := &mockFS{existing: map[string]bool{}, removeErr: map[string]error{}, renameErr: map[string]error{}}
	for _, p := range present {
		m.existing[p] = true
	}
	return m
}

func (m *mockFS) Exists(name string) bool { return m.existing[name] }

func (m *mockFS) Remove(name string) error {
	if e := m.removeErr[name]; e != nil {
		m.ops = append(m.ops, "rm-FAIL "+name)
		return e
	}
	delete(m.existing, name)
	m.ops = append(m.ops, "rm "+name)
	return nil
}

func (m *mockFS) Rename(o, n string) error {
	if e := m.renameErr[o+"->"+n]; e != nil {
		m.ops = append(m.ops, "mv-FAIL "+o+"->"+n)
		return e
	}
	if !m.existing[o] {
		m.ops = append(m.ops, "mv-NOSRC "+o+"->"+n)
		return fmt.Errorf("source missing: %s", o)
	}
	delete(m.existing, o)
	m.existing[n] = true
	m.ops = append(m.ops, "mv "+o+"->"+n)
	return nil
}

const (
	tExe = "/app/AMO-koteihyo.exe"
	tTmp = "/app/AMO-koteihyo-123.tmp"
	tPid = 999
	tVer = "v1.1.0"
)

var tOld = tExe + ".old"
var tFailed = tExe + ".failed-" + tVer + "-999"

func plan() Plan { return Plan{ExePath: tExe, TmpPath: tTmp, NewVersion: tVer, Port: 8080} }

func TestSwapHappyPath(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	launched := 0
	launch := func(exe string, env []string) error { launched++; return nil }
	res := swap(fs, launch, plan(), tPid)
	if !res.success || res.code != 0 {
		t.Fatalf("happy: code=%d success=%v", res.code, res.success)
	}
	if launched != 1 {
		t.Fatalf("happy: launched=%d", launched)
	}
	// 新 exe が所定、.old は復旧用に残す（後始末は bootstrap 200 後）、tmp は消費。
	if !fs.existing[tExe] || !fs.existing[tOld] || fs.existing[tTmp] {
		t.Fatalf("happy: bad final fs state: %+v", fs.existing)
	}
}

func TestSwapLeftoverOldRemoved(t *testing.T) {
	fs := newMockFS(tExe, tTmp, tOld) // 残置 .old
	res := swap(fs, func(string, []string) error { return nil }, plan(), tPid)
	if res.code != 0 {
		t.Fatalf("leftover: code=%d", res.code)
	}
	// 新 exe が所定、.old は最後に新版で作られたもの（旧 exe が退避された）。
	if !fs.existing[tExe] || !fs.existing[tOld] {
		t.Fatalf("leftover: fs=%+v", fs.existing)
	}
}

func TestSwapLeftoverOldRemoveFailsAborts(t *testing.T) {
	fs := newMockFS(tExe, tTmp, tOld)
	fs.removeErr[tOld] = fmt.Errorf("locked")
	res := swap(fs, func(string, []string) error { return nil }, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want abort code 1, got %d", res.code)
	}
	// live exe に触れていない（無変更）。
	if !fs.existing[tExe] || !fs.existing[tTmp] {
		t.Fatalf("abort should be no-op on exe/tmp: %+v", fs.existing)
	}
}

func TestSwapRenameExeToOldFailsAborts(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	fs.renameErr[tExe+"->"+tOld] = fmt.Errorf("denied")
	res := swap(fs, func(string, []string) error { return nil }, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want code 1, got %d", res.code)
	}
	if !fs.existing[tExe] || !fs.existing[tTmp] || fs.existing[tOld] {
		t.Fatalf("abort should be no-op: %+v", fs.existing)
	}
}

func TestSwapRenameTmpToExeFailsRollbackOK(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	fs.renameErr[tTmp+"->"+tExe] = fmt.Errorf("av lock")
	res := swap(fs, func(string, []string) error { return nil }, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want code 1, got %d", res.code)
	}
	// ロールバック成功 → exe 復元、.old 消滅。
	if !fs.existing[tExe] || fs.existing[tOld] {
		t.Fatalf("rollback should restore exe: %+v", fs.existing)
	}
}

func TestSwapRenameTmpToExeFailsRollbackFails(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	fs.renameErr[tTmp+"->"+tExe] = fmt.Errorf("av lock")
	fs.renameErr[tOld+"->"+tExe] = fmt.Errorf("rollback denied")
	res := swap(fs, func(string, []string) error { return nil }, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want code 1, got %d", res.code)
	}
	if !hasManualPaths(res.messages) {
		t.Fatalf("rollback-fail should print manual paths: %v", res.messages)
	}
}

func TestSwapLaunchFailsRecoversOld(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	calls := 0
	launch := func(exe string, env []string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("new build crashed on start")
		}
		return nil // 旧 exe の再起動は成功
	}
	res := swap(fs, launch, plan(), tPid)
	if res.code != 2 {
		t.Fatalf("recover: want code 2, got %d (%v)", res.code, res.messages)
	}
	// 新版は .failed へ、旧版は exe に復元、.old は消滅。
	if !fs.existing[tExe] || !fs.existing[tFailed] || fs.existing[tOld] {
		t.Fatalf("recover: fs=%+v", fs.existing)
	}
	if calls != 2 {
		t.Fatalf("recover: launch calls=%d", calls)
	}
}

func TestSwapLaunchFailsMoveAsideFails(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	fs.renameErr[tExe+"->"+tFailed] = fmt.Errorf("cannot move new exe")
	launch := func(exe string, env []string) error { return fmt.Errorf("crash") }
	res := swap(fs, launch, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want code 1, got %d", res.code)
	}
	if !hasManualPaths(res.messages) {
		t.Fatalf("should print manual paths: %v", res.messages)
	}
	// 既存の exe（新版）へ old を復元してはならない。
	if fs.existing[tExe] != true {
		t.Fatalf("new exe must remain (never restore over existing): %+v", fs.existing)
	}
}

func TestSwapLaunchFailsRestoreFails(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	fs.renameErr[tOld+"->"+tExe] = fmt.Errorf("restore denied")
	launch := func(exe string, env []string) error { return fmt.Errorf("crash") }
	res := swap(fs, launch, plan(), tPid)
	if res.code != 1 {
		t.Fatalf("want code 1, got %d", res.code)
	}
	if !hasManualPaths(res.messages) {
		t.Fatalf("should print manual paths: %v", res.messages)
	}
}

func TestSwapLaunchFailsRelaunchOldFails(t *testing.T) {
	fs := newMockFS(tExe, tTmp)
	launch := func(exe string, env []string) error { return fmt.Errorf("crash both") }
	res := swap(fs, launch, plan(), tPid)
	if res.code != 2 {
		t.Fatalf("relaunch-fail: want code 2, got %d", res.code)
	}
	// 旧版は exe へ戻っている（起動できなかっただけ）。
	if !fs.existing[tExe] {
		t.Fatalf("old should be restored: %+v", fs.existing)
	}
}

func hasManualPaths(msgs []string) bool {
	for _, m := range msgs {
		if containsAll(m, "旧(退避)") || containsAll(m, tOld) {
			return true
		}
	}
	return false
}

func containsAll(s, sub string) bool {
	return len(sub) > 0 && (len(s) >= len(sub)) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
