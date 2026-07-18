// Command koteihyo は AMO BARBER 工程表アプリの単体サーバ。
//
// Windows 11 でダブルクリック起動する .exe を想定。データは exe と同階層の
// data/koteihyo.json に保存し、同一 LAN の iPad / iPhone / 他PCのブラウザから
// http://<PCのIP>:<port> でアクセスできる。外部サーバ無し・標準ライブラリのみ。
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"koteihyo/internal/api"
	"koteihyo/internal/store"
	"koteihyo/internal/update"
)

// version はアプリ版。リリースビルドは build/build-windows.sh が
// -ldflags "-X main.version=vX.Y.Z" で埋め込む（§B1）。既定は開発ビルド用の "dev"。
var version = "dev"

// updateRepo は自動アップデートの取得元（GitHub Releases）。
const updateRepo = "zytakeshi/amo-koteihyo"

// web ディレクトリ全体を実行ファイルに埋め込む。
// index.html 等が無くてもコンパイルできるよう "web" ディレクトリ自体を埋め込む
// （qrcode.min.js が既にあるため //go:embed は成立する）。
//
//go:embed web
var webRoot embed.FS

func main() {
	port := flag.Int("port", 8080, "待ち受けポート（使用中なら +1 して空きを探す）")
	open := flag.Bool("open", true, "起動時に既定ブラウザを自動で開く（false で抑止）")
	flag.Parse()

	// データファイルのパス: 実行ファイルと同じ階層の data/koteihyo.json。
	// store.Open はここで §B3.5 のアップグレード前バックアップも行う。
	dataPath := resolveDataPath()
	st, err := store.Open(dataPath)
	if err != nil {
		log.Fatalf("データの初期化に失敗しました: %v", err)
	}

	exePath, _ := os.Executable()
	restart := update.RestartMode()

	// ポート確保。通常は空きを探す（+1 フォールバックあり）。再起動モード（自動更新
	// 直後）は URL/QR を安定させるため同一ポートに固定し、+1 へは落とさない（§B3.3）。
	// bind-first: ファイアウォール設定より先に listen する（UAC 待ちで bind を
	// 妨げないため）。
	var ln net.Listener
	var actualPort int
	if restart {
		want := *port
		if p, ok := update.ExpectPort(); ok {
			want = p
		}
		ln, err = listenSamePort(want, 10*time.Second)
		if err != nil {
			log.Fatalf("ポート %d を確保できませんでした（他のプログラムが使用中の可能性）: %v", want, err)
		}
		actualPort = want
	} else {
		ln, actualPort, err = listenWithFallback(*port)
		if err != nil {
			log.Fatalf("ポートの確保に失敗しました: %v", err)
		}
	}

	// 接続用 IP（既定ルート優先）と全候補 URL を求める。
	lanIP, lanCandidates := selectLANIPv4s()
	lanURL := fmt.Sprintf("http://%s:%d", lanIP, actualPort)
	localURL := fmt.Sprintf("http://localhost:%d", actualPort)
	lanURLs := make([]string, 0, len(lanCandidates))
	for _, c := range lanCandidates {
		lanURLs = append(lanURLs, fmt.Sprintf("http://%s:%d", c.IP, actualPort))
	}
	if len(lanURLs) == 0 {
		lanURLs = []string{lanURL}
	}

	// web 配下を fs.Sub で切り出して配信する。
	webFS, err := fs.Sub(webRoot, "web")
	if err != nil {
		log.Fatalf("埋め込み web の取得に失敗しました: %v", err)
	}

	// 自動アップデート管理（§B）。apply シグナルは main が所有するバッファ済み
	// チャネルで受ける。
	applyCh := make(chan update.Plan, 1)
	mgr := update.New(version, updateRepo, exePath, applyCh, actualPort)
	if restart {
		// ポート確保に成功 → .old 後始末ゲートの一条件を満たす（§B3.4）。
		mgr.MarkRestartBound()
	}

	srv := api.New(st, webFS, actualPort, lanURL, lanURLs, version, mgr)
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	printBanner(lanURL, lanURLs, actualPort, dataPath)

	// ブラウザ自動オープン（localhost）。再起動モード（自動更新直後）では抑止する。
	if *open && !restart {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openBrowser(localURL)
		}()
	}

	// 更新確認（起動時 + 24 時間ごと）。起動をブロックしない。
	go mgr.StartupCheck()

	// Serve をゴルーチンで回し、apply シグナルと select する（§B3-2）。apply が来たら
	// graceful shutdown → serve 完了待ち → メインゴルーチン上で同期スワップ。
	serveCh := make(chan error, 1)
	go func() {
		serveCh <- httpServer.Serve(ln)
	}()

	// シャットダウン合図。apply を受け取ったら cancel し、以後の修復(UAC)を抑止する。
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	// Windows の受信許可（ファイアウォール）は serve を妨げないよう、listen/serve を
	// 開始した後に非同期で検査・修復する（bind-first、通常・再起動の両モード共通、§B3.3）。
	// 昇格(UAC)待ちで LAN 公開が遅れないようにするため、必ず serve の後に走らせる。
	go ensureWindowsFirewall(shutdownCtx, mgr, exePath, filepath.Join(filepath.Dir(dataPath), ".firewall_ok"))

	select {
	case plan := <-applyCh:
		// apply ハンドラが既にファイアウォールゲートを保持しているため、ここで修復完了を
		// 待つ必要はない（旧・新プロセスの同時修復を防ぐ、finding 2/12）。cancel は
		// 進行中/以後の修復(UAC)子プロセスを CommandContext 経由で中断・抑止する。
		shutdownCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpServer.Shutdown(ctx)
		cancel()
		<-serveCh
		// 同期実行。成功時のみ exit 0（goroutine では絶対に実行しない）。
		update.SwapAndStart(plan)
	case err := <-serveCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("サーバが停止しました: %v", err)
		}
	}
}

