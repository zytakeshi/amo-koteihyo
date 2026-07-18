package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// ============================================================================
// 型付きエラー（§A7 エラー分類）
//
//   ValidationError → API で 400（フィールドを明示した日本語メッセージ）
//   ErrConflict     → API で 409（rev 不一致。currentDay/currentRev を同送）
//   その他（復号破損・保存失敗）→ 500
// ============================================================================

// ValidationError は入力検証エラー（API で 400）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func verr(format string, a ...interface{}) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// ErrConflict は楽観ロック衝突の番兵（errors.Is 用）。
var ErrConflict = errors.New("他の端末で更新されました。最新を確認してください")

// ConflictError は rev 不一致時に現在値を運ぶ（errors.Is(err, ErrConflict) が真）。
type ConflictError struct {
	CurrentDay *DayAttendance
	CurrentRev int
}

func (e *ConflictError) Error() string        { return ErrConflict.Error() }
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// ============================================================================
// 設定
// ============================================================================

func defaultTCConfig() *TCConfig {
	return &TCConfig{RoundUnit: 1, RoundDir: "floor", StandardMinutes: 480}
}

// TimecardConfig は現在のタイムカード設定のコピーを返す（bootstrap 等から利用）。
func (s *Store) TimecardConfig() TCConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.db.TCConfig
}

// ============================================================================
// キー・時刻ユーティリティ
// ============================================================================

func attKey(staffID, date string) string { return staffID + "|" + date }

// hmRe は厳密な "HH:MM"（ゼロ埋め、00:00〜23:59）のみ許可する。
var hmRe = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)

// parseHM は厳密 "HH:MM" を分に変換する（"9:05" は不許可）。
func parseHM(s string) (int, bool) {
	if !hmRe.MatchString(s) {
		return 0, false
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	return h*60 + m, true
}

// fmtHM は分を "HH:MM"（ゼロ埋め）にする（等価/順序/no-op 判定を一貫させるため）。
func fmtHM(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// fmtDuration は分を "H:MM"（時間はゼロ埋めなし、24時間超も可）にする。
func fmtDuration(min int) string { return fmt.Sprintf("%d:%02d", min/60, min%60) }

// fmtDecimalHours は分を小数2桁の時間にする（例: 506 → "8.43"）。
func fmtDecimalHours(min int) string { return fmt.Sprintf("%.2f", float64(min)/60.0) }

var weekdayJP = [...]string{"日", "月", "火", "水", "木", "金", "土"}

func weekdayOf(date string) string {
	t, err := time.ParseInLocation(dateFmt, date, JST)
	if err != nil {
		return ""
	}
	return weekdayJP[int(t.Weekday())]
}

// ============================================================================
// クローン・スナップショット復号（§A3）
// ============================================================================

// cloneDay は Breaks/Flags スライスまで深くコピーする（浅いコピーはスナップショットを壊す）。
func cloneDay(d *DayAttendance) *DayAttendance {
	if d == nil {
		return nil
	}
	c := *d
	c.Breaks = append([]BreakSpan(nil), d.Breaks...)
	c.Flags = append([]string(nil), d.Flags...)
	return &c
}

// decodeDay はイベントの Prev/Next（interface{}）を *DayAttendance に復号する。
// null は「欠勤」(nil,nil)。型不一致など壊れたスナップショットはエラーを返す
// （ゼロ値を黙って適用しない — §A3-2）。
func decodeDay(v interface{}) (*DayAttendance, error) {
	if v == nil {
		return nil, nil
	}
	if d, ok := v.(*DayAttendance); ok { // 生成直後（未ラウンドトリップ）の高速路。
		c := cloneDay(d)
		// 型付き高速路でも不変条件を検証する（in-memory な破損を黙って通さない、finding 3）。
		if !dayValid(c) {
			return nil, fmt.Errorf("勤怠スナップショットが不整合です（時刻形式・順序）")
		}
		return c, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("勤怠スナップショットの変換に失敗しました: %w", err)
	}
	var d DayAttendance
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("勤怠スナップショットの解析に失敗しました: %w", err)
	}
	// JSON 構文だけでなく不変条件（時刻形式・順序・休憩整合）も検証する（finding 7）。
	// スナップショットは常に検証済みの事実であるべきなので、破綻は fail loud。
	if !dayValid(&d) {
		return nil, fmt.Errorf("勤怠スナップショットが不整合です（時刻形式・順序）")
	}
	return &d, nil
}

// decodeTCConfig はイベントの Prev/Next を *TCConfig に復号する（欠落はエラー）。
func decodeTCConfig(v interface{}) (*TCConfig, error) {
	if v == nil {
		return nil, fmt.Errorf("設定スナップショットがありません")
	}
	if c, ok := v.(*TCConfig); ok {
		cc := *c
		if !validTCConfig(&cc) { // 型付き高速路でも不変条件を検証する（finding 3）。
			return nil, fmt.Errorf("設定スナップショットが不正です（丸め単位/方向・所定時間）")
		}
		return &cc, nil
	}
	if c, ok := v.(TCConfig); ok {
		cc := c
		if !validTCConfig(&cc) {
			return nil, fmt.Errorf("設定スナップショットが不正です（丸め単位/方向・所定時間）")
		}
		return &cc, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("設定スナップショットの変換に失敗しました: %w", err)
	}
	var c TCConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("設定スナップショットの解析に失敗しました: %w", err)
	}
	// 設定の不変条件（丸め単位/方向・所定時間の範囲）も検証する（finding 7）。
	if !validTCConfig(&c) {
		return nil, fmt.Errorf("設定スナップショットが不正です（丸め単位/方向・所定時間）")
	}
	return &c, nil
}

// validTCConfig は TCConfig の不変条件を検査する（§A7 の設定検証と同一の境界）。
func validTCConfig(c *TCConfig) bool {
	if c == nil {
		return false
	}
	switch c.RoundUnit {
	case 1, 5, 10, 15, 30:
	default:
		return false
	}
	switch c.RoundDir {
	case "floor", "nearest", "ceil":
	default:
		return false
	}
	return c.StandardMinutes >= 0 && c.StandardMinutes <= 1440
}

