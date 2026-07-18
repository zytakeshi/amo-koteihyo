package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// JST は tzdata 非依存で固定オフセットの日本標準時。
// time.LoadLocation は使わない（Windows/最小環境でも確実に動かすため）。
var JST = time.FixedZone("JST", 9*60*60)

// dateFmt は API 全体で使う日付フォーマット（YYYY-MM-DD）。
const dateFmt = "2006-01-02"

// 件数・単価の実用上限。理容室1店舗の規模では十分大きく、かつ
// 件数×単価や合計の集計が int(64bit) でオーバーフローしないようにする上限。
const (
	maxCount = 1_000_000   // 1セルあたりの件数上限
	maxPrice = 100_000_000 // 1工程あたりの単価上限（円）
)

// Store は DB の保護・永続化・全ビジネスロジックを担う。
// 全操作を単一 Mutex で直列化する。
type Store struct {
	mu   sync.Mutex
	db   *DB
	path string // data/koteihyo.json への絶対/相対パス
	// failSave はテスト専用の save 失敗注入フラグ。true の間 save() は IO を行わず
	// エラーを返す（チェックポイント復元の検証用）。本番では常に false。
	failSave bool
	// lastRollingDate は日次バックアップを最後に処理した YYYYMMDD（§B3.5-2）。
	// 同一日の 2 回目以降の save で stat を省くための面内キャッシュ。
	lastRollingDate string
}

// Open はデータファイルを読み込む（無ければ初期シードを投入して保存）。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("データフォルダ作成失敗: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.db = seed()
			if err := s.save(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("データ読み込み失敗: %w", err)
	}
	// ダウングレード安全網（§B3.5-1）: normalize 前の生バイトでタイムカード列の
	// 有無を判定し、旧フォーマットなら一度だけ退避する。失敗は起動中止。
	if err := maybePreUpgradeBackup(path, data); err != nil {
		return nil, err
	}
	var db DB
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("データ解析失敗: %w", err)
	}
	normalize(&db)
	s.db = &db
	return s, nil
}

// seed は初回起動時の初期データ（§1）。7工程＋単価、スタッフ3名（小池・井上・浜田）。
func seed() *DB {
	db := &DB{
		Version:       1,
		Staff:         []*Staff{},
		Processes:     []*Process{},
		Counts:        map[string]int{},
		Memos:         map[string]string{},
		Attendance:    map[string]*DayAttendance{},
		AttendanceRev: map[string]int{},
		TCConfig:      defaultTCConfig(),
		Events:        []*Event{},
		Redo:          []*Event{},
		Seq:           0,
	}
	procs := []struct {
		name  string
		price int
	}{
		{"シャンプー", 600},
		{"シェービング", 600},
		{"ヘッドスパ", 2500},
		{"肩マッサージ", 400},
		{"パック", 500},
		{"ピーリング", 500},
		{"造顔マッサージ", 2500},
	}
	for i, p := range procs {
		db.Seq++
		db.Processes = append(db.Processes, &Process{
			ID:     fmt.Sprintf("p_%d", db.Seq),
			Name:   p.name,
			Price:  p.price,
			Order:  i,
			Active: true,
		})
	}
	staffNames := []string{"小池", "井上", "浜田"}
	for i, name := range staffNames {
		db.Seq++
		db.Staff = append(db.Staff, &Staff{
			ID:     fmt.Sprintf("s_%d", db.Seq),
			Name:   name,
			Order:  i,
			Active: true,
		})
	}
	return db
}

// normalize は古い/壊れたファイルでも nil マップ等で落ちないよう防御的に整える。
func normalize(db *DB) {
	if db.Version == 0 {
		db.Version = 1
	}
	if db.Counts == nil {
		db.Counts = map[string]int{}
	}
	if db.Memos == nil {
		db.Memos = map[string]string{}
	}
	// 勤怠系コンテナの nil セーフ化。値（事実）は書き換えない（§A6）。
	if db.Attendance == nil {
		db.Attendance = map[string]*DayAttendance{}
	}
	if db.AttendanceRev == nil {
		db.AttendanceRev = map[string]int{}
	}
	if db.TCConfig == nil {
		db.TCConfig = defaultTCConfig()
	}
	// 手編集などで attendance の値が null（*DayAttendance が nil）のキーが混じって
	// いると後段の参照で panic し得る。コンテナ衛生としてそのキーを落とす
	// （「欠勤」= キー欠落なので事実の書き換えにはならない）。
	for k, v := range db.Attendance {
		if v == nil {
			delete(db.Attendance, k)
		}
	}
	// 壊れた/手編集されたファイルに null 要素（"staff":[null] 等）が混じって
	// いても、後段のソート/履歴/undo で nil 参照 panic にならないよう除去する。
	db.Events = compactNonNil(db.Events)
	db.Redo = compactNonNil(db.Redo)
	db.Staff = compactNonNil(db.Staff)
	db.Processes = compactNonNil(db.Processes)
	// 非正の件数は無効（setCount と同じ規約）。混入していたら捨てる。
	for k, v := range db.Counts {
		if v <= 0 {
			delete(db.Counts, k)
		}
	}
}

