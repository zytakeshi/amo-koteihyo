package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// ---- テストヘルパ ----

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "koteihyo.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// staffID は seed の N 番目(0基点)の active スタッフ ID を返す（小池/井上/浜田）。
func staffID(s *Store, n int) string {
	var ids []string
	for _, st := range s.db.Staff {
		ids = append(ids, st.ID)
	}
	if n >= len(ids) {
		return ""
	}
	return ids[n]
}

func processID(s *Store, n int) string {
	return s.db.Processes[n].ID
}

func jst(date string, hh, mm int) time.Time {
	t, err := time.ParseInLocation(dateFmt, date, JST)
	if err != nil {
		panic(err)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), hh, mm, 0, 0, JST)
}

// ============================================================================
// 状態機械（§A4）: 通常フロー
// ============================================================================

func TestPunch_NormalFlow(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"

	// 出勤。
	r, err := s.punch(sid, "in", jst(day, 9, 58))
	if err != nil {
		t.Fatalf("in: %v", err)
	}
	if r.State != stateWorking || r.Day.In != "09:58" || r.Rev != 1 {
		t.Fatalf("in result: %+v", r)
	}
	// 二重出勤は拒否。
	if _, err := s.punch(sid, "in", jst(day, 10, 0)); err == nil {
		t.Fatalf("重複出勤が拒否されない")
	}
	// 休憩開始。
	r, err = s.punch(sid, "break_start", jst(day, 12, 30))
	if err != nil {
		t.Fatalf("break_start: %v", err)
	}
	if r.State != stateBreak || len(r.Day.Breaks) != 1 || r.Day.Breaks[0].Start != "12:30" || r.Day.Breaks[0].End != "" {
		t.Fatalf("break_start result: %+v", r)
	}
	// 休憩中の退勤は拒否。
	if _, err := s.punch(sid, "out", jst(day, 13, 0)); err == nil {
		t.Fatalf("休憩中の退勤が拒否されない")
	}
	// 休憩終了。
	r, err = s.punch(sid, "break_end", jst(day, 13, 10))
	if err != nil {
		t.Fatalf("break_end: %v", err)
	}
	if r.State != stateWorking || r.Day.Breaks[0].End != "13:10" {
		t.Fatalf("break_end result: %+v", r)
	}
	// 退勤。
	r, err = s.punch(sid, "out", jst(day, 19, 4))
	if err != nil {
		t.Fatalf("out: %v", err)
	}
	if r.State != stateDone || r.Day.Out != "19:04" {
		t.Fatalf("out result: %+v", r)
	}
	// 退勤済からの操作は不可。
	if _, err := s.punch(sid, "break_start", jst(day, 19, 5)); err == nil {
		t.Fatalf("退勤済の休憩開始が拒否されない")
	}

	// rawWorked = 19:04-09:58 - 40 = 546-40 = 506 = 8:26。
	raw, complete := rawWorked(r.Day)
	if !complete || raw != 506 {
		t.Fatalf("rawWorked=%d complete=%v", raw, complete)
	}
}

func TestPunch_InactiveStaffRejected(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 2)
	if err := s.StaffDelete(sid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.punch(sid, "in", jst("2026-07-18", 9, 0)); err == nil {
		t.Fatalf("無効スタッフの打刻が拒否されない")
	}
}

// ============================================================================
// 状態機械: carryover（前日繰り越し）§A2/§A3
// ============================================================================

func TestPunch_Carryover(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	// 前日 09:00 出勤・未退勤・休憩なし。
	s.db.Attendance[attKey(sid, "2026-07-17")] = &DayAttendance{In: "09:00", Breaks: []BreakSpan{}, Flags: []string{}}

	now := jst("2026-07-18", 8, 0)

	// 実効状態は carryover。
	es := s.deriveState(sid, now)
	if es.state != stateCarryover || !es.carryover || es.bizDate != "2026-07-17" {
		t.Fatalf("carryover 状態が違う: %+v", es)
	}
	// carryover 中は今日の出勤ブロック。
	if _, err := s.punch(sid, "in", now); err == nil {
		t.Fatalf("carryover 中の出勤が許可された")
	}
	// 退勤(前日分) → 前日に 23:59 + midnight_clamped。
	r, err := s.punch(sid, "out", now)
	if err != nil {
		t.Fatalf("退勤(前日分): %v", err)
	}
	if r.Day.Out != "23:59" || !hasFlag(r.Day, "midnight_clamped") {
		t.Fatalf("前日退勤の結果が違う: %+v", r.Day)
	}
	// 解決後は今日の出勤が解禁。
	if es := s.deriveState(sid, now); es.state != stateOff {
		t.Fatalf("解決後の状態=%s", es.state)
	}
	if _, err := s.punch(sid, "in", now); err != nil {
		t.Fatalf("解決後の出勤: %v", err)
	}
}