// applyDaySnapshot は tc_set の逆/順適用: attendance[key] を復号値に設定する。
// rev はここで +1 する（reverse/forward apply も key の変更なので — §A3-4）。
func (s *Store) applyDaySnapshot(staffID, date string, v interface{}) error {
	day, err := decodeDay(v)
	if err != nil {
		return err
	}
	s.setDayRaw(attKey(staffID, date), day)
	return nil
}

// setDayRaw は attendance[key] を設定（nil=削除）し、rev を +1 する。
// 全ての key 変更経路（打刻・打刻修正・undo/redo/revert）がここを通る。
func (s *Store) setDayRaw(key string, day *DayAttendance) {
	if day == nil {
		delete(s.db.Attendance, key)
	} else {
		s.db.Attendance[key] = day
	}
	s.db.AttendanceRev[key]++
}

// ============================================================================
// 事実の妥当性・状態導出（§A2/§A6）
// ============================================================================

func hasOpenBreak(d *DayAttendance) bool {
	if d == nil || len(d.Breaks) == 0 {
		return false
	}
	return d.Breaks[len(d.Breaks)-1].End == ""
}

// dayValid は保存済みの勤怠事実が構造的に整合しているか判定する（日付文脈は見ない）。
// 不整合（時刻形式不正・出勤なし退勤・順序破綻・ゼロ長休憩・途中の未終了休憩）は
// 「invalid」アノマリとして扱う（§A6）。
func dayValid(d *DayAttendance) bool {
	if d == nil {
		return true
	}
	// 退勤しているのに未終了の休憩が残っているのは構造的に不整合（finding 4）。
	if d.Out != "" && hasOpenBreak(d) {
		return false
	}
	var lastMin int = -1
	if d.In != "" {
		m, ok := parseHM(d.In)
		if !ok {
			return false
		}
		lastMin = m
	}
	if d.Out != "" {
		if _, ok := parseHM(d.Out); !ok {
			return false
		}
		if d.In == "" {
			return false // 出勤なしの退勤。
		}
	}
	for i, b := range d.Breaks {
		if d.In == "" {
			return false // 出勤なしの休憩。
		}
		bs, ok := parseHM(b.Start)
		if !ok {
			return false
		}
		if bs < lastMin {
			return false // 順序破綻（開始が直前境界より前）。
		}
		if b.End == "" {
			// 未終了休憩は最後の要素のみ許可。
			if i != len(d.Breaks)-1 {
				return false
			}
			lastMin = bs
			continue
		}
		be, ok := parseHM(b.End)
		if !ok {
			return false
		}
		if be <= bs {
			return false // ゼロ長/逆転。
		}
		lastMin = be
	}
	if d.Out != "" {
		om, _ := parseHM(d.Out)
		if om < lastMin {
			return false // 退勤が最終境界より前。
		}
	}
	return true
}

// 実効状態コード（API の state フィールド）。
const (
	stateOff            = "off"             // 未出勤
	stateWorking        = "working"         // 勤務中
	stateBreak          = "break"           // 休憩中
	stateDone           = "done"            // 退勤済
	stateCarryover      = "carryover"       // 勤務中(前日)。退勤(前日分)のみ可
	stateCarryoverBreak = "carryover_break" // 休憩中(前日)。管理者修正のみ
	stateInvalid        = "invalid"         // 事実破損。管理者修正のみ
)

type effState struct {
	state     string
	carryover bool
	bizDate   string // 対象営業日（carryover は前日、それ以外は当日）
	day       *DayAttendance
}

// deriveState は §A2/§A3 の優先順位で当該スタッフの実効状態を導く。
func (s *Store) deriveState(staffID string, now time.Time) effState {
	today := now.Format(dateFmt)
	yesterday := now.AddDate(0, 0, -1).Format(dateFmt)
	todayRec := s.db.Attendance[attKey(staffID, today)]
	yRec := s.db.Attendance[attKey(staffID, yesterday)]

	// (1) 当日/前日の記録が invalid → 全ロック（管理者修正のみ）。
	if todayRec != nil && !dayValid(todayRec) {
		return effState{stateInvalid, false, today, todayRec}
	}
	if yRec != nil && !dayValid(yRec) {
		return effState{stateInvalid, false, yesterday, yRec}
	}
	// (2) 前日の未解決（carryover / 休憩閉じ忘れロック）が支配する。
	if yRec != nil && yRec.In != "" && yRec.Out == "" {
		if hasOpenBreak(yRec) {
			return effState{stateCarryoverBreak, true, yesterday, yRec}
		}
		return effState{stateCarryover, true, yesterday, yRec}
	}
	// (3) 当日の状態を通常導出。
	if todayRec == nil || todayRec.In == "" {
		return effState{stateOff, false, today, todayRec}
	}
	if hasOpenBreak(todayRec) {
		return effState{stateBreak, false, today, todayRec}
	}
	if todayRec.Out != "" {
		return effState{stateDone, false, today, todayRec}
	}
	return effState{stateWorking, false, today, todayRec}
}

// ============================================================================
// 実働時間の純関数（§A5）
// ============================================================================

// rawWorked は生の実働分を返す。不完全な日（未退勤/未終了休憩/未出勤）は complete=false。
// 構造的に不整合な事実（dayValid=false）も complete=false として給与・集計から除外する
// （破損データが complete=true で払い出されるのを防ぐ、finding 4）。
func rawWorked(d *DayAttendance) (minutes int, complete bool) {
	if !dayValid(d) {
		return 0, false
	}
	if d == nil || d.In == "" || d.Out == "" {
		return 0, false
	}
	in, ok := parseHM(d.In)
	if !ok {
		return 0, false
	}
	out, ok := parseHM(d.Out)
	if !ok {
		return 0, false
	}
	total := out - in
	for _, b := range d.Breaks {
		bs, ok1 := parseHM(b.Start)
		if b.End == "" {
			return 0, false // 未終了休憩 → 不完全。
		}
		be, ok2 := parseHM(b.End)
		if !ok1 || !ok2 {
			return 0, false
		}
		total -= be - bs
	}
	if total < 0 {
		return 0, false
	}
	return total, true
}