// compactNonNil は nil 要素を除いた新しいスライスを返す（nil 入力も安全）。
func compactNonNil[T any](in []*T) []*T {
	out := make([]*T, 0, len(in))
	for _, p := range in {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// save は一時ファイルに書いて fsync してから os.Rename で atomic に置き換える。
// 自動保存の台帳なので、電源断でも直近の保存が失われないよう本体・親ディレクトリ
// ともにディスクへ確実に書き出す（durability）。呼び出し側で mu を保持していること。
func (s *Store) save() error {
	if s.failSave {
		// テスト注入（チェックポイント復元の検証用）。本番では常に false。
		return fmt.Errorf("保存に失敗しました（注入）")
	}
	data, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return fmt.Errorf("データ整形失敗: %w", err)
	}
	tmp := s.path + ".tmp"
	// 一時ファイルを開き、Write → Sync（fsync）→ Close の順で本体を確実に
	// ディスクへ書いてから rename する。途中で失敗したら tmp を片付ける。
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("一時ファイル作成失敗: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("一時ファイル書き込み失敗: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("一時ファイル同期失敗: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("一時ファイルクローズ失敗: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("ファイル置換失敗: %w", err)
	}
	// rename 自体を永続化するため親ディレクトリも fsync する。
	// （Windows ではディレクトリの Sync は no-op / エラーになり得るので無視。）
	if dir, derr := os.Open(filepath.Dir(s.path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	// 保存成功後に日次ローリングバックアップ（§B3.5-2、非致命）。
	s.rollingBackup(data)
	return nil
}

// cloneDB は DB 全体を JSON ラウンドトリップで深くコピーする。
// undo/redo/revert・タイムカード系の「全か無か」チェックポイントに使う
// （このデータ規模では十分安価）。marshal/unmarshal はプレーンな型のみなので
// 失敗はプログラマエラー → fail loud（panic）。
func cloneDB(db *DB) *DB {
	data, err := json.Marshal(db)
	if err != nil {
		panic(fmt.Sprintf("cloneDB marshal 失敗: %v", err))
	}
	var c DB
	if err := json.Unmarshal(data, &c); err != nil {
		panic(fmt.Sprintf("cloneDB unmarshal 失敗: %v", err))
	}
	return &c
}

// nextID は連番 ID を発行する（決定的）。
func (s *Store) nextID(prefix string) string {
	s.db.Seq++
	if prefix == "e" {
		return fmt.Sprintf("e_%06d", s.db.Seq)
	}
	return fmt.Sprintf("%s_%d", prefix, s.db.Seq)
}

// nowTS は JST の現在時刻を RFC3339 で返す。
func nowTS() string {
	return time.Now().In(JST).Format(time.RFC3339)
}

// ---- キー組み立て ----

func countKey(staffID, date, processID string) string {
	return staffID + "|" + date + "|" + processID
}

func memoKey(staffID, date string) string {
	return staffID + "|" + date
}

// ---- 週・日付ユーティリティ ----

// MondayOf はその日付を含む週の月曜（週起点）を YYYY-MM-DD で返す。
func MondayOf(date string) (string, error) {
	t, err := time.ParseInLocation(dateFmt, date, JST)
	if err != nil {
		return "", fmt.Errorf("日付が不正です: %s", date)
	}
	// 月曜=0 になるようにオフセット（time.Weekday は日曜=0）。
	offset := (int(t.Weekday()) + 6) % 7
	mon := t.AddDate(0, 0, -offset)
	return mon.Format(dateFmt), nil
}

// Today は JST の今日を YYYY-MM-DD で返す。
func Today() string {
	return time.Now().In(JST).Format(dateFmt)
}

// weekDates は週起点(月曜)から7日分の日付を返す。
func weekDates(start string) ([]string, error) {
	t, err := time.ParseInLocation(dateFmt, start, JST)
	if err != nil {
		return nil, fmt.Errorf("日付が不正です: %s", start)
	}
	dates := make([]string, 7)
	for i := 0; i < 7; i++ {
		dates[i] = t.AddDate(0, 0, i).Format(dateFmt)
	}
	return dates, nil
}

// dateRange は start..end（両端含む）の日付列を返す。
func dateRange(start, end string) ([]string, error) {
	st, err := time.ParseInLocation(dateFmt, start, JST)
	if err != nil {
		return nil, fmt.Errorf("開始日が不正です: %s", start)
	}
	en, err := time.ParseInLocation(dateFmt, end, JST)
	if err != nil {
		return nil, fmt.Errorf("終了日が不正です: %s", end)
	}
	if en.Before(st) {
		return nil, fmt.Errorf("終了日が開始日より前です")
	}
	var out []string
	for d := st; !d.After(en); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dateFmt))
	}
	return out, nil
}

// ---- 公開スナップショット取得（読み取り系） ----

// Bootstrap 用データ。
type BootstrapData struct {
	Staff     []*Staff   `json:"staff"`
	Processes []*Process `json:"processes"`
	Today     string     `json:"today"`
	WeekStart string     `json:"weekStart"`
}

// Bootstrap は起動時にフロントへ渡す初期データ（port/lanURL は api 側で付与）。
func (s *Store) Bootstrap() *BootstrapData {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := Today()
	ws, _ := MondayOf(today)
	return &BootstrapData{
		Staff:     s.activeStaffSorted(),
		Processes: s.activeProcessesSorted(),
		Today:     today,
		WeekStart: ws,
	}
}

// activeStaffSorted は active なスタッフを order 昇順で返す（コピー）。
func (s *Store) activeStaffSorted() []*Staff {
	var out []*Staff
	for _, st := range s.db.Staff {
		if st.Active {
			c := *st
			out = append(out, &c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	if out == nil {
		out = []*Staff{}
	}
	return out
}

// activeProcessesSorted は active な工程を order 昇順で返す（コピー）。
func (s *Store) activeProcessesSorted() []*Process {
	var out []*Process
	for _, p := range s.db.Processes {
		if p.Active {
			c := *p
			out = append(out, &c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	if out == nil {
		out = []*Process{}
	}
	return out
}

// WeekData は /api/week の返り値。
type WeekData struct {
	Start   string           `json:"start"`
	Dates   []string         `json:"dates"`
	StaffID string           `json:"staffId"`
	Counts  map[string][]int `json:"counts"`
	Memos   []string         `json:"memos"`
	CanUndo bool             `json:"canUndo"`
	CanRedo bool             `json:"canRedo"`
}

// Week は指定スタッフ・週起点(月曜)のグリッドを返す。
func (s *Store) Week(start, staffID string) (*WeekData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dates, err := weekDates(start)
	if err != nil {
		return nil, err
	}
	procs := s.activeProcessesSorted()
	counts := make(map[string][]int, len(procs))
	for _, p := range procs {
		row := make([]int, 7)
		for i, d := range dates {
			row[i] = s.db.Counts[countKey(staffID, d, p.ID)]
		}
		counts[p.ID] = row
	}
	memos := make([]string, 7)
	for i, d := range dates {
		memos[i] = s.db.Memos[memoKey(staffID, d)]
	}
	return &WeekData{
		Start:   start,
		Dates:   dates,
		StaffID: staffID,
		Counts:  counts,
		Memos:   memos,
		CanUndo: s.canUndo(),
		CanRedo: s.canRedo(),
	}, nil
}

// ---- undo/redo 可否 ----

// canUndo は「まだ取り消されていない最後の実操作」が存在するか。
func (s *Store) canUndo() bool {
	return s.lastUndoableIndex() >= 0
}

func (s *Store) canRedo() bool {
	return len(s.db.Redo) > 0
}

// CanUndoRedo は外部から現在の活性状態を取得する。
func (s *Store) CanUndoRedo() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canUndo(), s.canRedo()
}

// isRealOp は実操作（undo で逆適用できる種別）か判定する。
// undo/redo/revert は監査イベントであり実操作ではない。
func isRealOp(kind string) bool {
	switch kind {
	case "undo", "redo", "revert":
		return false
	default:
		return true
	}
}

// lastUndoableIndex は events 内で「まだ取り消されていない最後の実操作」の添字を返す。
// 監査イベント undo は直前に取り消した実操作1件を相殺する。redo は実操作を1件復活させる。
func (s *Store) lastUndoableIndex() int {
	// 後ろから走査し、取り消し済みの実操作数を符号つき balance で数える。
	// undo は実操作1件を取り消し(+1)、redo はその取り消しを1件打ち消す(-1)。
	// 重要: redo は「まだ走査していない（時系列で前の）undo」も打ち消せるよう、
	// balance を負にできること。skip>0 のときだけ減算する旧実装だと、
	// op -> undo -> redo の並びで redo が無視され、復活済みの実操作を
	// 取り消し可能と判定できなくなる（canUndo が誤って false／対象もズレる）。
	balance := 0
	for i := len(s.db.Events) - 1; i >= 0; i-- {
		ev := s.db.Events[i]
		switch ev.Kind {
		case "undo":
			balance++
		case "redo":
			balance--
		case "revert":
			// revert は取消ごとに "undo" 監査を append 済み（その undo で
			// 既にカウント済み）。末尾の revert 監査自体は相殺しないので無視。
			continue
		default:
			// 実操作。balance>0 なら取り消し済みなので相殺して次へ。
			if balance > 0 {
				balance--
				continue
			}
			return i
		}
	}
	return -1
}

// ---- count 操作 ----

// CountOp は /api/count の処理。op = inc|dec|set。
func (s *Store) CountOp(staffID, date, processID, op string, value int) (int, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if staffID == "" || date == "" || processID == "" {
		return 0, false, false, fmt.Errorf("staffId / date / processId は必須です")
	}
	if _, err := time.ParseInLocation(dateFmt, date, JST); err != nil {
		return 0, false, false, fmt.Errorf("日付が不正です: %s", date)
	}
	// 削除済み(非アクティブ)/存在しないスタッフ・工程への書き込みを拒否する。
	// 古い画面のままの別端末が、消された対象に件数を入れて隠れデータが
	// 残る（削除を undo すると復活する）のを防ぐ。undo/redo の復元処理は
	// CountOp を通さない（reverseApply/forwardApply 直接）のでここは入口のみ。
	if st := s.findStaff(staffID); st == nil || !st.Active {
		return 0, false, false, fmt.Errorf("スタッフが見つかりません: %s", staffID)
	}
	proc := s.findProcess(processID)
	if proc == nil || !proc.Active {
		return 0, false, false, fmt.Errorf("工程が見つかりません: %s", processID)
	}

	key := countKey(staffID, date, processID)
	prev := s.db.Counts[key]
	var next int
	var kind, label string
	switch op {
	case "inc":
		next = prev + 1
		if next > maxCount { // オーバーフロー/異常値の実用上限
			next = maxCount
		}
		kind = "count_inc"
		label = fmt.Sprintf("%s +1", proc.Name)
	case "dec":
		next = prev - 1
		if next < 0 {
			next = 0
		}
		kind = "count_dec"
		label = fmt.Sprintf("%s -1", proc.Name)
	case "set":
		if value < 0 {
			value = 0
		}
		if value > maxCount {
			value = maxCount
		}
		next = value
		kind = "count_set"
		label = fmt.Sprintf("%s = %d", proc.Name, next)
	default:
		return 0, false, false, fmt.Errorf("op が不正です: %s", op)
	}

	if next == prev {
		// 変化なし（例: 0 を dec）。イベントを残さず現状を返す。
		return next, s.canUndo(), s.canRedo(), nil
	}

	s.setCount(key, next)
	s.appendEvent(&Event{
		BizDate:   date,
		StaffID:   staffID,
		Kind:      kind,
		ProcessID: processID,
		Prev:      prev,
		Next:      next,
		Label:     label,
	})
	if err := s.save(); err != nil {
		return 0, false, false, err
	}
	return next, s.canUndo(), s.canRedo(), nil
}

// setCount は0なら削除して map を肥大化させない。
func (s *Store) setCount(key string, v int) {
	if v <= 0 {
		delete(s.db.Counts, key)
		return
	}
	s.db.Counts[key] = v
}

// ---- memo 操作 ----

// MemoSet は /api/memo の処理。
func (s *Store) MemoSet(staffID, date, text string) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if staffID == "" || date == "" {
		return false, false, fmt.Errorf("staffId / date は必須です")
	}
	if _, err := time.ParseInLocation(dateFmt, date, JST); err != nil {
		return false, false, fmt.Errorf("日付が不正です: %s", date)
	}
	// 削除済み/存在しないスタッフへのメモ書き込みを拒否（入口のみ）。
	if st := s.findStaff(staffID); st == nil || !st.Active {
		return false, false, fmt.Errorf("スタッフが見つかりません: %s", staffID)
	}
	key := memoKey(staffID, date)
	prev := s.db.Memos[key]
	if prev == text {
		return s.canUndo(), s.canRedo(), nil
	}
	s.setMemo(key, text)
	s.appendEvent(&Event{
		BizDate: date,
		StaffID: staffID,
		Kind:    "memo_set",
		Prev:    prev,
		Next:    text,
		Label:   "備考を更新",
	})
	if err := s.save(); err != nil {
		return false, false, err
	}
	return s.canUndo(), s.canRedo(), nil
}

func (s *Store) setMemo(key, text string) {
	if text == "" {
		delete(s.db.Memos, key)
		return
	}
	s.db.Memos[key] = text
}

// ---- イベント append（実操作。redo をクリア） ----

func (s *Store) appendEvent(ev *Event) {
	ev.ID = s.nextID("e")
	ev.TS = nowTS()
	s.db.Events = append(s.db.Events, ev)
	// 実操作で履歴が分岐 → redo スタックは無効化。
	s.db.Redo = s.db.Redo[:0]
}

// appendAudit は undo/redo/revert の監査イベントを append（redo はクリアしない）。
func (s *Store) appendAudit(kind, label, bizDate, staffID string) {
	s.db.Events = append(s.db.Events, &Event{
		ID:      s.nextID("e"),
		TS:      nowTS(),
		BizDate: bizDate,
		StaffID: staffID,
		Kind:    kind,
		Label:   label,
	})
}

// ---- undo ----

// UndoResult は /api/undo の返り値。
type UndoResult struct {
	Applied bool   `json:"applied"`
	Label   string `json:"label,omitempty"`
	BizDate string `json:"bizDate,omitempty"`
	StaffID string `json:"staffId,omitempty"`
	CanUndo bool   `json:"canUndo"`
	CanRedo bool   `json:"canRedo"`
}

// Undo は最後の実操作を逆適用し、redo へ退避、kind:"undo" を append する。
// スナップショット復号失敗・保存失敗時は DB 全体を巻き戻して（全か無か）エラーを返す。
func (s *Store) Undo() (*UndoResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := cloneDB(s.db)
	res, err := s.undoOne()
	if err != nil {
		s.db = before
		return nil, err
	}
	if res.Applied {
		if err := s.save(); err != nil {
			s.db = before
			return nil, err
		}
	}
	res.CanUndo = s.canUndo()
	res.CanRedo = s.canRedo()
	return res, nil
}

// undoOne は1件 undo する（save はしない。caller が行う）。mu 保持前提。
// 逆適用に失敗（スナップショット破損等）した場合はエラーを返す。
func (s *Store) undoOne() (*UndoResult, error) {
	idx := s.lastUndoableIndex()
	if idx < 0 {
		return &UndoResult{Applied: false}, nil
	}
	ev := s.db.Events[idx]
	if err := s.reverseApply(ev); err != nil {
		return nil, err
	}
	// redo 用に退避（先頭へ）。
	s.db.Redo = append([]*Event{ev}, s.db.Redo...)
	// 監査イベント。
	s.appendAudit("undo", "元に戻す: "+ev.Label, ev.BizDate, ev.StaffID)
	return &UndoResult{
		Applied: true,
		Label:   ev.Label,
		BizDate: ev.BizDate,
		StaffID: ev.StaffID,
	}, nil
}

// reverseApply は実操作 ev を逆適用して prev 状態に戻す。
// tc_set / tc_config_set はスナップショット復号に失敗すると誤った状態を
// 黙って適用せず、エラーを返して呼び出し側（undo/redo/revert → API）で
// 500 として顕在化させる（§A3-3）。既存種別は常に nil を返す。
func (s *Store) reverseApply(ev *Event) error {
	switch ev.Kind {
	case "count_inc", "count_dec", "count_set":
		s.setCount(countKey(ev.StaffID, ev.BizDate, ev.ProcessID), toInt(ev.Prev))
	case "memo_set":
		s.setMemo(memoKey(ev.StaffID, ev.BizDate), toStr(ev.Prev))
	case "tc_set":
		return s.applyDaySnapshot(ev.StaffID, ev.BizDate, ev.Prev)
	case "tc_config_set":
		c, err := decodeTCConfig(ev.Prev)
		if err != nil {
			return err
		}
		s.db.TCConfig = c
		return nil
	case "process_add":
		// 追加の逆 = 非アクティブ化（ソフト削除）。
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Active = false
		}
	case "process_delete":
		// 削除の逆 = 再アクティブ化。
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Active = true
		}
	case "process_rename":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Name = toStr(ev.Prev)
		}
	case "price_set":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Price = toInt(ev.Prev)
		}
	case "process_reorder":
		s.applyOrder(true, toStrSlice(ev.Prev))
	case "staff_add":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Active = false
		}
	case "staff_delete":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Active = true
		}
	case "staff_rename":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Name = toStr(ev.Prev)
		}
	case "staff_reorder":
		s.applyOrder(false, toStrSlice(ev.Prev))
	}
	return nil
}