func TestPunch_CarryoverOpenBreakLocked(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	// 前日 出勤 + 未終了休憩。
	s.db.Attendance[attKey(sid, "2026-07-17")] = &DayAttendance{
		In:     "09:00",
		Breaks: []BreakSpan{{Start: "12:00", End: ""}},
		Flags:  []string{},
	}
	now := jst("2026-07-18", 8, 0)
	if es := s.deriveState(sid, now); es.state != stateCarryoverBreak {
		t.Fatalf("状態=%s（carryover_break を期待）", es.state)
	}
	// 打刻はロック（管理者修正のみ）。
	if _, err := s.punch(sid, "out", now); err == nil {
		t.Fatalf("休憩閉じ忘れ carryover の退勤が許可された")
	}
	if _, err := s.punch(sid, "in", now); err == nil {
		t.Fatalf("休憩閉じ忘れ carryover の出勤が許可された")
	}
}

func TestPunch_InvalidRecordLocks(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	// 出勤なしの退勤 = 破損した事実。
	s.db.Attendance[attKey(sid, "2026-07-18")] = &DayAttendance{Out: "19:00", Breaks: []BreakSpan{}, Flags: []string{}}
	now := jst("2026-07-18", 20, 0)
	if es := s.deriveState(sid, now); es.state != stateInvalid {
		t.Fatalf("状態=%s（invalid を期待）", es.state)
	}
	if _, err := s.punch(sid, "in", now); err == nil {
		t.Fatalf("invalid 記録で打刻が許可された")
	}
	// today エンドポイントの anomalies に invalid が出る。
	td, _ := s.timecardToday(now)
	found := false
	for _, a := range td.Anomalies {
		if a.Kind == "invalid" && a.StaffID == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("invalid アノマリが出ない: %+v", td.Anomalies)
	}
}

// ============================================================================
// 状態機械: clock warp（時計巻き戻しクランプ）§A10
// ============================================================================

func TestPunch_ClockWarpClamp(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	// 出勤 10:00。
	if _, err := s.punch(sid, "in", jst(day, 10, 0)); err != nil {
		t.Fatal(err)
	}
	// 休憩開始を 09:30（時計巻き戻し）→ 10:00 にクランプ + clock_warp。
	r, err := s.punch(sid, "break_start", jst(day, 9, 30))
	if err != nil {
		t.Fatalf("break_start: %v", err)
	}
	if r.Day.Breaks[0].Start != "10:00" || !hasFlag(r.Day, "clock_warp") {
		t.Fatalf("clock_warp クランプ失敗: %+v", r.Day)
	}
	// 休憩終了を 10:00 → 開始+1=10:01 にクランプ（ゼロ長休憩を作らない）。
	r, err = s.punch(sid, "break_end", jst(day, 10, 0))
	if err != nil {
		t.Fatalf("break_end: %v", err)
	}
	if r.Day.Breaks[0].End != "10:01" {
		t.Fatalf("break_end クランプ失敗: %+v", r.Day.Breaks[0])
	}
	// clock_warp は today anomalies に出る。
	td, _ := s.timecardToday(jst(day, 11, 0))
	found := false
	for _, a := range td.Anomalies {
		if a.Kind == "clock_warp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("clock_warp アノマリが出ない")
	}
}

func TestPunch_ClockWarpBeyondMidnightRejected(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	s.db.Attendance[attKey(sid, day)] = &DayAttendance{
		In:     "23:00",
		Breaks: []BreakSpan{{Start: "23:59", End: ""}},
		Flags:  []string{},
	}
	// 休憩終了の最小合法時刻 = 23:59+1 = 24:00 > 23:59 → 拒否。
	if _, err := s.punch(sid, "break_end", jst(day, 23, 0)); err == nil {
		t.Fatalf("翌日をまたぐ打刻が拒否されない")
	}
}

// ============================================================================
// 丸め境界テーブル（§A5）
// ============================================================================

func TestRoundMinutes_BoundaryTable(t *testing.T) {
	cases := []struct {
		raw, unit int
		dir       string
		want      int
	}{
		// unit=1 は素通し。
		{506, 1, "floor", 506},
		{506, 1, "nearest", 506},
		// floor（切り捨て）。
		{506, 15, "floor", 495}, // 8:26 → 8:15
		{9, 10, "floor", 0},
		{10, 10, "floor", 10},
		{11, 10, "floor", 10},
		// ceil（切り上げ）。
		{1, 10, "ceil", 10},
		{10, 10, "ceil", 10},
		{11, 10, "ceil", 20},
		{506, 15, "ceil", 510},
		// nearest（四捨五入・半分切り上げ）。 unit=10 の半分=5。
		{4, 10, "nearest", 0},
		{5, 10, "nearest", 10},
		{14, 10, "nearest", 10},
		{15, 10, "nearest", 20},
		// unit=15（奇数半分 7.5 → 8 で切り上がる）。
		{7, 15, "nearest", 0},
		{8, 15, "nearest", 15},
		{22, 15, "nearest", 15},
		{23, 15, "nearest", 30},
		// 境界 ±1（exact-unit）。
		{30, 30, "floor", 30},
		{31, 30, "floor", 30},
		{29, 30, "ceil", 30},
	}
	for _, c := range cases {
		got := roundMinutes(c.raw, c.unit, c.dir)
		if got != c.want {
			t.Errorf("round(%d,%d,%s)=%d want %d", c.raw, c.unit, c.dir, got, c.want)
		}
	}
}