// roundMinutes は参考表示用の丸め（unit<=1 は素通し）。nearest は半分切り上げ。
func roundMinutes(raw, unit int, dir string) int {
	if unit <= 1 || raw <= 0 {
		return raw
	}
	switch dir {
	case "ceil":
		return ((raw + unit - 1) / unit) * unit
	case "nearest":
		return ((raw + unit/2) / unit) * unit
	default: // floor
		return (raw / unit) * unit
	}
}

// overStdMinutes は所定超過（参考）= max(0, raw - std)。
func overStdMinutes(raw, std int) int {
	if raw <= std {
		return 0
	}
	return raw - std
}

// ============================================================================
// CSV 数式インジェクション対策（§A8）
// ============================================================================

// csvSafe は先頭が数式起点になり得る文字のセルに ' を前置する。
// 全 CSV 出力（タイムカード・工程表）のユーザ編集可能セルに適用する。
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// ============================================================================
// API DTO（§A7 finding 13）
//
// 保存形の DayAttendance は JSON で "note" を使う（後方互換）。一方 API 契約は
// 勤怠メモを "attendanceNote" で公開する。punch/day/409(currentDay) 応答が
// 保存型を直接シリアライズすると "note" が漏れて不整合になるため、公開用の
// 小さな DTO へ写像する。
// ============================================================================

// DayDTO は勤怠1日分の API 表現（保存形=note、API=attendanceNote）。
type DayDTO struct {
	In             string      `json:"in"`
	Out            string      `json:"out"`
	Breaks         []BreakSpan `json:"breaks"`
	AttendanceNote string      `json:"attendanceNote"`
	Flags          []string    `json:"flags"`
}

// NewDayDTO は保存型 *DayAttendance を API DTO へ写像する（nil→nil=欠勤）。
func NewDayDTO(d *DayAttendance) *DayDTO {
	if d == nil {
		return nil
	}
	return &DayDTO{
		In:             d.In,
		Out:            d.Out,
		Breaks:         append([]BreakSpan{}, d.Breaks...),
		AttendanceNote: d.Note,
		Flags:          append([]string{}, d.Flags...),
	}
}

// ============================================================================
// /api/timecard/today（§A7）
// ============================================================================

// TCTodayStaff は打刻画面1スタッフ分。
type TCTodayStaff struct {
	StaffID        string      `json:"staffId"`
	Name           string      `json:"name"`
	State          string      `json:"state"`
	Carryover      bool        `json:"carryover"`
	BizDate        string      `json:"bizDate"`
	In             string      `json:"in"`
	Out            string      `json:"out"`
	Breaks         []BreakSpan `json:"breaks"`
	AttendanceNote string      `json:"attendanceNote"`
	Flags          []string    `json:"flags"`
}

// TCAnomaly は勤怠の異常1件。
type TCAnomaly struct {
	StaffID   string `json:"staffId"`
	StaffName string `json:"staffName"`
	Date      string `json:"date"`
	Kind      string `json:"kind"` // 退勤忘れ / 休憩閉じ忘れ / midnight_clamped / clock_warp / invalid
}

// TCTodayData は /api/timecard/today の返り値。
type TCTodayData struct {
	Date      string         `json:"date"`
	ServerNow string         `json:"serverNow"`
	Staff     []TCTodayStaff `json:"staff"`
	Anomalies []TCAnomaly    `json:"anomalies"`
	CanUndo   bool           `json:"canUndo"`
	CanRedo   bool           `json:"canRedo"`
}

// TimecardToday は打刻画面用データ（active スタッフのみ + 全スタッフのアノマリ）。
func (s *Store) TimecardToday() (*TCTodayData, error) {
	return s.timecardToday(time.Now().In(JST))
}

func (s *Store) timecardToday(now time.Time) (*TCTodayData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := now.Format(dateFmt)

	staff := s.activeStaffSorted()
	out := make([]TCTodayStaff, 0, len(staff))
	for _, st := range staff {
		es := s.deriveState(st.ID, now)
		rec := es.day
		item := TCTodayStaff{
			StaffID:   st.ID,
			Name:      st.Name,
			State:     es.state,
			Carryover: es.carryover,
			BizDate:   es.bizDate,
			Breaks:    []BreakSpan{},
			Flags:     []string{},
		}
		if rec != nil {
			item.In = rec.In
			item.Out = rec.Out
			item.AttendanceNote = rec.Note
			if len(rec.Breaks) > 0 {
				item.Breaks = append([]BreakSpan(nil), rec.Breaks...)
			}
			if len(rec.Flags) > 0 {
				item.Flags = append([]string(nil), rec.Flags...)
			}
		}
		out = append(out, item)
	}

	return &TCTodayData{
		Date:      today,
		ServerNow: now.Format(time.RFC3339),
		Staff:     out,
		Anomalies: s.computeAnomalies(now),
		CanUndo:   s.canUndo(),
		CanRedo:   s.canRedo(),
	}, nil
}