// listenSamePort は指定ポートを timeout まで（500ms 間隔で）粘って確保する。
// +1 フォールバックはしない（自動更新の再起動で URL/QR を安定させるため、§B3.3）。
func listenSamePort(port int, timeout time.Duration) (net.Listener, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("ポート番号が不正です: %d", port)
	}
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			return ln, nil
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// resolveDataPath は保存先 koteihyo.json の絶対/相対パスを返す。
//
// 方針: 本番は exe と同階層の data/ を最優先で使う。ただし読み取り専用フォルダ
// （Program Files 直下など）に置かれて data/ を作成・書込できない場合は、
// os.UserConfigDir()/AmoKoteihyo/ にフォールバックして起動できるようにする。
// go run 時など一時ディレクトリの場合はカレントの data/ を使う（開発時の利便性）。
func resolveDataPath() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		if !isTempDir(dir) {
			// exe 同階層 data/ が作成/書込可能ならそれを使う。
			if p, ok := usableDataPath(filepath.Join(dir, "data")); ok {
				return p
			}
			// 書込不能なら UserConfigDir にフォールバック。
			if cfg, cerr := os.UserConfigDir(); cerr == nil {
				if p, ok := usableDataPath(filepath.Join(cfg, "AmoKoteihyo")); ok {
					return p
				}
			}
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return filepath.Join(wd, "data", "koteihyo.json")
}

// usableDataPath は dir を作成し書込可能か実際に検査し、可能なら
// dir/koteihyo.json を返す。検査用の一時ファイルは後始末する。
func usableDataPath(dir string) (string, bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	probe := filepath.Join(dir, ".write_test")
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return filepath.Join(dir, "koteihyo.json"), true
}

// isTempDir は go run のビルド一時ディレクトリらしきパスを判定する。
func isTempDir(dir string) bool {
	tmp := os.TempDir()
	if tmp != "" && strings.HasPrefix(dir, tmp) {
		return true
	}
	// go-build キャッシュ配下も一時扱い。
	return strings.Contains(dir, "go-build")
}

// listenWithFallback は start から順にポートを試し、空いた最初のもので listen する。
// 0.0.0.0 で待ち受ける（LAN 公開）。
func listenWithFallback(start int) (net.Listener, int, error) {
	const maxTries = 50
	if start <= 0 || start > 65535 {
		start = 8080
	}
	for p := start; p < start+maxTries && p <= 65535; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("空きポートが見つかりませんでした（%d〜）", start)
}

// lanCandidate は接続候補となる LAN の IPv4 と、その NIC 名。
type lanCandidate struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
}

