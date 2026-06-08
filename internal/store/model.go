package store

// Staff は1人のスタッフ（理容師）を表す。
type Staff struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Order  int    `json:"order"`
	Active bool   `json:"active"`
}

// Process は1つの工程（メニュー）を表す。単価つき。
type Process struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Price  int    `json:"price"`
	Order  int    `json:"order"`
	Active bool   `json:"active"`
}

// Event は append-only の監査ログ1件。undo/redo/revert もここに残る（履歴ぜんぶ保存）。
//
// prev / next は変更前後の値。種別により int / string / JSON のいずれか。
// interface{} で受けて JSON ラウンドトリップしても落ちないようにしている。
type Event struct {
	ID        string      `json:"id"`
	TS        string      `json:"ts"`                  // 操作時刻（サーバ時計, RFC3339 / JST）
	BizDate   string      `json:"bizDate"`             // 対象営業日（履歴の日付管理キー）
	StaffID   string      `json:"staffId"`             // 対象スタッフ（設定変更は "" 可）
	Kind      string      `json:"kind"`                // 種別
	ProcessID string      `json:"processId,omitempty"` // 該当時のみ
	Prev      interface{} `json:"prev"`                // 変更前
	Next      interface{} `json:"next"`                // 変更後
	Label     string      `json:"label"`               // 人が読める要約
}

// DB は永続化されるトップレベル構造。
type DB struct {
	Version   int        `json:"version"`
	Staff     []*Staff   `json:"staff"`
	Processes []*Process `json:"processes"`
	// 現在値。キー = "<staffId>|<YYYY-MM-DD>|<processId>"
	Counts map[string]int `json:"counts"`
	// 備考。キー = "<staffId>|<YYYY-MM-DD>"
	Memos map[string]string `json:"memos"`
	// append-only 監査ログ（絶対に削除しない）
	Events []*Event `json:"events"`
	// undo で取り消したものを redo 用に退避
	Redo []*Event `json:"redo"`
	// id 採番用カウンタ
	Seq int `json:"seq"`
}