// computeAnomalies は全スタッフの勤怠から異常を集める（§A6）。mu 保持前提。
func (s *Store) computeAnomalies(now time.Time) []TCAnomaly {
	today := now.Format(dateFmt)
	yesterday := now.AddDate(0, 0, -1).Format(dateFmt)

	nameByStaff := map[string]string{}
	for _, st := range s.db.Staff {
		nameByStaff[st.ID] = st.Name
	}

	// 決定的順序: (date, staffId) でソート。
	keys := make([]string, 0, len(s.db.Attendance))
	for k := range s.db.Attendance {
		keys = append(keys, k)
	}
	sortStrings(keys)

	var out []TCAnomaly
	add := func(sid, date, kind string) {
		out = append(out, TCAnomaly{StaffID: sid, StaffName: nameByStaff[sid], Date: date, Kind: kind})
	}
	for _, key := range keys {
		rec := s.db.Attendance[key]
		if rec == nil {
			continue
		}
		sid, date := splitAttKey(key)
		past := date < today
		isCarryoverYesterday := date == yesterday && rec.In != "" && rec.Out == "" && !hasOpenBreak(rec)

		// invalid（破損した事実）は日付を問わず顕在化。
		if !dayValid(rec) {
			add(sid, date, "invalid")
			continue
		}
		// 当日の未完成は異常ではない（開いているだけ）。
		if past {
			if rec.In != "" && rec.Out == "" && hasOpenBreak(rec) {
				add(sid, date, "休憩閉じ忘れ")
			} else if rec.In != "" && rec.Out == "" && !isCarryoverYesterday {
				add(sid, date, "退勤忘れ")
			}
		}
		// フラグ由来（当日/過去問わず、修正されるまで ⚠）。
		if hasFlag(rec, "midnight_clamped") {
			add(sid, date, "midnight_clamped")
		}
		if hasFlag(rec, "clock_warp") {
			add(sid, date, "clock_warp")
		}
	}
	return out
}

// ============================================================================
// /api/timecard/punch（§A4）状態機械
// ============================================================================

// PunchResult は打刻結果。
type PunchResult struct {
	Day   *DayAttendance `json:"day"`
	State string         `json:"state"`
	Rev   int            `json:"rev"`
}

// Punch はサーバ時刻で打刻する（クライアントは時刻を送らない）。
func (s *Store) Punch(staffID, action string) (*PunchResult, error) {
	return s.punch(staffID, action, time.Now().In(JST))
}

func (s *Store) punch(staffID, action string, now time.Time) (*PunchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.findStaff(staffID)
	if st == nil {
		return nil, verr("スタッフが見つかりません: %s", staffID)
	}
	if !st.Active {
		return nil, verr("無効なスタッフには打刻できません")
	}

	es := s.deriveState(staffID, now)
	nowMin := now.Hour()*60 + now.Minute()

	var bizDate, label string
	var newRec *DayAttendance

	switch action {
	case "in":
		if es.state != stateOff {
			return nil, punchStateError("in", es.state)
		}
		bizDate = es.bizDate // = today
		newRec = &DayAttendance{In: fmtHM(nowMin), Breaks: []BreakSpan{}, Flags: []string{}}
		label = "出勤 " + fmtHM(nowMin)

	case "break_start":
		if es.state != stateWorking {
			return nil, punchStateError("break_start", es.state)
		}
		bizDate = es.bizDate
		work := cloneDay(es.day)
		stampMin, warp, err := clampPunch(work, "break_start", nowMin)
		if err != nil {
			return nil, err
		}
		work.Breaks = append(work.Breaks, BreakSpan{Start: fmtHM(stampMin), End: ""})
		if warp {
			addFlag(work, "clock_warp")
		}
		newRec = work
		label = "休憩開始 " + fmtHM(stampMin)

	case "break_end":
		if es.state != stateBreak {
			return nil, punchStateError("break_end", es.state)
		}
		bizDate = es.bizDate
		work := cloneDay(es.day)
		stampMin, warp, err := clampPunch(work, "break_end", nowMin)
		if err != nil {
			return nil, err
		}
		work.Breaks[len(work.Breaks)-1].End = fmtHM(stampMin)
		if warp {
			addFlag(work, "clock_warp")
		}
		newRec = work
		label = "休憩終了 " + fmtHM(stampMin)

	case "out":
		switch es.state {
		case stateCarryover:
			// 前日の退勤（日跨ぎ）→ 23:59 に clamp + midnight_clamped。
			bizDate = es.bizDate // = yesterday
			work := cloneDay(es.day)
			work.Out = "23:59"
			addFlag(work, "midnight_clamped")
			newRec = work
			label = "退勤(前日分) 23:59"
		case stateWorking:
			bizDate = es.bizDate
			work := cloneDay(es.day)
			stampMin, warp, err := clampPunch(work, "out", nowMin)
			if err != nil {
				return nil, err
			}
			work.Out = fmtHM(stampMin)
			if warp {
				addFlag(work, "clock_warp")
			}
			newRec = work
			label = "退勤 " + fmtHM(stampMin)
		default:
			return nil, punchStateError("out", es.state)
		}
	default:
		return nil, verr("打刻の種別が不正です: %s", action)
	}

	// チェックポイント（全か無か）。
	before := cloneDB(s.db)
	key := attKey(staffID, bizDate)
	prev := cloneDay(s.db.Attendance[key])
	s.setDayRaw(key, cloneDay(newRec))
	s.appendEvent(&Event{
		BizDate: bizDate,
		StaffID: staffID,
		Kind:    "tc_set",
		Prev:    prev,
		Next:    cloneDay(newRec),
		Label:   label,
	})
	if err := s.save(); err != nil {
		s.db = before
		return nil, err
	}

	after := s.deriveState(staffID, now)
	return &PunchResult{
		Day:   cloneDay(s.db.Attendance[key]),
		State: after.state,
		Rev:   s.db.AttendanceRev[key],
	}, nil
}

