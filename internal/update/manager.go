// Package update は GitHub Releases による自己アップデート（§B）を担う。
//
// 責務: 起動時＋24時間ごとの更新確認、状態の一元管理（1適用ずつ）、資産の
// ステージング（sha256 + MZ + サイズ検証）、そして main が同期実行するスワップ
// 状態機（swap.go）。ネットワーク層は期待される外部失敗として bounded に扱い、
// 失敗はコンソール1行に落として起動やサービスを止めない。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// 再起動モードの環境変数（§B3.2c / §B3.3）。
const (
	envRestart       = "AMO_UPDATE_RESTART"
	envExpectPort    = "AMO_EXPECT_PORT"
	envExpectVersion = "AMO_EXPECT_VERSION"
)

// Phase は更新処理の段階。
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseChecking    Phase = "checking"
	PhaseDownloading Phase = "downloading"
	PhaseStaged      Phase = "staged"
	PhaseApplying    Phase = "applying"
)

// 適用可否の理由（フロントの手動リンク分岐に使う）。
const (
	reasonManualRequired = "manual_required"
	reasonUnsupportedOS  = "unsupported_os"
)

// エラー分類（API のステータス割当に使う）。
var ErrApplyInProgress = errors.New("更新の適用が既に進行中です")

// NotApplicableError → 400（適用対象なし / 環境的に不可）。
type NotApplicableError struct{ Msg string }

func (e *NotApplicableError) Error() string { return e.Msg }

// StageFailError → 500（ダウンロード/検証の失敗）。
type StageFailError struct{ Msg string }

func (e *StageFailError) Error() string { return e.Msg }

// StatusView は /api/update/status のレスポンス（§B2）。
type StatusView struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
	Phase     Phase  `json:"phase"`
	CanApply  bool   `json:"canApply"`
}

// Manager は更新状態を一元管理する（全アクセスは mu 経由）。
type Manager struct {
	mu sync.Mutex

	current string // 実行中バージョン（ldflags 埋め込み）
	repo    string // "owner/name"
	exePath string
	exeDir  string
	port    int

	// 適用可否は起動時に確定（OS・版の canonical 性・exe ディレクトリの書込可否）。
	canApply bool
	reason   string

	// 直近の確認結果。
	latest    string
	notes     string
	available bool
	phase     Phase
	err       string
	lastCheck time.Time
	exeURL    string
	shaURL    string
	exeSize   int64

	// 再起動モード / .old 後始末（§B3.4）。
	restart       bool
	expectVersion string
	bound         bool
	cleanupOnce   sync.Once

	applyCh   chan Plan
	apiClient *http.Client
	dlClient  *http.Client

	// firewallGate はファイアウォール修復(UAC)と更新適用を調停する容量1のゲート
	// （finding 2）。修復は保持中のみ実行し、適用はステージング前に bounded wait で
	// 確保する。確保できなければ「修復進行中（UAC 待ち等）」→ busy を返す。適用が
	// 確保に成功したら保持したままスワップ（プロセス終了）へ進む。
	firewallGate chan struct{}
}

// New は Manager を生成し、適用可否を確定する。
func New(current, repo, exePath string, applyCh chan Plan, port int) *Manager {
	exeDir := filepath.Dir(exePath)
	m := &Manager{
		current:       current,
		repo:          repo,
		exePath:       exePath,
		exeDir:        exeDir,
		port:          port,
		phase:         PhaseIdle,
		restart:       RestartMode(),
		expectVersion: ExpectVersion(),
		applyCh:       applyCh,
		apiClient:     &http.Client{Timeout: 8 * time.Second},
		dlClient:      &http.Client{Timeout: 120 * time.Second},
		firewallGate:  make(chan struct{}, 1),
	}
	m.canApply, m.reason = computeCanApply(current, exePath, exeDir)
	return m
}

