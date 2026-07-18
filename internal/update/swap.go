package update

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Plan は main へ渡す「差し替え計画」。
type Plan struct {
	ExePath    string // 現在の実行ファイルの絶対パス
	TmpPath    string // 検証済みの新 exe（exeDir 内の一時ファイル）
	NewVersion string // 新バージョン（vX.Y.Z）
	Port       int    // 再起動後も維持すべきポート
}

// fsOps はスワップ状態機のファイル操作。テストでモック化できるよう抽象化する。
type fsOps interface {
	Exists(name string) bool
	Rename(oldPath, newPath string) error
	Remove(name string) error
}

type realFS struct{}

func (realFS) Exists(name string) bool  { _, err := os.Stat(name); return err == nil }
func (realFS) Rename(o, n string) error { return os.Rename(o, n) }
func (realFS) Remove(name string) error { return os.Remove(name) }

// launchFunc は差し替え後の exe を起動する（テストで差し替え可能）。
type launchFunc func(exe string, env []string) error

// swapResult は状態機の結果（純粋）。code はプロセス終了コード、messages は
// コンソールへ出す行（手動復旧の正確なパスを含む）。success は新版起動成功。
type swapResult struct {
	code     int
	success  bool
	messages []string
}

// swap は §B3.2b の状態機を「純粋な手続き」として実行する（os.Exit を含まない）。
// SwapAndStart から realFS/realLaunch/実 PID で呼ぶ本体。dry-run テスト可能。
//
// 不変条件: どの分岐でも os.Rename は必ず結果確認し、既存の宛先へは決して復元
// せず（never restore over existing）、失敗分岐では正確な手動復旧パスを表示して
// 非ゼロ終了する。成功（新版 Start()）のときだけ 0 を返す。
func swap(fs fsOps, launch launchFunc, plan Plan, pid int) swapResult {
	exe := plan.ExePath
	old := exe + ".old"
	tmp := plan.TmpPath

	// 手動復旧の案内（新・旧の両パスを明示）。
	manual := func(head ...string) []string {
		lines := append([]string{}, head...)
		return append(lines,
			"  手動で復旧してください。次のファイルを確認します:",
			"    新: "+exe,
			"    旧(退避): "+old,
			"  旧を元の名前に戻せば以前のバージョンで起動できます。",
		)
	}

	// (i) 残置 .old があれば削除。削除失敗 → live exe に触れる前に中止（無変更）。
	if fs.Exists(old) {
		if err := fs.Remove(old); err != nil {
			return swapResult{code: 1, messages: []string{
				"更新を中止しました（古い退避ファイルを削除できません）: " + err.Error(),
				"  ファイルは一切変更していません。もう一度更新してください。",
				"    退避: " + old,
			}}
		}
	}

	// (ii) exe → exe.old。失敗 → 中止（無変更）。
	if err := fs.Rename(exe, old); err != nil {
		return swapResult{code: 1, messages: []string{
			"更新を中止しました（現在の実行ファイルを退避できません）: " + err.Error(),
			"  ファイルは一切変更していません。もう一度更新してください。",
		}}
	}

	// (iii) tmp → exe。失敗 → old→exe へロールバック。ロールバックも失敗 →
	// 何も削除せず両パスを表示して exit 1。
	if err := fs.Rename(tmp, exe); err != nil {
		if rb := fs.Rename(old, exe); rb != nil {
			return swapResult{code: 1, messages: manual(
				"更新に失敗し、元に戻すこともできませんでした: "+err.Error(),
				"  復旧失敗: "+rb.Error(),
			)}
		}
		return swapResult{code: 1, messages: []string{
			"更新に失敗しましたが、元のバージョンに戻しました: " + err.Error(),
			"  そのまま起動し直してください。",
		}}
	}

	// (iv) 新 exe 起動。失敗 → 新 exe を衝突しない .failed 名へ退避 → exe 名が
	// 空いたら old→exe を復元 → 旧 exe を再起動。各段は失敗を確認する。
	env := restartEnv(plan)
	if err := launch(exe, env); err != nil {
		failed := failedName(fs, exe, plan.NewVersion, pid)
		if rn := fs.Rename(exe, failed); rn != nil {
			// 退避できない → old を既存の exe 名へ復元してはならない。中止。
			return swapResult{code: 1, messages: manual(
				"新バージョンの起動に失敗し、退避もできませんでした: "+err.Error(),
				"  退避失敗: "+rn.Error(),
			)}
		}
		// exe 名は空いた（今 failed へ退避した）。old を復元する。
		if r := fs.Rename(old, exe); r != nil {
			return swapResult{code: 1, messages: manual(
				"新バージョンの起動に失敗し、旧バージョンの復元にも失敗しました: "+err.Error(),
				"  復元失敗: "+r.Error(),
				"  退避した新版: "+failed,
			)}
		}
		// 旧 exe を再起動（復旧）。
		if rl := launch(exe, env); rl != nil {
			return swapResult{code: 2, messages: []string{
				"新バージョンの起動に失敗し、旧バージョンに戻しましたが自動起動できませんでした: " + err.Error(),
				"  AMO-koteihyo.exe をダブルクリックして起動し直してください。",
			}}
		}
		return swapResult{code: 2, messages: []string{
			"新バージョンの起動に失敗したため、旧バージョンで再起動しました: " + err.Error(),
			"  退避した新版: " + failed,
		}}
	}

	// 新版 Start() 成功 — ここだけが exit 0。
	return swapResult{code: 0, success: true, messages: []string{
		"更新が完了しました。新しいバージョン " + plan.NewVersion + " で再起動します。",
	}}
}

// SwapAndStart は main の（唯一の）exit 0 経路。状態機を実行し、結果行を表示して
// プロセスを終了する。同期実行される（goroutine 内では絶対に呼ばない）。
func SwapAndStart(plan Plan) {
	res := swap(realFS{}, realLaunch, plan, os.Getpid())
	for _, m := range res.messages {
		fmt.Println(m)
	}
	os.Exit(res.code)
}

// realLaunch は差し替え後の exe を新プロセスとして起動する。
func realLaunch(exe string, env []string) error {
	cmd := exec.Command(exe)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// restartEnv は再起動モードの環境変数を付与する（§B3.2c）。
func restartEnv(plan Plan) []string {
	env := os.Environ()
	env = append(env,
		envRestart+"=1",
		envExpectPort+"="+strconv.Itoa(plan.Port),
		envExpectVersion+"="+plan.NewVersion,
	)
	return env
}

// failedName は起動失敗した新 exe の退避名を返す。version+PID でも古い退避物と
// 衝突し得るため、存在しない名前になるまでカウンタ接尾辞を付けて確保する（finding 14）。
func failedName(fs fsOps, exe, ver string, pid int) string {
	base := exe + ".failed-" + sanitizeVer(ver) + "-" + strconv.Itoa(pid)
	if !fs.Exists(base) {
		return base
	}
	for i := 1; ; i++ {
		cand := base + "-" + strconv.Itoa(i)
		if !fs.Exists(cand) {
			return cand
		}
	}
}

// sanitizeVer はファイル名に使えない文字を除いた版番号を返す。
func sanitizeVer(ver string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return r
	}
	return strings.Map(repl, ver)
}