// punchStateError は状態に対して不正な打刻の日本語メッセージ（400）を返す。
func punchStateError(action, state string) *ValidationError {
	switch action {
	case "in":
		switch state {
		case stateWorking:
			return verr("すでに出勤済みです")
		case stateBreak:
			return verr("休憩中です。先に休憩終了を押してください")
		case stateDone:
			return verr("本日は退勤済みです")
		case stateCarryover, stateCarryoverBreak:
			return verr("前日の退勤打刻が済んでいません")
		case stateInvalid:
			return verr("打刻データに不整合があります。月次画面の打刻修正で直してください")
		}
	case "break_start":
		switch state {
		case stateOff, stateCarryover, stateCarryoverBreak:
			return verr("出勤していません")
		case stateBreak:
			return verr("すでに休憩中です")
		case stateDone:
			return verr("すでに退勤済みです")
		case stateInvalid:
			return verr("打刻データに不整合があります。月次画面の打刻修正で直してください")
		}
	case "break_end":
		switch state {
		case stateWorking, stateCarryover:
			return verr("休憩中ではありません")
		case stateOff, stateCarryoverBreak:
			return verr("出勤していません")
		case stateDone:
			return verr("すでに退勤済みです")
		case stateInvalid:
			return verr("打刻データに不整合があります。月次画面の打刻修正で直してください")
		}
	case "out":
		switch state {
		case stateOff:
			return verr("出勤していません")
		case stateBreak:
			return verr("休憩終了を先に押してください")
		case stateDone:
			return verr("すでに退勤済みです")
		case stateCarryoverBreak:
			return verr("前日の休憩が終了していません。月次画面の打刻修正で直してください")
		case stateInvalid:
			return verr("打刻データに不整合があります。月次画面の打刻修正で直してください")
		}
	}
	return verr("その操作はできません")
}

// clampPunch は action 固有の最小合法時刻を記録から算出し、now を上方向へ丸める
// （システム時計の巻き戻し対策 §A10）。丸めが起きたら warp=true。
// 最小が 23:59 を超える場合は管理者修正を促すエラー。順序破綻/ゼロ長休憩を作らない。
func clampPunch(rec *DayAttendance, action string, nowMin int) (stamp int, warp bool, err error) {
	var minLegal int
	switch action {
	case "break_start", "out":
		minLegal = lastBoundary(rec) // 直前境界。等しいのは許可。
	case "break_end":
		// 未終了休憩の開始 +1（厳密に後）。
		if len(rec.Breaks) == 0 {
			return 0, false, verr("休憩中ではありません")
		}
		bs, ok := parseHM(rec.Breaks[len(rec.Breaks)-1].Start)
		if !ok {
			return 0, false, verr("休憩開始時刻が不正です")
		}
		minLegal = bs + 1
	}
	if minLegal > 1439 {
		return 0, false, verr("打刻時刻が翌日をまたぎます。月次画面の打刻修正で直してください")
	}
	if nowMin < minLegal {
		return minLegal, true, nil
	}
	return nowMin, false, nil
}

// lastBoundary は記録内の最終確定時刻（分）= max(in, 各閉じた休憩の end)。
func lastBoundary(rec *DayAttendance) int {
	last := -1
	if rec.In != "" {
		if m, ok := parseHM(rec.In); ok {
			last = m
		}
	}
	for _, b := range rec.Breaks {
		if b.End == "" {
			continue
		}
		if m, ok := parseHM(b.End); ok && m > last {
			last = m
		}
	}
	return last
}

// ============================================================================
// /api/timecard/month（§A7）
// ============================================================================

// TCRosterEntry は月次タブの1スタッフ（退職/無効も含む）。
type TCRosterEntry struct {
	StaffID  string `json:"staffId"`
	Name     string `json:"name"`
	Inactive bool   `json:"inactive"`
}

// TCDayRow は月次の1日分。
type TCDayRow struct {
	Date           string      `json:"date"`
	Weekday        string      `json:"weekday"`
	In             string      `json:"in"`
	Out            string      `json:"out"`
	Breaks         []BreakSpan `json:"breaks"`
	AttendanceNote string      `json:"attendanceNote"`
	Flags          []string    `json:"flags"`
	WorkedRaw      *int        `json:"workedRaw"` // 分。不完全日は null
	WorkedRef      *int        `json:"workedRef"` // 分。roundUnit==1 or 不完全は null
	OverStd        *int        `json:"overStd"`   // 分。不完全は null
	Complete       bool        `json:"complete"`
	Invalid        bool        `json:"invalid"` // 事実が構造的に不整合（fail-visible、finding 4）
	Rev            int         `json:"rev"`
}

// TCMonthTotals は月次合計（生ベース）。
type TCMonthTotals struct {
	Days      int  `json:"days"`      // 出勤日数（完全日）
	WorkedRaw int  `json:"workedRaw"` // 分
	WorkedRef *int `json:"workedRef"` // 分。roundUnit==1 は null
	OverStd   int  `json:"overStd"`   // 分
}

// TCMonthData は /api/timecard/month の返り値。
type TCMonthData struct {
	Month   string          `json:"month"`
	StaffID string          `json:"staffId"`
	Roster  []TCRosterEntry `json:"roster"`
	Days    []TCDayRow      `json:"days"`
	Totals  TCMonthTotals   `json:"totals"`
	Config  TCConfig        `json:"config"`
	CanUndo bool            `json:"canUndo"`
	CanRedo bool            `json:"canRedo"`
}

// TimecardMonth は月次データ（1スタッフ）を返す。roster は active ∪ 当月出勤のある無効者。
func (s *Store) TimecardMonth(staffID, month string) (*TCMonthData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first, last, err := monthBounds(month)
	if err != nil {
		return nil, err
	}
	if staffID == "" {
		return nil, verr("staffId は必須です")
	}
	if s.findStaff(staffID) == nil {
		return nil, verr("スタッフが見つかりません: %s", staffID)
	}

	cfg := *s.db.TCConfig
	roster := s.tcRoster(month)

	dates, _ := dateRange(first, last)
	days := make([]TCDayRow, 0, len(dates))
	totals := TCMonthTotals{}
	var totalRef int
	for _, d := range dates {
		key := attKey(staffID, d)
		rec := s.db.Attendance[key]
		row := buildDayRow(d, rec, cfg, s.db.AttendanceRev[key])
		days = append(days, row)
		if row.Complete {
			totals.Days++
			totals.WorkedRaw += *row.WorkedRaw
			totals.OverStd += *row.OverStd
			if row.WorkedRef != nil {
				totalRef += *row.WorkedRef
			}
		}
	}
	if cfg.RoundUnit != 1 {
		totals.WorkedRef = &totalRef
	}

	return &TCMonthData{
		Month:   month,
		StaffID: staffID,
		Roster:  roster,
		Days:    days,
		Totals:  totals,
		Config:  cfg,
		CanUndo: s.canUndo(),
		CanRedo: s.canRedo(),
	}, nil
}