// forwardApply は実操作 ev を再適用して next 状態にする（redo 用）。
func (s *Store) forwardApply(ev *Event) error {
	switch ev.Kind {
	case "count_inc", "count_dec", "count_set":
		s.setCount(countKey(ev.StaffID, ev.BizDate, ev.ProcessID), toInt(ev.Next))
	case "memo_set":
		s.setMemo(memoKey(ev.StaffID, ev.BizDate), toStr(ev.Next))
	case "tc_set":
		return s.applyDaySnapshot(ev.StaffID, ev.BizDate, ev.Next)
	case "tc_config_set":
		c, err := decodeTCConfig(ev.Next)
		if err != nil {
			return err
		}
		s.db.TCConfig = c
		return nil
	case "process_add":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Active = true
		}
	case "process_delete":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Active = false
		}
	case "process_rename":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Name = toStr(ev.Next)
		}
	case "price_set":
		if p := s.findProcess(ev.ProcessID); p != nil {
			p.Price = toInt(ev.Next)
		}
	case "process_reorder":
		s.applyOrder(true, toStrSlice(ev.Next))
	case "staff_add":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Active = true
		}
	case "staff_delete":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Active = false
		}
	case "staff_rename":
		if st := s.findStaff(ev.StaffID); st != nil {
			st.Name = toStr(ev.Next)
		}
	case "staff_reorder":
		s.applyOrder(false, toStrSlice(ev.Next))
	}
	return nil
}