// TryAcquireFirewallGate はファイアウォール修復と更新適用を調停するゲートを、
// まず非ブロッキングで、次に wait まで（ctx キャンセルでも中断）確保する（finding 2）。
// 確保できたら true を返す。呼び出し側は使い終えるまで保持し ReleaseFirewallGate で
// 解放する（適用成功時はスワップ＝プロセス終了まで保持し続ける）。
func (m *Manager) TryAcquireFirewallGate(ctx context.Context, wait time.Duration) bool {
	select {
	case m.firewallGate <- struct{}{}:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case m.firewallGate <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// ReleaseFirewallGate はゲートを解放する（保持していないときの呼び出しは避けること）。
func (m *Manager) ReleaseFirewallGate() { <-m.firewallGate }

// AcquireApplyGate は適用ハンドラ用にゲートを最大2秒待って確保する（finding 2）。
// 修復(UAC)が進行中で時間内に確保できなければ false（→ ハンドラは busy を返す）。
func (m *Manager) AcquireApplyGate(ctx context.Context) bool {
	return m.TryAcquireFirewallGate(ctx, 2*time.Second)
}

// computeCanApply は自動適用の可否を決める（§B1/§B2）。
//   - Windows 以外 → 不可（unsupported_os）。
//   - 現在版が非 canonical（dev/dirty/hash）→ 不可（manual_required）。
//   - exe ディレクトリが書込不可（読取専用設置）→ 不可（manual_required）。
func computeCanApply(current, exePath, exeDir string) (bool, string) {
	if runtime.GOOS != "windows" {
		return false, reasonUnsupportedOS
	}
	if exePath == "" {
		return false, reasonManualRequired
	}
	if _, _, _, ok := parseSemver(current); !ok {
		return false, reasonManualRequired
	}
	if !probeWritable(exeDir) {
		return false, reasonManualRequired
	}
	return true, ""
}

// probeWritable は exeDir に対し create+rename+delete で書込可否を実検査する（§B2）。
func probeWritable(dir string) bool {
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".amo-write-*.tmp")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	renamed := name + ".r"
	if err := os.Rename(name, renamed); err != nil {
		_ = os.Remove(name)
		return false
	}
	// 後始末の失敗は「書込可」とみなさない（残置を記録して false、finding 19）。
	if err := os.Remove(renamed); err != nil {
		log.Printf("書込検査の一時ファイルを削除できませんでした（残置）: %s: %v", renamed, err)
		return false
	}
	return true
}

// Current は実行中バージョンを返す（bootstrap の version 用のヘルパ）。
func (m *Manager) Current() string { return m.current }

// StartupCheck は起動直後に1回＋以後24時間ごとに確認する（goroutine 前提）。
func (m *Manager) StartupCheck() {
	m.Check()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		m.Check()
	}
}

// Check は最新リリースを確認して状態を更新する。確認・適用は排他（PhaseIdle の
// ときだけ開始）。取得後、依然として PhaseChecking のときだけ結果を反映する
// （その間に適用が割り込んだ場合はこの確認は無効化＝superseded、§B1 finding 1）。
func (m *Manager) Check() {
	m.mu.Lock()
	if m.phase != PhaseIdle {
		m.mu.Unlock()
		return
	}
	m.phase = PhaseChecking
	m.lastCheck = time.Now()
	current, repo := m.current, m.repo
	m.mu.Unlock()

	rel, err := fetchLatest(m.apiClient, repo, current)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.phase != PhaseChecking {
		// 取得中に別操作（適用等）が phase を進めた → この確認結果は破棄する。
		return
	}
	m.phase = PhaseIdle
	if err != nil {
		m.err = err.Error()
		log.Printf("更新の確認をスキップしました: %v", err)
		return
	}
	m.err = ""
	m.latest = rel.TagName
	m.notes = capString(rel.Body, int(capNotes))
	exeURL, shaURL, size, ok := matchAssets(rel.Assets)
	if ok && newerAvailable(rel.TagName, current) {
		m.available = true
		m.exeURL, m.shaURL, m.exeSize = exeURL, shaURL, size
	} else {
		m.available = false
		m.exeURL, m.shaURL, m.exeSize = "", "", 0
	}
}

// Status は現在の状態を返す。古ければ（未確認 or 1時間超）非同期に確認を促す
// （lazy check、§B2）。ハンドラをブロックしない。
func (m *Manager) Status() StatusView {
	m.mu.Lock()
	v := StatusView{
		Current:   m.current,
		Latest:    m.latest,
		Available: m.available,
		Notes:     m.notes,
		Phase:     m.phase,
		CanApply:  m.canApply,
	}
	stale := m.lastCheck.IsZero() || time.Since(m.lastCheck) > time.Hour
	idle := m.phase == PhaseIdle
	m.mu.Unlock()
	if stale && idle {
		go m.Check()
	}
	return v
}

// Stage は資産をダウンロード＋検証して差し替え計画を用意する（§B3-1）。
// 適用は同時に1つだけ（進行中は ErrApplyInProgress → 409）。
func (m *Manager) Stage() (Plan, string, error) {
	m.mu.Lock()
	if !m.canApply {
		m.mu.Unlock()
		return Plan{}, "", &NotApplicableError{Msg: "この環境では自動更新できません（手動で更新してください）"}
	}
	// 確認・適用は排他。PhaseIdle でなければ（確認中を含め）進行中扱いにする
	// （§B1 finding 1: Check と Stage の同時進行を禁止する）。
	if m.phase != PhaseIdle {
		m.mu.Unlock()
		return Plan{}, "", ErrApplyInProgress
	}
	if !m.available || m.exeURL == "" || m.shaURL == "" {
		m.mu.Unlock()
		return Plan{}, "", &NotApplicableError{Msg: "適用できる更新がありません"}
	}
	m.phase = PhaseDownloading
	exeURL, shaURL, ver, port := m.exeURL, m.shaURL, m.latest, m.port
	m.mu.Unlock()

	tmp, err := m.stageDownload(exeURL, shaURL)
	if err != nil {
		m.mu.Lock()
		m.phase = PhaseIdle
		m.err = err.Error()
		m.mu.Unlock()
		return Plan{}, "", &StageFailError{Msg: err.Error()}
	}

	m.mu.Lock()
	m.phase = PhaseStaged
	m.mu.Unlock()
	return Plan{ExePath: m.exePath, TmpPath: tmp, NewVersion: ver, Port: port}, ver, nil
}