// buildDayRow は1日分の行を組み立てる（純データ）。
func buildDayRow(date string, rec *DayAttendance, cfg TCConfig, rev int) TCDayRow {
	row := TCDayRow{
		Date:    date,
		Weekday: weekdayOf(date),
		Breaks:  []BreakSpan{},
		Flags:   []string{},
		Rev:     rev,
	}
	if rec != nil {
		row.In = rec.In
		row.Out = rec.Out
		row.AttendanceNote = rec.Note
		if len(rec.Breaks) > 0 {
			row.Breaks = append([]BreakSpan(nil), rec.Breaks...)
		}
		if len(rec.Flags) > 0 {
			row.Flags = append([]string(nil), rec.Flags...)
		}
		// 破損した事実は月次行でも顕在化する（fail-visible、finding 4）。
		row.Invalid = !dayValid(rec)
	}
	raw, complete := rawWorked(rec)
	row.Complete = complete
	if complete {
		r := raw
		row.WorkedRaw = &r
		o := overStdMinutes(raw, cfg.StandardMinutes)
		row.OverStd = &o
		if cfg.RoundUnit != 1 {
			ref := roundMinutes(raw, cfg.RoundUnit, cfg.RoundDir)
			row.WorkedRef = &ref
		}
	}
	return row
}

// tcRoster は当月の月次タブ用スタッフ一覧（§A7）: active 全員 ∪ 当月出勤のある無効者。
func (s *Store) tcRoster(month string) []TCRosterEntry {
	// 当月に勤怠のあるスタッフ集合。
	hasAttendance := map[string]bool{}
	for key, rec := range s.db.Attendance {
		if rec == nil {
			continue
		}
		sid, date := splitAttKey(key)
		if len(date) >= 7 && date[:7] == month {
			hasAttendance[sid] = true
		}
	}
	var roster []TCRosterEntry
	seen := map[string]bool{}
	for _, st := range s.activeStaffSorted() {
		roster = append(roster, TCRosterEntry{StaffID: st.ID, Name: st.Name, Inactive: false})
		seen[st.ID] = true
	}
	// 無効だが当月出勤のある者を order 順で追加。
	var inactive []*Staff
	for _, st := range s.db.Staff {
		if st.Active || seen[st.ID] || !hasAttendance[st.ID] {
			continue
		}
		c := *st
		inactive = append(inactive, &c)
	}
	sortStaffByOrder(inactive)
	for _, st := range inactive {
		roster = append(roster, TCRosterEntry{StaffID: st.ID, Name: st.Name, Inactive: true})
	}
	if roster == nil {
		roster = []TCRosterEntry{}
	}
	return roster
}

// ============================================================================
// /api/timecard/day（§A7/§A10）打刻修正
// ============================================================================

// TCDayResult は打刻修正の結果。
type TCDayResult struct {
	Day *DayAttendance `json:"day"`
	Rev int            `json:"rev"`
}

// TimecardDay は1日分の全置換（打刻修正）。全空は削除。rev 不一致は 409。
func (s *Store) TimecardDay(staffID, date, in, out string, breaks []BreakSpan, note string, expectedRev int) (*TCDayResult, error) {
	return s.timecardDay(staffID, date, in, out, breaks, note, expectedRev, time.Now().In(JST))
}

func (s *Store) timecardDay(staffID, date, in, out string, breaks []BreakSpan, note string, expectedRev int, now time.Time) (*TCDayResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if staffID == "" {
		return nil, verr("staffId は必須です")
	}
	// 無効スタッフも編集可（退職者の給与修正は正当）。未知は拒否。
	if s.findStaff(staffID) == nil {
		return nil, verr("スタッフが見つかりません: %s", staffID)
	}

	// (A10) 入力検証（rev チェックより先。フィールドを明示した 400）。
	canonDay, err := validateDay(date, in, out, breaks, note, now)
	if err != nil {
		return nil, err
	}

	key := attKey(staffID, date)
	existing := s.db.Attendance[key]
	curRev := s.db.AttendanceRev[key]

	// (A10-9) rev チェック → 409。
	if expectedRev != curRev {
		return nil, &ConflictError{CurrentDay: cloneDay(existing), CurrentRev: curRev}
	}

	// フラグ処理: 時刻/休憩内容が変わったらクリア、メモのみ変更は保持（§A2）。
	// 前日が nil（新規レコード）でも、時刻/休憩の射影を空とみなして比較する。
	// 空の時刻+休憩でメモだけを持つ新規レコードは「メモのみ変更」扱い（finding 18）。
	timeChanged := true
	noteOnly := false
	var finalDay *DayAttendance
	if canonDay != nil {
		var prevIn, prevOut, prevNote string
		var prevBreaks []BreakSpan
		var prevFlags []string
		if existing != nil {
			prevIn, prevOut, prevNote = existing.In, existing.Out, existing.Note
			prevBreaks, prevFlags = existing.Breaks, existing.Flags
		}
		timeChanged = prevIn != canonDay.In || prevOut != canonDay.Out || !breaksEqual(prevBreaks, canonDay.Breaks)
		if timeChanged {
			canonDay.Flags = []string{}
		} else {
			canonDay.Flags = append([]string(nil), prevFlags...)
			noteOnly = prevNote != canonDay.Note
		}
		finalDay = canonDay
	}

	// no-op（現状と同一）: イベントを残さず redo も触らない（§A3）。
	if dayEqual(existing, finalDay) {
		return &TCDayResult{Day: cloneDay(existing), Rev: curRev}, nil
	}

	label := "打刻修正"
	if finalDay != nil && !timeChanged && noteOnly {
		label = "勤怠メモ変更"
	}

	before := cloneDB(s.db)
	prev := cloneDay(existing)
	s.setDayRaw(key, cloneDay(finalDay))
	s.appendEvent(&Event{
		BizDate: date,
		StaffID: staffID,
		Kind:    "tc_set",
		Prev:    prev,
		Next:    cloneDay(finalDay),
		Label:   label,
	})
	if err := s.save(); err != nil {
		s.db = before
		return nil, err
	}
	return &TCDayResult{Day: cloneDay(s.db.Attendance[key]), Rev: s.db.AttendanceRev[key]}, nil
}