// applyOrder は ids の並びに従って order を 0..n-1 で振り直す。
// isProcess=true なら工程、false ならスタッフ。
func (s *Store) applyOrder(isProcess bool, ids []string) {
	for i, id := range ids {
		if isProcess {
			if p := s.findProcess(id); p != nil {
				p.Order = i
			}
		} else {
			if st := s.findStaff(id); st != nil {
				st.Order = i
			}
		}
	}
}

// ---- redo ----

// RedoResult は /api/redo の返り値。
type RedoResult struct {
	Applied bool   `json:"applied"`
	Label   string `json:"label,omitempty"`
	CanUndo bool   `json:"canUndo"`
	CanRedo bool   `json:"canRedo"`
}

// Redo は redo スタック先頭を再適用し、kind:"redo" を append する。
func (s *Store) Redo() (*RedoResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.db.Redo) == 0 {
		return &RedoResult{Applied: false, CanUndo: s.canUndo(), CanRedo: s.canRedo()}, nil
	}
	before := cloneDB(s.db)
	ev := s.db.Redo[0]
	s.db.Redo = s.db.Redo[1:]
	if err := s.forwardApply(ev); err != nil {
		s.db = before
		return nil, err
	}
	s.appendAudit("redo", "やり直し: "+ev.Label, ev.BizDate, ev.StaffID)
	if err := s.save(); err != nil {
		s.db = before
		return nil, err
	}
	return &RedoResult{
		Applied: true,
		Label:   ev.Label,
		CanUndo: s.canUndo(),
		CanRedo: s.canRedo(),
	}, nil
}

