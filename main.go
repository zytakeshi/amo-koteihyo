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

	// 空きポートを探す（0.0.0.0 で listen）。
	ln, actualPort, err := listenWithFallback(*port)
	if err != nil {
		log.Fatalf("ポートの確保に失敗しました: %v", err)
	}

	lanIP := firstLANIPv4()
	lanURL := fmt.Sprintf("http://%s:%d", lanIP, actualPort)
	localURL := fmt.Sprintf("http://localhost:%d", actualPort)

	// web 配下を fs.Sub で切り出して配信する。
	webFS, err := fs.Sub(webRoot, "web")
	if err != nil {
		log.Fatalf("埋め込み web の取得に失敗しました: %v", err)
	}

	srv := api.New(st, webFS, actualPort, lanURL)
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	printBanner(lanURL, actualPort, dataPath)

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

// firstLANIPv4 は QR/接続用に最適な IPv4 を選ぶ。
//
// 単純な「最初の非ループバック IPv4」だと VPN・仮想NIC・複数NIC 環境で
// iPad/iPhone から到達できない IP を選んでしまう。そこで RFC1918 の
// プライベート LAN 範囲（192.168/16, 172.16/12, 10/8）を優先し、
// 192.168 → 172 → 10 の順で選ぶ。リンクローカル(169.254/16)・ループバック・
// ダウン中IF は除外。プライベート候補が無ければ従来通り最初の非ループバック
// IPv4、それも無ければ "127.0.0.1" にフォールバックする。
func firstLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	// プライベート優先度ごとの最初の候補と、最初の非プライベート候補を集める。
	var pref192, pref172, pref10, fallback string
	for _, iface := range ifaces {
		// ダウン中・ループバックは除外。
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
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
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue // IPv6 はスキップ。
			}
			if ip4.IsLinkLocalUnicast() {
				continue // 169.254.x.x は除外。
			}
			switch {
			case ip4[0] == 192 && ip4[1] == 168:
				if pref192 == "" {
					pref192 = ip4.String()
				}
			case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
				if pref172 == "" {
					pref172 = ip4.String()
				}
			case ip4[0] == 10:
				if pref10 == "" {
					pref10 = ip4.String()
				}
			default:
				if fallback == "" {
					fallback = ip4.String() // 最初の非ループバック IPv4（最終手段）。
				}
			}
		}
	}
	switch {
	case pref192 != "":
		return pref192
	case pref172 != "":
		return pref172
	case pref10 != "":
		return pref10
	case fallback != "":
		return fallback
	default:
		return "127.0.0.1"
	}
}

// printBanner は §7 の起動バナーをコンソールに大きく表示する。
func printBanner(lanURL string, port int, dataPath string) {
	line := "============================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  AMO BARBER 工程表 が起動しました")
	fmt.Println("  このウィンドウは閉じないでください")
	fmt.Println("  iPad / iPhone は下のURLかQRコードでどうぞ")
	fmt.Printf("    →  %s\n", lanURL)
	fmt.Println(line)
	fmt.Printf("  ポート     : %d\n", port)
	fmt.Printf("  データ保存 : %s\n", dataPath)
	fmt.Printf("  この端末用 : http://localhost:%d\n", port)
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