// validateDay は §A10 の検証マトリクスを適用し、正規化した *DayAttendance を返す
// （全空は nil = 削除）。違反は ValidationError（フィールドを明示）。
func validateDay(date, in, out string, breaks []BreakSpan, note string, now time.Time) (*DayAttendance, error) {
	today := now.Format(dateFmt)

	// (1) 日付: 形式 & 未来不可。
	if _, err := time.ParseInLocation(dateFmt, date, JST); err != nil {
		return nil, verr("日付が不正です: %s", date)
	}
	if date > today {
		return nil, verr("未来の日付は指定できません")
	}

	// (8) メモ 500 コードポイント、休憩 10 回まで。
	if utf8.RuneCountInString(note) > 500 {
		return nil, verr("勤怠メモは500文字以内で入力してください")
	}
	if len(breaks) > 10 {
		return nil, verr("休憩は1日10回までです")
	}

	// (2) 時刻の厳密形式 + 正規化。
	var inMin, outMin int
	if in != "" {
		m, ok := parseHM(in)
		if !ok {
			return nil, verr("出勤時刻の形式が不正です（HH:MM）: %s", in)
		}
		inMin = m
		in = fmtHM(m)
	}
	if out != "" {
		m, ok := parseHM(out)
		if !ok {
			return nil, verr("退勤時刻の形式が不正です（HH:MM）: %s", out)
		}
		outMin = m
		out = fmtHM(m)
	}

	// (3) 出勤なしの退勤/休憩は不可。
	if out != "" && in == "" {
		return nil, verr("退勤だけの記録はできません（出勤が必要です）")
	}
	if len(breaks) > 0 && in == "" {
		return nil, verr("休憩の記録には出勤が必要です")
	}

	// 休憩の正規化 + 検証。
	canonBreaks := make([]BreakSpan, 0, len(breaks))
	lastMin := -1
	if in != "" {
		lastMin = inMin
	}
	for i, b := range breaks {
		bs, ok := parseHM(b.Start)
		if !ok {
			return nil, verr("休憩開始時刻の形式が不正です（HH:MM）: %s", b.Start)
		}
		// (4) 順序: 直前境界 ≤ 開始（等しいのは許可）。
		if bs < lastMin {
			return nil, verr("休憩の時刻の順序が不正です（%d番目）", i+1)
		}
		if b.End == "" {
			// (6) 未終了休憩: 当日 & out=="" & 最後の休憩 のときのみ。
			if date != today || out != "" || i != len(breaks)-1 {
				return nil, verr("終了していない休憩は指定できません（%d番目）", i+1)
			}
			canonBreaks = append(canonBreaks, BreakSpan{Start: fmtHM(bs), End: ""})
			lastMin = bs
			continue
		}
		be, ok := parseHM(b.End)
		if !ok {
			return nil, verr("休憩終了時刻の形式が不正です（HH:MM）: %s", b.End)
		}
		// (4) ゼロ長/逆転休憩は不可（開始 < 終了）。
		if be <= bs {
			return nil, verr("休憩の終了は開始より後にしてください（%d番目）", i+1)
		}
		canonBreaks = append(canonBreaks, BreakSpan{Start: fmtHM(bs), End: fmtHM(be)})
		lastMin = be
	}

	// (5) 退勤が設定されていれば、最終休憩 ≤ 退勤（閉じた休憩は [in,out] 内）。
	if out != "" && outMin < lastMin {
		return nil, verr("退勤時刻は休憩終了より後にしてください")
	}

	// (7) out=="" は当日のみ許可（過去日は完全 or 全空=削除）。
	if in != "" && out == "" && date != today {
		return nil, verr("過去の日付は退勤時刻が必要です")
	}

	// 全空 → 削除。
	if in == "" && out == "" && len(canonBreaks) == 0 && note == "" {
		return nil, nil
	}

	return &DayAttendance{
		In:     in,
		Out:    out,
		Breaks: canonBreaks,
		Note:   note,
		Flags:  []string{},
	}, nil
}

// ============================================================================
// /api/timecard/config（§A7）
// ============================================================================

// TimecardConfigSet はタイムカード設定を更新する（同一設定は no-op）。
func (s *Store) TimecardConfigSet(roundUnit int, roundDir string, standardMinutes int) (*TCConfig, error) {
	return s.timecardConfigSet(roundUnit, roundDir, standardMinutes, time.Now().In(JST))
}

func (s *Store) timecardConfigSet(roundUnit int, roundDir string, standardMinutes int, now time.Time) (*TCConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch roundUnit {
	case 1, 5, 10, 15, 30:
	default:
		return nil, verr("丸め単位が不正です（1/5/10/15/30分）")
	}
	switch roundDir {
	case "floor", "nearest", "ceil":
	default:
		return nil, verr("丸め方向が不正です（floor/nearest/ceil）")
	}
	if standardMinutes < 0 || standardMinutes > 1440 {
		return nil, verr("所定労働時間は0〜1440分で指定してください")
	}

	cur := *s.db.TCConfig
	next := TCConfig{RoundUnit: roundUnit, RoundDir: roundDir, StandardMinutes: standardMinutes}
	if cur == next {
		c := cur
		return &c, nil // no-op。
	}

	before := cloneDB(s.db)
	prev := cur
	nc := next
	s.db.TCConfig = &nc
	s.appendEvent(&Event{
		BizDate: now.Format(dateFmt),
		StaffID: "",
		Kind:    "tc_config_set",
		Prev:    &prev,
		Next:    &next,
		Label:   "タイムカード設定変更",
	})
	if err := s.save(); err != nil {
		s.db = before
		return nil, err
	}
	c := *s.db.TCConfig
	return &c, nil
}