// ---- revert（ここまで戻す） ----

// Revert は指定 event 以降の実操作を新しい順に undo していく。
func (s *Store) Revert(eventID string) (int, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 入力（未知/欠落 ID）は 400 になるよう ValidationError で返す。復号破損・保存失敗は
	// 型なしエラーのまま 500 に振り分けられる（§A7 finding 6）。
	if eventID == "" {
		return 0, false, false, verr("eventId は必須です")
	}
	// 指定 event の添字を探す。
	target := -1
	for i, ev := range s.db.Events {
		if ev.ID == eventID {
			target = i
			break
		}
	}
	if target < 0 {
		return 0, false, false, verr("イベントが見つかりません: %s", eventID)
	}

	// スナップショットの事前検証（§A3.5 / finding 7）: revert が触れ得る target 以降の
	// tc_set / tc_config_set を、いずれのミューテーションよりも前に全て復号検証する。
	// 破損があればここで（無変更のまま）失敗させる。
	for i := target; i < len(s.db.Events); i++ {
		ev := s.db.Events[i]
		switch ev.Kind {
		case "tc_set":
			if _, err := decodeDay(ev.Prev); err != nil {
				return 0, false, false, err
			}
		case "tc_config_set":
			if _, err := decodeTCConfig(ev.Prev); err != nil {
				return 0, false, false, err
			}
		}
	}

	// 「指定 event 以降の実操作」をすべて取り消す。
	// undoOne を繰り返し、lastUndoableIndex が target より前になるまで続ける。
	// ただし無限ループ防止に上限を events 件数とする。
	// 途中の復号失敗・保存失敗では DB 全体を巻き戻す（全か無か）。
	before := cloneDB(s.db)
	count := 0
	guard := len(s.db.Events) + 1
	for guard > 0 {
		guard--
		idx := s.lastUndoableIndex()
		if idx < target {
			break // target より前まで戻った（これ以上は対象外）。
		}
		res, err := s.undoOne()
		if err != nil {
			s.db = before
			return 0, false, false, err
		}
		if !res.Applied {
			break
		}
		count++
	}
	if count == 0 {
		// 戻すべき実操作が無かった（既にここまで戻っている等）。
		return 0, s.canUndo(), s.canRedo(), nil
	}
	// revert 監査イベント（最後にまとめて1件）。
	s.appendAudit("revert", fmt.Sprintf("ここまで戻す（%d件取消）", count), s.db.Events[target].BizDate, s.db.Events[target].StaffID)
	if err := s.save(); err != nil {
		s.db = before
		return 0, false, false, err
	}
	return count, s.canUndo(), s.canRedo(), nil
}