// selectLANIPv4s は iPad/iPhone 接続用の「最有力 IP」と「全候補」を返す。
//
// 単純な「最初の 192.168.x」だと、VirtualBox(192.168.56.1)・Hyper-V/WSL・
// Docker・モバイルホットスポット(192.168.137.1) 等の仮想NICを誤選択して
// スマホから到達できない IP を出してしまう。そこで OS の既定ルートで実際に
// 使われる送信元 IP（＝本物の Wi-Fi/有線）を最優先にし、加えて全候補も返して
// 接続画面でフォールバック表示できるようにする。標準ライブラリのみ。
func selectLANIPv4s() (string, []lanCandidate) {
	candidates := candidateLANIPv4s()
	if routeIP := defaultRouteIPv4(); routeIP != "" {
		// 既定ルートの IP を先頭へ。候補に無ければ先頭に追加する。
		ordered := make([]lanCandidate, 0, len(candidates)+1)
		var rest []lanCandidate
		found := false
		for _, c := range candidates {
			if c.IP == routeIP {
				found = true
				ordered = append(ordered, c)
			} else {
				rest = append(rest, c)
			}
		}
		if !found {
			ordered = append(ordered, lanCandidate{IP: routeIP, Interface: "default route"})
		}
		ordered = append(ordered, rest...)
		return routeIP, ordered
	}
	if len(candidates) > 0 {
		return candidates[0].IP, candidates
	}
	return "127.0.0.1", nil
}

// defaultRouteIPv4 は既定ルートで外部へ出るときの送信元 IPv4 を返す。
// UDP の connect はパケットを送らず経路だけを確定するので、これで実際に
// 使われるローカル IP（本物の Wi-Fi/有線）が分かる。失敗時は空文字。
func defaultRouteIPv4() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || !usablePrivateIPv4(addr.IP) {
		return ""
	}
	return addr.IP.To4().String()
}

// candidateLANIPv4s は到達可能性の高いプライベート IPv4 候補を集める。
// ダウン中・ループバック・仮想NICらしき名前の IF は除外する。
func candidateLANIPv4s() []lanCandidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []lanCandidate
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if likelyVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if !usablePrivateIPv4(ip) {
				continue
			}
			s := ip.To4().String()
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, lanCandidate{IP: s, Interface: iface.Name})
		}
	}
	return out
}

// usablePrivateIPv4 は RFC1918 のプライベート IPv4 かどうか。
func usablePrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
		return false
	}
	return ip4[0] == 10 ||
		(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
		(ip4[0] == 192 && ip4[1] == 168)
}