func TestOverStdAndFormatting(t *testing.T) {
	if overStdMinutes(506, 480) != 26 {
		t.Fatalf("overStd 506-480")
	}
	if overStdMinutes(400, 480) != 0 {
		t.Fatalf("overStd 下限")
	}
	if fmtDuration(506) != "8:26" {
		t.Fatalf("fmtDuration 506=%s", fmtDuration(506))
	}
	if fmtDuration(10110) != "168:30" {
		t.Fatalf("fmtDuration 10110=%s", fmtDuration(10110))
	}
	if fmtDecimalHours(506) != "8.43" {
		t.Fatalf("decimal 506=%s", fmtDecimalHours(506))
	}
	if fmtDecimalHours(10110) != "168.50" {
		t.Fatalf("decimal 10110=%s", fmtDecimalHours(10110))
	}
}

// ============================================================================
// undo/redo/revert ラウンドトリップ（§A3）
// ============================================================================

func TestUndoRedo_TimecardRoundTrip(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	steps := []struct {
		action string
		hh, mm int
	}{
		{"in", 9, 58}, {"break_start", 12, 30}, {"break_end", 13, 10}, {"out", 19, 4},
	}
	for _, st := range steps {
		if _, err := s.punch(sid, st.action, jst(day, st.hh, st.mm)); err != nil {
			t.Fatalf("%s: %v", st.action, err)
		}
	}
	key := attKey(sid, day)
	final := cloneDay(s.db.Attendance[key])

	// 4回 undo → 記録は消える。
	for i := 0; i < 4; i++ {
		if _, err := s.Undo(); err != nil {
			t.Fatalf("undo %d: %v", i, err)
		}
	}
	if s.db.Attendance[key] != nil {
		t.Fatalf("undo 後に記録が残っている: %+v", s.db.Attendance[key])
	}
	// 4回 redo → 記録は完全復元（バイト等価）。
	for i := 0; i < 4; i++ {
		if _, err := s.Redo(); err != nil {
			t.Fatalf("redo %d: %v", i, err)
		}
	}
	if !dayEqual(final, s.db.Attendance[key]) {
		t.Fatalf("redo 復元が不一致: want %+v got %+v", final, s.db.Attendance[key])
	}
	// rev はすべての変更経路で加算（4 punch + 4 undo + 4 redo = 12）。
	if s.db.AttendanceRev[key] != 12 {
		t.Fatalf("rev=%d want 12", s.db.AttendanceRev[key])
	}
}