// ---- 履歴 ----

// HistoryEvent は履歴表示用の1件。
type HistoryEvent struct {
	ID        string `json:"id"`
	TS        string `json:"ts"`
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	StaffName string `json:"staffName"`
}

// HistoryDay は日付ごとのグループ。
type HistoryDay struct {
	Date   string         `json:"date"`
	Events []HistoryEvent `json:"events"`
}

// History は日付フィルタ（省略=最新200件相当）で履歴を新しい順に返す。
func (s *Store) History(date string) ([]HistoryDay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if date != "" {
		if _, err := time.ParseInLocation(dateFmt, date, JST); err != nil {
			return nil, fmt.Errorf("日付が不正です: %s", date)
		}
	}

	// スタッフ名引き（非アクティブ含む）。
	nameByStaff := map[string]string{}
	for _, st := range s.db.Staff {
		nameByStaff[st.ID] = st.Name
	}

	// 対象イベントを収集。
	type pair struct {
		date string
		ev   HistoryEvent
	}
	var collected []pair
	for _, ev := range s.db.Events {
		if date != "" && ev.BizDate != date {
			continue
		}
		collected = append(collected, pair{
			date: ev.BizDate,
			ev: HistoryEvent{
				ID:        ev.ID,
				TS:        ev.TS,
				Label:     ev.Label,
				Kind:      ev.Kind,
				StaffName: nameByStaff[ev.StaffID],
			},
		})
	}

	// date 省略時は最新200件に制限（収集後の末尾200件）。
	if date == "" && len(collected) > 200 {
		collected = collected[len(collected)-200:]
	}

	// 日付ごとにグループ化（イベントは新しい順）。
	byDate := map[string][]HistoryEvent{}
	var order []string
	// 新しい順に走査してグループ内も新しい順にする。
	for i := len(collected) - 1; i >= 0; i-- {
		p := collected[i]
		if _, ok := byDate[p.date]; !ok {
			order = append(order, p.date)
		}
		byDate[p.date] = append(byDate[p.date], p.ev)
	}

	// order は最初に出現した（=最新の）日付順。日付降順に整える。
	sort.SliceStable(order, func(i, j int) bool { return order[i] > order[j] })

	days := make([]HistoryDay, 0, len(order))
	for _, d := range order {
		days = append(days, HistoryDay{Date: d, Events: byDate[d]})
	}
	return days, nil
}

// ---- 集計（totals） ----

// StaffTotal は1スタッフの合計。
type StaffTotal struct {
	StaffID string `json:"staffId"`
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Sales   int    `json:"sales"`
}

// ProcessTotal は1工程の合計。
type ProcessTotal struct {
	ProcessID string `json:"processId"`
	Name      string `json:"name"`
	Count     int    `json:"count"`
	Sales     int    `json:"sales"`
}

// Grand は全体合計。
type Grand struct {
	Count int `json:"count"`
	Sales int `json:"sales"`
}

// Totals は /api/totals の返り値。
type Totals struct {
	PerStaff   []StaffTotal   `json:"perStaff"`
	PerProcess []ProcessTotal `json:"perProcess"`
	Grand      Grand          `json:"grand"`
}

// Totals は期間 start..end（両端含む）、任意の staffId 絞り込みで集計する。
func (s *Store) Totals(start, end, staffID string) (*Totals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalsLocked(start, end, staffID)
}