// likelyVirtualInterface は名前から仮想/VPN系の NIC を推定して除外する。
func likelyVirtualInterface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{
		"virtualbox", "vbox", "vmware", "hyper-v", "vethernet",
		"wsl", "docker", "wi-fi direct", "mobile hotspot",
		"tailscale", "zerotier", "wireguard", "tap", "tun", "vpn",
		"loopback", "teredo", "isatap",
	} {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// firewall 関連の固定 PowerShell スクリプト（§C2）。
//
// 方針:
//   - 規則名は exe パス（小文字正規化した絶対パス）の SHA-256 先頭 8 桁を含む
//     一意名 "AMO-koteihyo v2 <hash8>"。古い名前や移動後の stale 規則を検出できる。
//   - netsh の出力はロケール依存なので一切パースしない。非昇格の構造化 PowerShell
//     (Get-NetFirewallRule → *Filter cmdlet) の列挙値で検査し、終了コードで分岐する。
//   - 規則名・exe パスは環境変数 AMO_FW_RULE / AMO_FW_EXE で渡し、スクリプト本文へ
//     一切文字列補間しない（インジェクション防止、§C2 / iter-4 P1）。
//   - 修復は「厳密同名を削除 → 旧規則を許可リスト削除 → New-NetFirewallRule → 再検査」
//     を 1 回の昇格(UAC)トランザクションで実行する。UAC は 1 起動あたり最大 1 回。
const fwRulePrefix = "AMO-koteihyo v2 "

// fwValidateScript は「現在の規則がちょうど 1 つ、正しい属性で存在するか」を検査する
// 固定スクリプト。exit 0=正常 / 2=規則が無い(または複数) / 3=属性不一致。
const fwValidateScript = `$r = @(Get-NetFirewallRule -DisplayName $env:AMO_FW_RULE -ErrorAction SilentlyContinue)
if ($r.Count -ne 1) { exit 2 }
$app = @(($r[0] | Get-NetFirewallApplicationFilter).Program)
$addr = @(($r[0] | Get-NetFirewallAddressFilter).RemoteAddress)
$proto = @(($r[0] | Get-NetFirewallPortFilter).Protocol)
$ok = ($r[0].Enabled -eq 'True') -and ($r[0].Direction -eq 'Inbound') -and ($r[0].Action -eq 'Allow') -and ([string]$r[0].Profile -eq 'Any') -and ($app.Count -eq 1) -and ($app[0] -eq $env:AMO_FW_EXE) -and ($addr.Count -eq 1) -and ($addr[0] -eq 'LocalSubnet') -and ($proto.Count -eq 1) -and ($proto[0] -eq 'TCP')
if ($ok) { exit 0 } else { exit 3 }`

// fwRepairPrefix は昇格側で規則を（再）作成する固定スクリプト前半。末尾に
// fwValidateScript を連結し、終了コード＝検査結果とする。
// 旧規則の削除は規則ごとに try/catch で結果を捕捉し、失敗は名前＋理由を Write-Warning で
// 記録するが修復全体は止めない（§C 可観測性、finding 20）。対象 = 厳密同名（重複防止）／
// 旧名 'AMO-koteihyo'／別ハッシュの v2 規則。それ以外には一切触れない。
const fwRepairPrefix = `$rule = $env:AMO_FW_RULE
$exe = $env:AMO_FW_EXE
$stale = @(Get-NetFirewallRule -ErrorAction SilentlyContinue | Where-Object { ($_.DisplayName -eq $rule) -or ($_.DisplayName -eq 'AMO-koteihyo') -or (($_.DisplayName -match '^AMO-koteihyo v2 [0-9a-f]{8}$') -and ($_.DisplayName -ne $rule)) })
foreach ($s in $stale) {
  try { $s | Remove-NetFirewallRule -ErrorAction Stop } catch { if ($env:AMO_FW_LOG) { Add-Content -LiteralPath $env:AMO_FW_LOG -Encoding UTF8 -Value ('既存規則の削除に失敗: ' + $s.DisplayName + ': ' + $_.Exception.Message) } }
}
New-NetFirewallRule -DisplayName $rule -Direction Inbound -Action Allow -Enabled True -Profile Any -Protocol TCP -RemoteAddress LocalSubnet -Program $exe -ErrorAction SilentlyContinue | Out-Null
`

// fwElevateLauncher は非昇格の親から昇格 PowerShell を 1 回だけ起動する固定スクリプト。
// -EncodedCommand は環境変数 AMO_FW_ENC で受け取り本文へ補間しない。昇格子プロセスは
// 親の環境変数（AMO_FW_RULE / AMO_FW_EXE）を継承する。
const fwElevateLauncher = `$ErrorActionPreference = 'Stop'
$p = Start-Process -FilePath powershell -Verb RunAs -WindowStyle Hidden -Wait -PassThru -ArgumentList @('-NoProfile','-NonInteractive','-EncodedCommand',$env:AMO_FW_ENC)
exit $p.ExitCode`

// firewallMarker は §C2 のマーカー JSON（schema:2）。毎起動のライブ検査を「省略」する
// ためのものではなく、状態説明のために残す。
type firewallMarker struct {
	Schema   int    `json:"schema"`
	RuleName string `json:"ruleName"`
	ExePath  string `json:"exePath"`
}

// canonicalExePath は exe パスを小文字ハッシュ用に正規化する（filepath.Clean +
// EvalSymlinks best-effort）。.bat 側も同じアルゴリズムで同じ規則名を導く（§C2）。
func canonicalExePath(exePath string) string {
	p := filepath.Clean(exePath)
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != "" {
		p = resolved
	}
	return p
}

// firewallRuleName は正規化済み exe パスの小文字 SHA-256 先頭 8 桁から一意な規則名を作る。
func firewallRuleName(canonExe string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(canonExe)))
	return fwRulePrefix + hex.EncodeToString(sum[:])[:8]
}