// Signal は差し替え計画を main へ渡す（バッファ済みチャネル、非ブロッキング）。
// ハンドラがレスポンスを flush して return した後に呼ぶ（§B3-2）。
func (m *Manager) Signal(plan Plan) {
	m.mu.Lock()
	m.phase = PhaseApplying
	m.mu.Unlock()
	m.applyCh <- plan
}

// MarkRestartBound は再起動モードでポート確保に成功したことを記録する（§B3.4 の
// 後始末ゲートの一条件）。
func (m *Manager) MarkRestartBound() {
	m.mu.Lock()
	m.bound = true
	m.mu.Unlock()
}

// NotifyBootstrapServed は最初の /api/bootstrap 200 応答後に呼ばれ、
// 条件が揃っていれば旧 exe（.old）を後始末する（§B3.4、sync.Once）。
// 全条件: 再起動モード・ポート確保済み・実行版 == AMO_EXPECT_VERSION。
func (m *Manager) NotifyBootstrapServed() {
	m.cleanupOnce.Do(func() {
		m.mu.Lock()
		ok := m.restart && m.bound && m.expectVersion != "" && m.current == m.expectVersion
		exePath := m.exePath
		m.mu.Unlock()
		if !ok {
			return
		}
		old := exePath + ".old"
		if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
			// 失敗は記録のみ。次回の更新サイクル 2b(i) で再削除される。
			log.Printf("旧バージョン（%s）の後始末に失敗しました（次回更新時に再試行）: %v", old, err)
		}
	})
}

// ---- ステージング（ダウンロード + 検証） ----

// stageDownload は exe を exeDir 内の一時ファイルへストリームし、sha256（サイド
// カー完全一致）・MZ マジック・サイズ>1MB を検証する（§B3-1）。失敗時は tmp を削除。
func (m *Manager) stageDownload(exeURL, shaURL string) (string, error) {
	want, err := m.fetchSha(shaURL)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, exeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "AMO-koteihyo/"+m.current)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := m.dlClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ダウンロード応答コード %d", resp.StatusCode)
	}
	if resp.ContentLength > capExe {
		return "", fmt.Errorf("更新ファイルが大きすぎます（%d > %d）", resp.ContentLength, capExe)
	}

	f, err := os.CreateTemp(m.exeDir, "AMO-koteihyo-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, capExe+1))
	if err != nil {
		cleanup()
		return "", err
	}
	if n > capExe {
		cleanup()
		return "", fmt.Errorf("更新ファイルが上限（%d）を超えました", capExe)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	// sha256 完全一致。
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("チェックサムが一致しません（ダウンロード破損の可能性）")
	}
	// サイズ > 1MB。
	if n <= 1<<20 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("更新ファイルが小さすぎます（%d バイト）", n)
	}
	// MZ マジック（Windows 実行ファイル）。
	if !hasMZMagic(tmp) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("更新ファイルの形式が不正です（実行ファイルではありません）")
	}
	return tmp, nil
}

// fetchSha は .sha256 サイドカーを取得し、最初の 64 桁 16 進を小文字で返す。
func (m *Manager) fetchSha(shaURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, shaURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "AMO-koteihyo/"+m.current)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := m.apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("サイドカー応答コード %d", resp.StatusCode)
	}
	body, err := readCapped(resp, capSidecar)
	if err != nil {
		return "", err
	}
	sum := parseSha256(string(body))
	if sum == "" {
		return "", fmt.Errorf("sha256 サイドカーの解析に失敗しました")
	}
	return sum, nil
}

// hasMZMagic は先頭2バイトが "MZ" かを確認する。
func hasMZMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return b[0] == 'M' && b[1] == 'Z'
}

// ---- 再起動モードの環境変数ヘルパ ----

// RestartMode は AMO_UPDATE_RESTART=1 かどうか。
func RestartMode() bool { return os.Getenv(envRestart) == "1" }

// ExpectPort は再起動後も維持すべきポート（未設定/不正なら ok=false）。
func ExpectPort() (int, bool) {
	v := os.Getenv(envExpectPort)
	if v == "" {
		return 0, false
	}
	p, err := strconv.Atoi(v)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

// ExpectVersion は再起動後に期待されるバージョン（.old 後始末ゲート用）。
func ExpectVersion() string { return os.Getenv(envExpectVersion) }