// ============================================================================
// CSV データ（§A8）
// ============================================================================

// TCExportDay は CSV の1日行（表示済み文字列）。
type TCExportDay struct {
	Date          string
	Weekday       string
	In            string
	Out           string
	BreakMinutes  int
	WorkedRaw     string // "8:26"、不完全は ""
	WorkedDecimal string // "8.43"、不完全は ""
	WorkedRef     string // "8:15"、roundUnit==1 or 不完全は ""
	OverStd       string // "0:26"、不完全は ""
	Note          string // csvSafe 済み
	Flags         string // フラグを ; で連結
}

// TCExportStaff は1スタッフのブロック。
type TCExportStaff struct {
	StaffName     string // csvSafe 済み
	Inactive      bool
	Days          []TCExportDay
	TotalDays     int
	TotalWorked   string // "168:30"
	TotalDecimal  string // "168.50"
	TotalNoteText string // "出勤20日"
}

// TCExportData は CSV 生成用データ（api で整形）。
type TCExportData struct {
	Month     string
	RoundUnit int // 参考列を出すかの判定用
	Staff     []TCExportStaff
}

// TimecardExportData は月次 CSV 用データを返す。staffID 省略で全員（roster 順）。
func (s *Store) TimecardExportData(month, staffID string) (*TCExportData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first, last, err := monthBounds(month)
	if err != nil {
		return nil, err
	}
	cfg := *s.db.TCConfig
	dates, _ := dateRange(first, last)

	// 対象スタッフ列を決める。
	roster := s.tcRoster(month)
	var targets []TCRosterEntry
	if staffID != "" {
		if s.findStaff(staffID) == nil {
			return nil, verr("スタッフが見つかりません: %s", staffID)
		}
		st := s.findStaff(staffID)
		inactive := !st.Active
		targets = []TCRosterEntry{{StaffID: st.ID, Name: st.Name, Inactive: inactive}}
	} else {
		targets = roster
	}

	out := &TCExportData{Month: month, RoundUnit: cfg.RoundUnit}
	for _, entry := range targets {
		block := TCExportStaff{
			StaffName: csvSafe(entry.Name),
			Inactive:  entry.Inactive,
		}
		var totalRaw int
		for _, d := range dates {
			rec := s.db.Attendance[attKey(entry.StaffID, d)]
			day := TCExportDay{
				Date:    d,
				Weekday: weekdayOf(d),
			}
			if rec != nil {
				day.In = rec.In
				day.Out = rec.Out
				day.Note = csvSafe(rec.Note)
				day.BreakMinutes = breakMinutesOf(rec)
				day.Flags = joinFlags(rec.Flags)
			}
			if raw, complete := rawWorked(rec); complete {
				day.WorkedRaw = fmtDuration(raw)
				day.WorkedDecimal = fmtDecimalHours(raw)
				day.OverStd = fmtDuration(overStdMinutes(raw, cfg.StandardMinutes))
				if cfg.RoundUnit != 1 {
					day.WorkedRef = fmtDuration(roundMinutes(raw, cfg.RoundUnit, cfg.RoundDir))
				}
				totalRaw += raw
				block.TotalDays++
			}
			block.Days = append(block.Days, day)
		}
		block.TotalWorked = fmtDuration(totalRaw)
		block.TotalDecimal = fmtDecimalHours(totalRaw)
		block.TotalNoteText = fmt.Sprintf("出勤%d日", block.TotalDays)
		out.Staff = append(out.Staff, block)
	}
	return out, nil
}

// breakMinutesOf は閉じた休憩の合計分（未終了は 0 扱い）。
func breakMinutesOf(rec *DayAttendance) int {
	if rec == nil {
		return 0
	}
	total := 0
	for _, b := range rec.Breaks {
		if b.End == "" {
			continue
		}
		bs, ok1 := parseHM(b.Start)
		be, ok2 := parseHM(b.End)
		if ok1 && ok2 && be > bs {
			total += be - bs
		}
	}
	return total
}

// ============================================================================
// 小ヘルパ
// ============================================================================

func hasFlag(d *DayAttendance, flag string) bool {
	if d == nil {
		return false
	}
	for _, f := range d.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

func addFlag(d *DayAttendance, flag string) {
	if hasFlag(d, flag) {
		return
	}
	d.Flags = append(d.Flags, flag)
}

func joinFlags(flags []string) string {
	out := ""
	for i, f := range flags {
		if i > 0 {
			out += ";"
		}
		out += f
	}
	return out
}

func breaksEqual(a, b []BreakSpan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func flagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dayEqual は2つの勤怠が同一か（nil ≡ 欠勤、nil スライス ≡ 空）。
func dayEqual(a, b *DayAttendance) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.In == b.In && a.Out == b.Out && a.Note == b.Note &&
		breaksEqual(a.Breaks, b.Breaks) && flagsEqual(a.Flags, b.Flags)
}

func splitAttKey(key string) (staffID, date string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// monthBounds は "YYYY-MM" から当月の初日・末日を返す。
func monthBounds(month string) (first, last string, err error) {
	t, e := time.ParseInLocation("2006-01", month, JST)
	if e != nil {
		return "", "", verr("月の指定が不正です（YYYY-MM）: %s", month)
	}
	f := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, JST)
	l := f.AddDate(0, 1, -1)
	return f.Format(dateFmt), l.Format(dateFmt), nil
}

// sortStrings は昇順ソート（決定的順序用の小ユーティリティ）。
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

func sortStaffByOrder(a []*Staff) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1].Order > a[j].Order; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