// encodePowerShell は PowerShell -EncodedCommand 用に UTF-16LE→Base64 化する。
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i] = byte(v)
		b[2*i+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// runFirewallValidate は非昇格の固定検査スクリプトを実行し終了コードを返す。
// PowerShell 自体を起動できなければ (-1, err) を返す。
func runFirewallValidate(ruleName, exePath string) (int, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fwValidateScript)
	cmd.Env = append(os.Environ(), "AMO_FW_RULE="+ruleName, "AMO_FW_EXE="+exePath)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// runFirewallRepair は昇格(UAC)トランザクションを 1 回起動する。返り値はランチャの
// 終了コード（＝昇格側の再検査結果）。UAC 拒否や作成失敗は非ゼロになる。
// ctx はシャットダウンで（非昇格の）ランチャ子プロセスを中断するために使う
// （CommandContext、finding 2）。旧規則の削除失敗は昇格側（隠しウィンドウ）から見えない
// ため、AMO_FW_LOG で渡した決定的パスのファイルに書かせ、Go 側で読み出してログに出す
// （finding 5）。
//
// 受容する残余（round-3 P2c）: CommandContext はランチャは中断できるが、その先の
// 昇格された子（実際に New-NetFirewallRule を実行するプロセス）までは所有しない。
// キャンセル時に昇格側がランチャより長生きし得るが、無害である — 規則追加は冪等で、
// 次回起動のライブ検査で必ず整合が取り直され、apply/修復ゲートが同時スワップを防ぐ。
// その場合は診断ログを消さずに残し、次回起動の掃き出し（sweepFirewallRepairLog）で回収する。
func runFirewallRepair(ctx context.Context, logPath, ruleName, exePath string) (int, error) {
	enc := encodePowerShell(fwRepairPrefix + fwValidateScript)
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", fwElevateLauncher)
	cmd.Env = append(os.Environ(),
		"AMO_FW_RULE="+ruleName,
		"AMO_FW_EXE="+exePath,
		"AMO_FW_ENC="+enc,
		"AMO_FW_LOG="+logPath,
	)
	err := cmd.Run()
	// ランチャが「正常終了」した場合のみ診断ログを掃き出して消す（終了コード非ゼロも
	// ランチャ自身は走り切っているので正常）。ctx キャンセル/kill 時は消さず残し、次回
	// 起動の sweep で回収する（round-3 P2a）。
	if ctx.Err() == nil {
		drainFirewallRepairLog(logPath)
	}
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// firewallRepairLogPath は修復診断ログの決定的パス（<dataDir>/fw_repair.log）。
// os.CreateTemp を使わないのは、キャンセルで残ったログを次回起動から見つけられるように
// するため（round-3 P2b）。marker と同階層に置く。
func firewallRepairLogPath(markerPath string) string {
	if markerPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(markerPath), "fw_repair.log")
}

// sweepFirewallRepairLog は前回のキャンセル等で残った診断ログを回収（ログ出力＋削除）する。
// 毎起動、修復をスケジュールする前に呼ぶ（round-3 P2b）。
func sweepFirewallRepairLog(logPath string) { drainFirewallRepairLog(logPath) }

// drainFirewallRepairLog は昇格側が残した診断行を Go のログへ転記し、ファイルを消す。
// 昇格側は -Encoding UTF8 で書くため先頭の UTF-8 BOM を除いてから出力する（round-3 P2）。
func drainFirewallRepairLog(logPath string) {
	if logPath == "" {
		return
	}
	defer os.Remove(logPath)
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return
	}
	body := strings.TrimPrefix(string(data), "\ufeff") // UTF-8 BOM を除去。
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			log.Printf("ファイアウォール既存規則の整理: %s", line)
		}
	}
}

