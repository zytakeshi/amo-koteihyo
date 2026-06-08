// Command koteihyo は AMO BARBER 工程表アプリの単体サーバ。
//
// Windows 11 でダブルクリック起動する .exe を想定。データは exe と同階層の
// data/koteihyo.json に保存し、同一 LAN の iPad / iPhone / 他PCのブラウザから
// http://<PCのIP>:<port> でアクセスできる。外部サーバ無し・標準ライブラリのみ。
package main

import (
	"embed"
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

	"koteihyo/internal/api"
	"koteihyo/internal/store"
)

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
	dataPath := resolveDataPath()
	st, err := store.Open(dataPath)
	if err != nil {
		log.Fatalf("データの初期化に失敗しました: %v", err)
	}

	// Windows では初回起動時に受信許可（ファイアウォール）を自動設定する。
	// これで iPad/iPhone から「箱から出してすぐ」つながる（UAC は初回のみ）。
	exePath, _ := os.Executable()
	ensureWindowsFirewall(exePath, filepath.Join(filepath.Dir(dataPath), ".firewall_ok"))

	// 空きポートを探す（0.0.0.0 で listen）。
	ln, actualPort, err := listenWithFallback(*port)
	if err != nil {
		log.Fatalf("ポートの確保に失敗しました: %v", err)
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

	srv := api.New(st, webFS, actualPort, lanURL, lanURLs)
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	printBanner(lanURL, lanURLs, actualPort, dataPath)

	// ブラウザ自動オープン（localhost）。サーバ起動と競合しないよう少し遅延。
	if *open {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openBrowser(localURL)
		}()
	}

	if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("サーバが停止しました: %v", err)
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

// ensureWindowsFirewall は Windows 初回起動時に、この exe への受信を許可する
// ファイアウォール規則を自動追加する（「箱から出してすぐ使える」ため）。
// 既に規則があれば何もしない（参照は管理者不要）。無ければ netsh を昇格(UAC)で
// 実行して追加する＝初回だけポップアップで「はい」を押せば以後は不要。
// UAC で拒否されても致命的ではなく、同梱の「ファイアウォール許可.bat」や
// 手動許可にフォールバックできる。
func ensureWindowsFirewall(exePath, markerPath string) {
	if runtime.GOOS != "windows" || exePath == "" {
		return
	}
	// 一度成功（または既存確認）したらマーカーを置き、毎起動で UAC を出さない。
	// （netsh show rule が管理者必須な環境でも毎回昇格を試みないための保険。）
	if markerPath != "" {
		if _, err := os.Stat(markerPath); err == nil {
			return
		}
	}
	const ruleName = "AMO-koteihyo"
	if out, err := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName).Output(); err == nil {
		if strings.Contains(string(out), ruleName) {
			touchMarker(markerPath) // 既に許可済み。
			return
		}
	}
	fmt.Println("  ファイアウォールの許可を設定します…")
	fmt.Println("  →「許可しますか？」のポップアップが出たら『はい』を押してください。")
	tokens := []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + ruleName, "dir=in", "action=allow",
		"program=" + exePath, "enable=yes", "profile=any",
	}
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		quoted = append(quoted, "'"+strings.ReplaceAll(t, "'", "''")+"'")
	}
	ps := "Start-Process -FilePath netsh -Verb RunAs -WindowStyle Hidden -Wait -ArgumentList " + strings.Join(quoted, ",")
	// 成功時のみマーカーを書く（UACで「いいえ」なら次回また促す）。失敗しても続行。
	if err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run(); err == nil {
		touchMarker(markerPath)
	}
}

// touchMarker は空のマーカーファイルを作る（best-effort）。
func touchMarker(path string) {
	if path == "" {
		return
	}
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); err == nil {
		_ = f.Close()
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
	fmt.Printf("  ポート     : %d\n", port)
	fmt.Printf("  データ保存 : %s\n", dataPath)
	fmt.Printf("  この端末用 : http://localhost:%d\n", port)
	if runtime.GOOS == "windows" {
		fmt.Println(line)
		fmt.Println("  ※ スマホからつながらない時は Windows の「許可」が必要です。")
		fmt.Println("    同じフォルダの『ファイアウォール許可.bat』を右クリック→")
		fmt.Println("    「管理者として実行」で一発で許可できます。")
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