// totalsLocked は mu 保持前提の集計本体（CSV からも使う）。
func (s *Store) totalsLocked(start, end, staffID string) (*Totals, error) {
	dates, err := dateRange(start, end)
	if err != nil {
		return nil, err
	}
	dateSet := map[string]bool{}
	for _, d := range dates {
		dateSet[d] = true
	}

	staff := s.activeStaffSorted()
	procs := s.activeProcessesSorted()

	priceByProc := map[string]int{}
	nameByProc := map[string]string{}
	for _, p := range procs {
		priceByProc[p.ID] = p.Price
		nameByProc[p.ID] = p.Name
	}
	staffActive := map[string]bool{}
	for _, st := range staff {
		staffActive[st.ID] = true
	}

	staffCount := map[string]int{}
	staffSales := map[string]int{}
	procCount := map[string]int{}
	procSales := map[string]int{}
	grand := Grand{}

	for key, c := range s.db.Counts {
		if c <= 0 {
			continue
		}
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 {
			continue
		}
		sid, d, pid := parts[0], parts[1], parts[2]
		if !dateSet[d] {
			continue
		}
		if staffID != "" && sid != staffID {
			continue
		}
		// 非アクティブなスタッフ/工程は集計対象外。
		if !staffActive[sid] {
			continue
		}
		price, ok := priceByProc[pid]
		if !ok {
			continue // 非アクティブ工程はスキップ。
		}
		sales := c * price
		staffCount[sid] += c
		staffSales[sid] += sales
		procCount[pid] += c
		procSales[pid] += sales
		grand.Count += c
		grand.Sales += sales
	}

	perStaff := make([]StaffTotal, 0, len(staff))
	for _, st := range staff {
		if staffID != "" && st.ID != staffID {
			continue
		}
		perStaff = append(perStaff, StaffTotal{
			StaffID: st.ID,
			Name:    st.Name,
			Count:   staffCount[st.ID],
			Sales:   staffSales[st.ID],
		})
	}
	perProcess := make([]ProcessTotal, 0, len(procs))
	for _, p := range procs {
		perProcess = append(perProcess, ProcessTotal{
			ProcessID: p.ID,
			Name:      p.Name,
			Count:     procCount[p.ID],
			Sales:     procSales[p.ID],
		})
	}

	return &Totals{PerStaff: perStaff, PerProcess: perProcess, Grand: grand}, nil
}

// CSVRow は CSV 出力用の1行（スタッフ×工程の期間合計）。
type CSVRow struct {
	StaffName   string
	ProcessName string
	Price       int
	Count       int
	Sales       int
}

// ExportCSVData は期間内の (スタッフ×工程) 集計行を返す。
func (s *Store) ExportCSVData(start, end string) ([]CSVRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dates, err := dateRange(start, end)
	if err != nil {
		return nil, err
	}
	dateSet := map[string]bool{}
	for _, d := range dates {
		dateSet[d] = true
	}

	staff := s.activeStaffSorted()
	procs := s.activeProcessesSorted()
	nameByStaff := map[string]string{}
	for _, st := range staff {
		nameByStaff[st.ID] = st.Name
	}
	priceByProc := map[string]int{}
	nameByProc := map[string]string{}
	procOrder := map[string]int{}
	for i, p := range procs {
		priceByProc[p.ID] = p.Price
		nameByProc[p.ID] = p.Name
		procOrder[p.ID] = i
	}
	staffOrder := map[string]int{}
	for i, st := range staff {
		staffOrder[st.ID] = i
	}

	// (staffID, processID) → count を集計。
	type sk struct{ sid, pid string }
	agg := map[sk]int{}
	for key, c := range s.db.Counts {
		if c <= 0 {
			continue
		}
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 {
			continue
		}
		sid, d, pid := parts[0], parts[1], parts[2]
		if !dateSet[d] {
			continue
		}
		if _, ok := nameByStaff[sid]; !ok {
			continue // 非アクティブスタッフは除外。
		}
		if _, ok := nameByProc[pid]; !ok {
			continue // 非アクティブ工程は除外。
		}
		agg[sk{sid, pid}] += c
	}

	// 決定的な順序で行を構築する: スタッフ順 → 工程順。
	var rows []CSVRow
	for _, st := range staff {
		for _, p := range procs {
			c := agg[sk{st.ID, p.ID}]
			if c == 0 {
				continue
			}
			rows = append(rows, CSVRow{
				// CSV 数式インジェクション対策（§A8）。ユーザ編集可能な
				// スタッフ名・工程名を含む全 CSV 出力に適用する。
				StaffName:   csvSafe(st.Name),
				ProcessName: csvSafe(p.Name),
				Price:       p.Price,
				Count:       c,
				Sales:       c * p.Price,
			})
		}
	}
	return rows, nil
}

// ---- 工程の編集 ----

// ProcessUpsert は id 無=追加 / 有=更新（名前・単価）。
func (s *Store) ProcessUpsert(id, name string, price int) (*Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("工程名は必須です")
	}
	if price < 0 {
		return nil, fmt.Errorf("単価は0以上で入力してください")
	}
	if price > maxPrice {
		return nil, fmt.Errorf("単価が大きすぎます（上限 %d 円）", maxPrice)
	}

	if id == "" {
		// 追加。
		maxOrder := -1
		for _, p := range s.db.Processes {
			if p.Active && p.Order > maxOrder {
				maxOrder = p.Order
			}
		}
		p := &Process{
			ID:     s.nextID("p"),
			Name:   name,
			Price:  price,
			Order:  maxOrder + 1,
			Active: true,
		}
		s.db.Processes = append(s.db.Processes, p)
		s.appendEvent(&Event{
			BizDate:   Today(),
			Kind:      "process_add",
			ProcessID: p.ID,
			Prev:      nil,
			Next:      map[string]interface{}{"name": name, "price": price},
			Label:     fmt.Sprintf("工程追加: %s (¥%d)", name, price),
		})
		if err := s.save(); err != nil {
			return nil, err
		}
		c := *p
		return &c, nil
	}

	// 更新。名前・単価を別イベントとして記録（undo 粒度を保つ）。
	p := s.findProcess(id)
	if p == nil || !p.Active {
		return nil, fmt.Errorf("工程が見つかりません: %s", id)
	}
	changed := false
	if p.Name != name {
		prev := p.Name
		p.Name = name
		s.appendEvent(&Event{
			BizDate:   Today(),
			Kind:      "process_rename",
			ProcessID: p.ID,
			Prev:      prev,
			Next:      name,
			Label:     fmt.Sprintf("工程名変更: %s → %s", prev, name),
		})
		changed = true
	}
	if p.Price != price {
		prev := p.Price
		p.Price = price
		s.appendEvent(&Event{
			BizDate:   Today(),
			Kind:      "price_set",
			ProcessID: p.ID,
			Prev:      prev,
			Next:      price,
			Label:     fmt.Sprintf("%s 単価変更: ¥%d → ¥%d", p.Name, prev, price),
		})
		changed = true
	}
	if changed {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	c := *p
	return &c, nil
}