func TestUndo_NewPunchClearsRedo(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	if _, err := s.punch(sid, "in", jst(day, 9, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if !s.canRedo() {
		t.Fatalf("undo 後 redo できるはず")
	}
	// 新しい実操作 → redo は破棄。
	if _, err := s.punch(sid, "in", jst(day, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if s.canRedo() {
		t.Fatalf("新規打刻後も redo が残っている")
	}
}

func TestRevert_AcrossMixedKinds(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	pid := processID(s, 0)
	day := "2026-07-18"

	// staff_rename + count_set + tc_set + tc_config_set。
	if _, err := s.StaffUpsert(sid, "改名太郎"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CountOp(sid, day, pid, "set", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.punch(sid, "in", jst(day, 9, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.timecardConfigSet(15, "floor", 450, jst(day, 9, 0)); err != nil {
		t.Fatal(err)
	}

	firstEvent := s.db.Events[0].ID
	n, _, _, err := s.Revert(firstEvent)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if n != 4 {
		t.Fatalf("revert count=%d want 4", n)
	}
	// すべて初期状態へ。
	if st := s.findStaff(sid); st.Name != "小池" {
		t.Fatalf("名前が戻っていない: %s", st.Name)
	}
	if s.db.Counts[countKey(sid, day, pid)] != 0 {
		t.Fatalf("count が戻っていない")
	}
	if s.db.Attendance[attKey(sid, day)] != nil {
		t.Fatalf("勤怠が戻っていない")
	}
	if *s.db.TCConfig != *defaultTCConfig() {
		t.Fatalf("設定が戻っていない: %+v", *s.db.TCConfig)
	}
}

// ============================================================================
// チェックポイント: 保存失敗でバイト等価復元（§A3-5）
// ============================================================================

func TestCheckpoint_RevertSaveFailureByteIdentical(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	pid := processID(s, 0)
	day := "2026-07-18"

	if _, err := s.StaffUpsert(sid, "改名太郎"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CountOp(sid, day, pid, "set", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.punch(sid, "in", jst(day, 9, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.timecardConfigSet(15, "floor", 450, jst(day, 9, 0)); err != nil {
		t.Fatal(err)
	}

	// ディスク表現（イベント snapshot が map 化された状態）に正規化してから基準を取る。
	s.db = cloneDB(s.db)
	before, _ := json.Marshal(s.db)
	firstEvent := s.db.Events[0].ID

	// 保存失敗を注入 → Revert は全体を巻き戻してエラーを返す。
	s.failSave = true
	if _, _, _, err := s.Revert(firstEvent); err == nil {
		t.Fatalf("注入した保存失敗でエラーにならない")
	}
	s.failSave = false

	after, _ := json.Marshal(s.db)
	if string(before) != string(after) {
		t.Fatalf("保存失敗後の DB がバイト等価でない\nbefore=%s\nafter =%s", before, after)
	}
}

func TestCheckpoint_PunchSaveFailure(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	before, _ := json.Marshal(s.db)
	s.failSave = true
	if _, err := s.punch(sid, "in", jst(day, 9, 0)); err == nil {
		t.Fatalf("保存失敗で打刻がエラーにならない")
	}
	s.failSave = false
	after, _ := json.Marshal(s.db)
	if string(before) != string(after) {
		t.Fatalf("打刻の保存失敗で状態が変わった")
	}
	if len(s.db.Events) != 0 {
		t.Fatalf("失敗打刻でイベントが残った: %d", len(s.db.Events))
	}
}

// ============================================================================
// rev / 409 セマンティクス（§A7/§A10）
// ============================================================================

func TestTimecardDay_RevConflict(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	now := jst(day, 20, 0)

	// 初回作成（expectedRev=0）。
	r, err := s.timecardDay(sid, day, "09:00", "18:00", nil, "", 0, now)
	if err != nil {
		t.Fatalf("初回: %v", err)
	}
	if r.Rev != 1 {
		t.Fatalf("初回 rev=%d want 1", r.Rev)
	}
	// 古い rev で保存 → 409。
	_, err = s.timecardDay(sid, day, "09:30", "18:00", nil, "", 0, now)
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("409 にならない: %v", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("ConflictError でない")
	}
	if ce.CurrentRev != 1 || ce.CurrentDay == nil || ce.CurrentDay.In != "09:00" {
		t.Fatalf("ConflictError ペイロード不正: %+v", ce)
	}
	// 正しい rev で成功。
	r, err = s.timecardDay(sid, day, "09:30", "18:00", nil, "", 1, now)
	if err != nil {
		t.Fatalf("正 rev: %v", err)
	}
	if r.Rev != 2 || r.Day.In != "09:30" {
		t.Fatalf("更新結果: %+v", r)
	}
}

func TestTimecardDay_NoOpNoEvent(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	now := jst(day, 20, 0)
	if _, err := s.timecardDay(sid, day, "09:00", "18:00", nil, "", 0, now); err != nil {
		t.Fatal(err)
	}
	evCount := len(s.db.Events)
	rev := s.db.AttendanceRev[attKey(sid, day)]
	// 同一内容で再保存 → イベントも rev も増えない。
	r, err := s.timecardDay(sid, day, "09:00", "18:00", nil, "", 1, now)
	if err != nil {
		t.Fatalf("no-op: %v", err)
	}
	if len(s.db.Events) != evCount {
		t.Fatalf("no-op でイベントが増えた")
	}
	if r.Rev != rev {
		t.Fatalf("no-op で rev が変わった")
	}
}

func TestTimecardDay_FlagPreserveOnNoteOnly(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-17" // 過去日（完全な記録が必要）。
	// midnight_clamped フラグ付きの記録を直接投入。
	s.db.Attendance[attKey(sid, day)] = &DayAttendance{
		In: "09:00", Out: "18:00", Breaks: []BreakSpan{}, Flags: []string{"midnight_clamped"},
	}
	now := jst("2026-07-18", 10, 0)
	rev := s.db.AttendanceRev[attKey(sid, day)]

	// メモのみ変更 → フラグ保持、ラベル「勤怠メモ変更」。
	r, err := s.timecardDay(sid, day, "09:00", "18:00", nil, "電車遅延", rev, now)
	if err != nil {
		t.Fatalf("note-only: %v", err)
	}
	if !hasFlag(r.Day, "midnight_clamped") {
		t.Fatalf("メモのみ変更でフラグが消えた: %+v", r.Day.Flags)
	}
	if last := s.db.Events[len(s.db.Events)-1]; last.Label != "勤怠メモ変更" {
		t.Fatalf("ラベル=%s want 勤怠メモ変更", last.Label)
	}

	// 時刻変更 → フラグクリア、ラベル「打刻修正」。
	rev = r.Rev
	r, err = s.timecardDay(sid, day, "09:05", "18:00", nil, "電車遅延", rev, now)
	if err != nil {
		t.Fatalf("time change: %v", err)
	}
	if hasFlag(r.Day, "midnight_clamped") {
		t.Fatalf("時刻変更でフラグが残った")
	}
	if last := s.db.Events[len(s.db.Events)-1]; last.Label != "打刻修正" {
		t.Fatalf("ラベル=%s want 打刻修正", last.Label)
	}
}

func TestTimecardDay_DeleteAllEmpty(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	now := jst(day, 20, 0)
	if _, err := s.timecardDay(sid, day, "09:00", "18:00", nil, "", 0, now); err != nil {
		t.Fatal(err)
	}
	rev := s.db.AttendanceRev[attKey(sid, day)]
	// 全空 → 削除。
	if _, err := s.timecardDay(sid, day, "", "", nil, "", rev, now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.db.Attendance[attKey(sid, day)] != nil {
		t.Fatalf("削除されていない")
	}
}

// ============================================================================
// 検証マトリクス（§A10）
// ============================================================================

func TestValidateDay_Matrix(t *testing.T) {
	now := jst("2026-07-18", 20, 0)
	today := "2026-07-18"
	past := "2026-07-10"
	future := "2026-07-25"

	type tc struct {
		name    string
		date    string
		in, out string
		breaks  []BreakSpan
		note    string
		wantErr bool
	}
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'あ'
	}
	cases := []tc{
		{"未来日は不可", future, "09:00", "18:00", nil, "", true},
		{"日付不正", "2026-13-99", "09:00", "18:00", nil, "", true},
		{"非ゼロ埋め時刻は不可", today, "9:05", "18:00", nil, "", true},
		{"範囲外時刻は不可", today, "24:00", "", nil, "", true},
		{"出勤なし退勤は不可", past, "", "18:00", nil, "", true},
		{"出勤なし休憩は不可", past, "", "", []BreakSpan{{Start: "12:00", End: "13:00"}}, "", true},
		{"ゼロ長休憩は不可", past, "09:00", "18:00", []BreakSpan{{Start: "12:00", End: "12:00"}}, "", true},
		{"休憩逆転は不可", past, "09:00", "18:00", []BreakSpan{{Start: "13:00", End: "12:00"}}, "", true},
		{"休憩重複は不可", past, "09:00", "18:00", []BreakSpan{{Start: "12:00", End: "13:00"}, {Start: "12:30", End: "13:30"}}, "", true},
		{"退勤より後の休憩は不可", past, "09:00", "12:00", []BreakSpan{{Start: "13:00", End: "13:30"}}, "", true},
		{"過去日の未終了休憩は不可", past, "09:00", "", []BreakSpan{{Start: "12:00", End: ""}}, "", true},
		{"過去日の未退勤は不可", past, "09:00", "", nil, "", true},
		{"メモ501文字は不可", today, "09:00", "18:00", nil, string(long), true},
		{"休憩11回は不可", past, "09:00", "23:00", makeBreaks(11), "", true},
		// 正常系。
		{"当日の未退勤は可", today, "09:00", "", nil, "", false},
		{"当日の未終了休憩は可", today, "09:00", "", []BreakSpan{{Start: "12:00", End: ""}}, "", false},
		{"境界一致の休憩は可", past, "09:00", "18:00", []BreakSpan{{Start: "12:00", End: "13:00"}, {Start: "13:00", End: "13:30"}}, "", false},
		{"完全な過去日は可", past, "09:00", "18:00", []BreakSpan{{Start: "12:00", End: "13:00"}}, "メモ", false},
	}
	for _, c := range cases {
		_, err := validateDay(c.date, c.in, c.out, c.breaks, c.note, now)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("%s: ValidationError でない: %v", c.name, err)
			}
		}
	}
}

func makeBreaks(n int) []BreakSpan {
	out := make([]BreakSpan, n)
	for i := 0; i < n; i++ {
		h := 10 + i
		out[i] = BreakSpan{Start: fmtHM(h * 60), End: fmtHM(h*60 + 1)}
	}
	return out
}

// ============================================================================
// スナップショット復号の破損拒否（§A3-2/3）
// ============================================================================

func TestDecodeDay_RejectsCorrupt(t *testing.T) {
	// 型不一致。
	if _, err := decodeDay(map[string]interface{}{"in": 123}); err == nil {
		t.Fatalf("数値 in を受理した")
	}
	// breaks が配列でない。
	if _, err := decodeDay(map[string]interface{}{"breaks": "x"}); err == nil {
		t.Fatalf("非配列 breaks を受理した")
	}
	// 未知フィールド。
	if _, err := decodeDay(map[string]interface{}{"bogus": 1}); err == nil {
		t.Fatalf("未知フィールドを受理した")
	}
	// null は欠勤（エラーなし）。
	if d, err := decodeDay(nil); err != nil || d != nil {
		t.Fatalf("null 復号: d=%v err=%v", d, err)
	}
}

func TestUndo_FailsLoudOnCorruptSnapshot(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	// 破損した Prev を持つ tc_set を直接仕込む。
	s.db.Events = []*Event{{
		ID: "e_000001", TS: nowTS(), BizDate: "2026-07-18", StaffID: sid,
		Kind:  "tc_set",
		Prev:  map[string]interface{}{"in": 999}, // 型不一致 → 復号失敗。
		Next:  nil,
		Label: "打刻修正",
	}}
	before, _ := json.Marshal(s.db)
	if _, err := s.Undo(); err == nil {
		t.Fatalf("破損スナップショットで undo がエラーにならない")
	}
	after, _ := json.Marshal(s.db)
	if string(before) != string(after) {
		t.Fatalf("破損 undo 失敗で状態が変わった")
	}
}

// ============================================================================
// csvSafe（§A8）
// ============================================================================

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")",
		"+SUM(1)":           "'+SUM(1)",
		"-2":                "'-2",
		"@x":                "'@x",
		"\tx":               "'\tx",
		"\rx":               "'\rx",
		"浜田":                "浜田",
		"":                  "",
		"a=b":               "a=b",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q)=%q want %q", in, got, want)
		}
	}
}

func TestExportCSVData_AppliesCSVSafe(t *testing.T) {
	s := newStore(t)
	// 数式起点の名前のスタッフ/工程を作る。
	st, err := s.StaffUpsert("", "=cmd")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ProcessUpsert("", "=EVIL", 100)
	if err != nil {
		t.Fatal(err)
	}
	day := "2026-07-18"
	if _, _, _, err := s.CountOp(st.ID, day, p.ID, "set", 1); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ExportCSVData(day, day)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.StaffName == "'=cmd" && r.ProcessName == "'=EVIL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("既存CSVに csvSafe が適用されていない: %+v", rows)
	}
}

// ============================================================================
// 月次 / roster（§A7）
// ============================================================================

func TestTimecardMonth_RosterIncludesInactiveWithAttendance(t *testing.T) {
	s := newStore(t)
	active := staffID(s, 0)
	departed := staffID(s, 2)
	month := "2026-07"

	// 退職者に当月の勤怠を投入してからソフト削除。
	s.db.Attendance[attKey(departed, "2026-07-05")] = &DayAttendance{
		In: "09:00", Out: "18:00", Breaks: []BreakSpan{}, Flags: []string{},
	}
	if err := s.StaffDelete(departed); err != nil {
		t.Fatal(err)
	}

	md, err := s.TimecardMonth(active, month)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Days) != 31 {
		t.Fatalf("7月の日数=%d want 31", len(md.Days))
	}
	var sawDeparted bool
	for _, r := range md.Roster {
		if r.StaffID == departed {
			if !r.Inactive {
				t.Fatalf("退職者が inactive=false")
			}
			sawDeparted = true
		}
	}
	if !sawDeparted {
		t.Fatalf("当月勤怠のある退職者が roster にいない: %+v", md.Roster)
	}
}

func TestTimecardMonth_Totals(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	// 2日分の完全記録（各 8:26）+ 不完全1日。
	s.db.Attendance[attKey(sid, "2026-07-01")] = &DayAttendance{
		In: "09:58", Out: "19:04", Breaks: []BreakSpan{{Start: "12:30", End: "13:10"}}, Flags: []string{},
	}
	s.db.Attendance[attKey(sid, "2026-07-02")] = &DayAttendance{
		In: "09:58", Out: "19:04", Breaks: []BreakSpan{{Start: "12:30", End: "13:10"}}, Flags: []string{},
	}
	s.db.Attendance[attKey(sid, "2026-07-03")] = &DayAttendance{
		In: "09:00", Out: "", Breaks: []BreakSpan{}, Flags: []string{}, // 不完全（今日でなくても past の未退勤）
	}
	md, err := s.TimecardMonth(sid, "2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if md.Totals.Days != 2 {
		t.Fatalf("出勤日数=%d want 2", md.Totals.Days)
	}
	if md.Totals.WorkedRaw != 1012 {
		t.Fatalf("合計実働=%d want 1012", md.Totals.WorkedRaw)
	}
	if md.Totals.WorkedRef != nil {
		t.Fatalf("roundUnit==1 では WorkedRef は null")
	}
	// 不完全日は complete=false・WorkedRaw=nil。
	for _, d := range md.Days {
		if d.Date == "2026-07-03" {
			if d.Complete || d.WorkedRaw != nil {
				t.Fatalf("不完全日が totals に混入: %+v", d)
			}
		}
	}
}

// ============================================================================
// 設定（§A7）
// ============================================================================

func TestTimecardConfigSet(t *testing.T) {
	s := newStore(t)
	now := jst("2026-07-18", 10, 0)
	// 不正値。
	if _, err := s.timecardConfigSet(7, "floor", 480, now); err == nil {
		t.Fatalf("不正な丸め単位が通った")
	}
	if _, err := s.timecardConfigSet(15, "bogus", 480, now); err == nil {
		t.Fatalf("不正な丸め方向が通った")
	}
	if _, err := s.timecardConfigSet(15, "floor", 2000, now); err == nil {
		t.Fatalf("範囲外の所定時間が通った")
	}
	// 正常。
	c, err := s.timecardConfigSet(15, "nearest", 450, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.RoundUnit != 15 || c.RoundDir != "nearest" || c.StandardMinutes != 450 {
		t.Fatalf("設定が反映されない: %+v", c)
	}
	// 同一設定は no-op（イベント増えない）。
	ev := len(s.db.Events)
	if _, err := s.timecardConfigSet(15, "nearest", 450, now); err != nil {
		t.Fatal(err)
	}
	if len(s.db.Events) != ev {
		t.Fatalf("同一設定でイベントが増えた")
	}
	// undo で戻る。
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if *s.db.TCConfig != *defaultTCConfig() {
		t.Fatalf("設定 undo が効かない: %+v", *s.db.TCConfig)
	}
}

// ============================================================================
// CSV データ（§A8）
// ============================================================================

func TestTimecardExportData(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	s.db.Attendance[attKey(sid, "2026-07-01")] = &DayAttendance{
		In: "09:58", Out: "19:04", Breaks: []BreakSpan{{Start: "12:30", End: "13:10"}},
		Note: "=inject", Flags: []string{},
	}
	ex, err := s.TimecardExportData("2026-07", sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Staff) != 1 {
		t.Fatalf("staff blocks=%d", len(ex.Staff))
	}
	block := ex.Staff[0]
	var d1 *TCExportDay
	for i := range block.Days {
		if block.Days[i].Date == "2026-07-01" {
			d1 = &block.Days[i]
		}
	}
	if d1 == nil {
		t.Fatalf("7/1 の行がない")
	}
	if d1.WorkedRaw != "8:26" || d1.WorkedDecimal != "8.43" || d1.BreakMinutes != 40 {
		t.Fatalf("7/1 集計不正: %+v", d1)
	}
	if d1.Note != "'=inject" {
		t.Fatalf("勤怠メモに csvSafe 未適用: %q", d1.Note)
	}
	if block.TotalDays != 1 || block.TotalWorked != "8:26" {
		t.Fatalf("合計不正: %+v", block)
	}
	// roundUnit==1 では参考列は空。
	if d1.WorkedRef != "" {
		t.Fatalf("roundUnit==1 で参考列が出た: %q", d1.WorkedRef)
	}
}

// ============================================================================
// dayValid / rawWorked 事実整合（finding 4）
// ============================================================================

func TestDayValid_RejectsOutWithOpenBreak(t *testing.T) {
	// 退勤済みなのに未終了休憩が残る = 構造的に不整合。
	d := &DayAttendance{In: "09:00", Out: "18:00", Breaks: []BreakSpan{{Start: "12:00", End: ""}}}
	if dayValid(d) {
		t.Fatalf("退勤済 + 未終了休憩を valid と判定した")
	}
	// 破損した事実は complete=false（給与・集計から除外）。
	if _, complete := rawWorked(d); complete {
		t.Fatalf("破損した日が complete=true になった")
	}
	// 月次行では invalid が fail-visible。
	row := buildDayRow("2026-07-18", d, *defaultTCConfig(), 3)
	if !row.Invalid {
		t.Fatalf("月次行 Invalid が立っていない")
	}
	if row.Complete || row.WorkedRaw != nil {
		t.Fatalf("破損行が完全扱い: complete=%v raw=%v", row.Complete, row.WorkedRaw)
	}
}

func TestDecodeDay_RejectsSemanticInvariant(t *testing.T) {
	// JSON 構文は正しいが不変条件（退勤 + 未終了休憩）に反するスナップショットは拒否。
	bad := map[string]interface{}{
		"in":  "09:00",
		"out": "18:00",
		"breaks": []interface{}{
			map[string]interface{}{"start": "12:00", "end": ""},
		},
	}
	if _, err := decodeDay(bad); err == nil {
		t.Fatalf("不変条件違反スナップショットを受理した")
	}
}

// ============================================================================
// 打刻修正 ラベル: メモのみの新規レコード（finding 18）
// ============================================================================

func TestTimecardDay_NoteOnlyNewRecord(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	day := "2026-07-18"
	now := jst(day, 12, 0)
	// 新規で「勤怠メモだけ」（時刻・休憩なし）を登録 → ラベルは「勤怠メモ変更」。
	r, err := s.timecardDay(sid, day, "", "", nil, "電車遅延", 0, now)
	if err != nil {
		t.Fatalf("timecardDay: %v", err)
	}
	if r.Day == nil || r.Day.Note != "電車遅延" {
		t.Fatalf("メモが保存されていない: %+v", r.Day)
	}
	if last := s.db.Events[len(s.db.Events)-1]; last.Label != "勤怠メモ変更" {
		t.Fatalf("ラベル=%s want 勤怠メモ変更", last.Label)
	}
}

// ============================================================================
// 型付きスナップショットの不変条件検証（finding 3）
// ============================================================================

func TestDecodeTypedFastPath_RejectsInvariantViolations(t *testing.T) {
	// 型付き高速路（*DayAttendance / *TCConfig / TCConfig）でも不変条件を検証する。
	if _, err := decodeDay(&DayAttendance{In: "09:00", Out: "18:00", Breaks: []BreakSpan{{Start: "12:00", End: ""}}}); err == nil {
		t.Fatalf("型付き不整合 DayAttendance を受理した")
	}
	if _, err := decodeTCConfig(&TCConfig{RoundUnit: 7, RoundDir: "floor", StandardMinutes: 480}); err == nil {
		t.Fatalf("型付き不正 *TCConfig（roundUnit=7）を受理した")
	}
	if _, err := decodeTCConfig(TCConfig{RoundUnit: 5, RoundDir: "bogus", StandardMinutes: 480}); err == nil {
		t.Fatalf("型付き不正 TCConfig（roundDir）を受理した")
	}
}

func TestUndo_FailsLoudOnInvalidTypedDaySnapshot(t *testing.T) {
	s := newStore(t)
	sid := staffID(s, 0)
	key := attKey(sid, "2026-07-18")
	// Prev が「型付きポインタ」で、かつ構造的に不整合（退勤 + 未終了休憩）。
	// マップ復号ではなく型付き高速路を通るが、検証で拒否されねばならない。
	s.db.Events = []*Event{{
		ID: "e_000001", TS: nowTS(), BizDate: "2026-07-18", StaffID: sid,
		Kind:  "tc_set",
		Prev:  &DayAttendance{In: "09:00", Out: "18:00", Breaks: []BreakSpan{{Start: "12:00", End: ""}}},
		Next:  nil,
		Label: "打刻修正",
	}}
	if _, err := s.Undo(); err == nil {
		t.Fatalf("不整合な型付きスナップショットで undo がエラーにならない")
	}
	// 失敗した undo は状態を一切変えない（破損を黙って適用していない）。
	if _, ok := s.db.Attendance[key]; ok {
		t.Fatalf("失敗した undo で attendance が書き込まれた")
	}
	if len(s.db.Redo) != 0 {
		t.Fatalf("失敗した undo で redo に積まれた: %d", len(s.db.Redo))
	}
	if len(s.db.Events) != 1 {
		t.Fatalf("失敗した undo でイベントが増減した: %d", len(s.db.Events))
	}
}

func TestRevert_FailsLoudOnInvalidTypedConfigSnapshot(t *testing.T) {
	s := newStore(t)
	// tc_config_set の Prev が型付きポインタで不正な設定（roundUnit=7）。
	s.db.Events = []*Event{{
		ID: "e_000001", TS: nowTS(), BizDate: "2026-07-18",
		Kind:  "tc_config_set",
		Prev:  &TCConfig{RoundUnit: 7, RoundDir: "floor", StandardMinutes: 480},
		Next:  &TCConfig{RoundUnit: 1, RoundDir: "floor", StandardMinutes: 480},
		Label: "タイムカード設定変更",
	}}
	if _, _, _, err := s.Revert("e_000001"); err == nil {
		t.Fatalf("不正な型付き設定スナップショットで revert がエラーにならない")
	}
	// 失敗した revert は設定・イベントを一切変えない。
	if s.db.TCConfig.RoundUnit != 1 {
		t.Fatalf("失敗した revert で設定が変わった: %+v", *s.db.TCConfig)
	}
	if len(s.db.Events) != 1 {
		t.Fatalf("失敗した revert でイベントが増減した: %d", len(s.db.Events))
	}
}
