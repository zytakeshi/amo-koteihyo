package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// 資産名（GitHub は非 ASCII 名を落とすため ASCII 固定。build スクリプトと一致）。
const (
	assetExe = "AMO-koteihyo.exe"
	assetSha = "AMO-koteihyo.exe.sha256"
)

// 応答上限（§B2）。
const (
	capAPIBody int64 = 1 << 20   // GitHub API 本文 1MB
	capNotes   int64 = 8 * 1024  // リリースノート 8KB
	capSidecar int64 = 4 * 1024  // sha256 サイドカー 4KB
	capExe     int64 = 100 << 20 // 実行ファイル 100MiB
)

// release は /releases/latest の必要部分だけを取り出す。
type release struct {
	TagName string  `json:"tag_name"`
	Body    string  `json:"body"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// fetchLatest は最新リリースを取得する（prerelease は GitHub 側で除外済み）。
// 期待される外部失敗（ネット断・レート制限・非公開）は error として返し、
// 呼び出し側で「今回はスキップ」に落とす（§B2）。
func fetchLatest(client *http.Client, repo, version string) (*release, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AMO-koteihyo/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 応答コード %d", resp.StatusCode)
	}
	body, err := readCapped(resp, capAPIBody)
	if err != nil {
		return nil, err
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("リリース情報の解析に失敗しました: %w", err)
	}
	return &rel, nil
}

// matchAssets は同一リリースに exe と .sha256 の両方が揃っているかを検査する
// （両方無ければ「更新なし」扱い、§B2）。純関数。
func matchAssets(assets []asset) (exeURL, shaURL string, exeSize int64, ok bool) {
	for _, a := range assets {
		switch a.Name {
		case assetExe:
			exeURL, exeSize = a.URL, a.Size
		case assetSha:
			shaURL = a.URL
		}
	}
	if exeURL == "" || shaURL == "" {
		return "", "", 0, false
	}
	return exeURL, shaURL, exeSize, true
}

// readCapped は Content-Length を前検査し、さらに LimitReader で本体長も検証する
// （二重防御、§B2）。cap を超えたら弾く。
func readCapped(resp *http.Response, cap int64) ([]byte, error) {
	if resp.ContentLength > cap {
		return nil, fmt.Errorf("応答が大きすぎます（%d > %d）", resp.ContentLength, cap)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > cap {
		return nil, fmt.Errorf("応答が上限（%d）を超えました", cap)
	}
	return b, nil
}

// capString は文字列を最大 n バイトに丸める（リリースノートの上限用）。
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