// ProcessDelete はソフト削除（active=false）。
func (s *Store) ProcessDelete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.findProcess(id)
	if p == nil {
		return fmt.Errorf("工程が見つかりません: %s", id)
	}
	if !p.Active {
		return nil // 既に削除済み。冪等。
	}
	p.Active = false
	s.appendEvent(&Event{
		BizDate:   Today(),
		Kind:      "process_delete",
		ProcessID: p.ID,
		Prev:      true,
		Next:      false,
		Label:     fmt.Sprintf("工程削除: %s", p.Name),
	})
	return s.save()
}

// ProcessReorder は active 工程の並び順を ids で振り直す。
func (s *Store) ProcessReorder(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		return fmt.Errorf("ids は必須です")
	}
	prev := s.currentOrder(true)
	s.applyOrder(true, ids)
	s.appendEvent(&Event{
		BizDate: Today(),
		Kind:    "process_reorder",
		Prev:    prev,
		Next:    append([]string{}, ids...),
		Label:   "工程の並び替え",
	})
	return s.save()
}

// ---- スタッフの編集 ----

// StaffUpsert は id 無=追加 / 有=改名。
func (s *Store) StaffUpsert(id, name string) (*Staff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("スタッフ名は必須です")
	}

	if id == "" {
		maxOrder := -1
		for _, st := range s.db.Staff {
			if st.Active && st.Order > maxOrder {
				maxOrder = st.Order
			}
		}
		st := &Staff{
			ID:     s.nextID("s"),
			Name:   name,
			Order:  maxOrder + 1,
			Active: true,
		}
		s.db.Staff = append(s.db.Staff, st)
		s.appendEvent(&Event{
			BizDate: Today(),
			StaffID: st.ID,
			Kind:    "staff_add",
			Prev:    nil,
			Next:    name,
			Label:   fmt.Sprintf("スタッフ追加: %s", name),
		})
		if err := s.save(); err != nil {
			return nil, err
		}
		c := *st
		return &c, nil
	}

	st := s.findStaff(id)
	if st == nil || !st.Active {
		return nil, fmt.Errorf("スタッフが見つかりません: %s", id)
	}
	if st.Name != name {
		prev := st.Name
		st.Name = name
		s.appendEvent(&Event{
			BizDate: Today(),
			StaffID: st.ID,
			Kind:    "staff_rename",
			Prev:    prev,
			Next:    name,
			Label:   fmt.Sprintf("スタッフ名変更: %s → %s", prev, name),
		})
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	c := *st
	return &c, nil
}

// StaffDelete はソフト削除。
func (s *Store) StaffDelete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.findStaff(id)
	if st == nil {
		return fmt.Errorf("スタッフが見つかりません: %s", id)
	}
	// active スタッフが1人だけのときは削除を拒否（グリッドが空になるのを防ぐ）。
	activeCount := 0
	for _, x := range s.db.Staff {
		if x.Active {
			activeCount++
		}
	}
	if st.Active && activeCount <= 1 {
		return fmt.Errorf("最後のスタッフは削除できません")
	}
	if !st.Active {
		return nil // 冪等。
	}
	st.Active = false
	s.appendEvent(&Event{
		BizDate: Today(),
		StaffID: st.ID,
		Kind:    "staff_delete",
		Prev:    true,
		Next:    false,
		Label:   fmt.Sprintf("スタッフ削除: %s", st.Name),
	})
	return s.save()
}

// StaffReorder は active スタッフの並び順を ids で振り直す。
func (s *Store) StaffReorder(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		return fmt.Errorf("ids は必須です")
	}
	prev := s.currentOrder(false)
	s.applyOrder(false, ids)
	s.appendEvent(&Event{
		BizDate: Today(),
		Kind:    "staff_reorder",
		Prev:    prev,
		Next:    append([]string{}, ids...),
		Label:   "スタッフの並び替え",
	})
	return s.save()
}

// currentOrder は active な工程/スタッフを order 昇順で並べた id 列を返す（reorder の prev 用）。
func (s *Store) currentOrder(isProcess bool) []string {
	if isProcess {
		ps := s.activeProcessesSorted()
		ids := make([]string, len(ps))
		for i, p := range ps {
			ids[i] = p.ID
		}
		return ids
	}
	ss := s.activeStaffSorted()
	ids := make([]string, len(ss))
	for i, st := range ss {
		ids[i] = st.ID
	}
	return ids
}

// ---- ルックアップ ----

func (s *Store) findProcess(id string) *Process {
	for _, p := range s.db.Processes {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Store) findStaff(id string) *Staff {
	for _, st := range s.db.Staff {
		if st.ID == id {
			return st
		}
	}
	return nil
}

// ---- interface{} 変換ヘルパ（JSON ラウンドトリップ対応） ----

// toInt は prev/next に格納された数値を int に戻す（JSON では float64 になりうる）。
func toInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case float32:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	default:
		return 0
	}
}

func toStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// toStrSlice は reorder の prev/next（[]string）を復元する。
func toStrSlice(v interface{}) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, toStr(e))
		}
		return out
	default:
		return nil
	}
}