// writeFirewallMarker はマーカー JSON を atomic（temp→rename）に書く（best-effort）。
func writeFirewallMarker(path, ruleName, exePath string) {
	if path == "" {
		return
	}
	data, err := json.Marshal(firewallMarker{Schema: 2, RuleName: ruleName, ExePath: exePath})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// ensureWindowsFirewall は Windows 起動時に、この exe への LAN 受信を許可する規則を
// 検査し、無い/壊れていれば昇格(UAC)で 1 回だけ修復する（§C2、「箱から出してすぐ使える」）。
//
// 毎起動でライブ検査を行い、マーカーで検査を省略しない（stale 規則を確実に検出するため）。
// 正しく存在すれば UAC は出さずマーカーを更新して終わる。UAC を拒否されても致命的では
// なく、同梱の「ファイアウォール許可.bat」や手動許可にフォールバックできる。
func ensureWindowsFirewall(ctx context.Context, mgr *update.Manager, exePath, markerPath string) {
	if runtime.GOOS != "windows" || exePath == "" {
		return
	}
	canon := canonicalExePath(exePath)
	ruleName := firewallRuleName(canon)
	logPath := firewallRepairLogPath(markerPath)

	// 前回のキャンセル等で残った修復診断ログを、修復をスケジュールする前に回収する
	// （round-3 P2b）。無ければ何もしない。
	sweepFirewallRepairLog(logPath)

	// 毎起動のライブ検査（ロケール非依存の構造化検査）。マーカーはこれを省略しない。
	code, err := runFirewallValidate(ruleName, canon)
	if err != nil {
		// PowerShell を起動できない環境。検査も修復もできないので何もしない
		//（.bat / 手動許可のフォールバックに委ねる）。
		return
	}
	if code == 0 {
		// 既に正しい規則がある（verify-then-mark）。UAC は出さない。
		writeFirewallMarker(markerPath, ruleName, canon)
		return
	}

	// 更新の適用（＝シャットダウン）が始まっていたら UAC を出さない（finding 12）。
	if ctx.Err() != nil {
		return
	}
	// 修復ゲートを非ブロッキングで確保する。適用ハンドラや別の修復が保持していれば
	// 修復しない（apply の UAC ブロックを防ぐ、finding 2）。取得できたら保持したまま
	// 1 回だけ修復し、必ず解放する。
	if mgr == nil || !mgr.TryAcquireFirewallGate(ctx, 0) {
		return
	}
	defer mgr.ReleaseFirewallGate()
	if ctx.Err() != nil {
		return
	}

	// 規則が無い / 属性不一致 → 昇格トランザクションで 1 回だけ修復（UAC 最大 1 回）。
	fmt.Println("  ファイアウォールの許可を設定します…")
	fmt.Println("  →「許可しますか？」のポップアップが出たら『はい』を押してください。")
	repairCode, repairErr := runFirewallRepair(ctx, logPath, ruleName, canon)
	if repairErr != nil || repairCode != 0 {
		// UAC 拒否・作成失敗など。マーカーは書かず、次回起動で再試行する。
		return
	}
	// 昇格側 exit 0 に加え、非昇格での再検査が通ったときだけマーカーを書く。
	if code2, err2 := runFirewallValidate(ruleName, canon); err2 == nil && code2 == 0 {
		writeFirewallMarker(markerPath, ruleName, canon)
	}
}

// printBanner は §7 の起動バナーをコンソールに大きく表示する。
func printBanner(lanURL string, lanURLs []string, port int, dataPath string) {
	line := "============================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  AMO BARBER 工程表 が起動しました")
	fmt.Println("  このウィンドウは閉じないでください")
	fmt.Println("  iPad / iPhone は下のURLかQRコードでどうぞ")
	fmt.Printf("    →  %s\n", lanURL)
	// 候補が複数あれば、つながらない時のフォールバックとして表示する。
	others := 0
	for _, u := range lanURLs {
		if u == lanURL {
			continue
		}
		if others == 0 {
			fmt.Println("  （上でつながらない時は、こちらも試してください）")
		}
		fmt.Printf("    ・  %s\n", u)
		others++
	}
	fmt.Println(line)
	fmt.Printf("  バージョン : %s\n", version)
	fmt.Printf("  ポート     : %d\n", port)
	fmt.Printf("  データ保存 : %s\n", dataPath)
	fmt.Printf("  この端末用 : http://localhost:%d\n", port)
	if runtime.GOOS == "windows" {
		fmt.Println(line)
		fmt.Println("  ※ 初回起動時にファイアウォールの「許可しますか？」が出たら")
		fmt.Println("    『はい』を押してください（iPad/iPhone から接続するために必要）。")
		fmt.Println("  ※ 押しそびれて スマホからつながらない時は、同じフォルダの")
		fmt.Println("    『ファイアウォール許可.bat』を右クリック→「管理者として実行」で")
		fmt.Println("    同じ許可を追加できます。")
	}
	fmt.Println(line)
	fmt.Println()
}

// openBrowser は OS ごとに既定ブラウザで URL を開く。失敗しても致命的ではない。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// cmd /c start "" <url> （URL の & 等を安全に扱うため空タイトルを置く）。
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default: // linux 等
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ブラウザの自動起動に失敗しました（手動で %s を開いてください）: %v", url, err)
	}
}
