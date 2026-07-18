package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"koteihyo/internal/store"
)

// newTestServer は seed 済みストア（スタッフ s_8=小池 / s_9=井上 / s_10=浜田）に対する
// HTTP ハンドラを組み立てる。
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "koteihyo.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv := New(st, fstest.MapFS{}, 8080, "http://127.0.0.1:8080", []string{"http://127.0.0.1:8080"}, "v-test", nil)
	return srv.Handler()
}

// do は 1 リクエストを実行し、ステータスとボディを返す。
func do(t *testing.T, h http.Handler, method, path string, body interface{}) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func decodeMap(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json decode: %v (body=%s)", err, string(b))
	}
	return m
}

const (
	sKoike = "s_8"  // 小池
	sInoue = "s_9"  // 井上
	sHama  = "s_10" // 浜田
)

func today() string { return store.Today() }

// ---- bootstrap ----

func TestBootstrapHasTCConfig(t *testing.T) {
	h := newTestServer(t)
	code, b := do(t, h, http.MethodGet, "/api/bootstrap", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	cfg, ok := m["tcConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("tcConfig missing/wrong type: %s", b)
	}
	if cfg["roundUnit"].(float64) != 1 {
		t.Fatalf("default roundUnit want 1, got %v", cfg["roundUnit"])
	}
	if _, ok := cfg["roundDir"]; !ok {
		t.Fatalf("roundDir missing")
	}
}

// ---- today ----

func TestTimecardToday(t *testing.T) {
	h := newTestServer(t)
	code, b := do(t, h, http.MethodGet, "/api/timecard/today", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	if m["date"] != today() {
		t.Fatalf("date want %s got %v", today(), m["date"])
	}
	if _, err := time.Parse(time.RFC3339, m["serverNow"].(string)); err != nil {
		t.Fatalf("serverNow not RFC3339: %v", err)
	}
	staff := m["staff"].([]interface{})
	if len(staff) != 3 {
		t.Fatalf("want 3 active staff, got %d", len(staff))
	}
	if _, ok := m["canUndo"]; !ok {
		t.Fatalf("canUndo missing")
	}
	if _, ok := m["canRedo"]; !ok {
		t.Fatalf("canRedo missing")
	}
	if _, ok := m["anomalies"]; !ok {
		t.Fatalf("anomalies missing")
	}
}

func TestTimecardTodayMethodNotAllowed(t *testing.T) {
	h := newTestServer(t)
	code, _ := do(t, h, http.MethodPost, "/api/timecard/today", nil)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", code)
	}
}

// ---- punch ----

func TestPunchFlow(t *testing.T) {
	h := newTestServer(t)

	// 出勤。
	code, b := do(t, h, http.MethodPost, "/api/timecard/punch", map[string]interface{}{"staffId": sKoike, "action": "in"})
	if code != http.StatusOK {
		t.Fatalf("in: status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	if m["state"] != "working" {
		t.Fatalf("after in, state want working got %v", m["state"])
	}
	if m["rev"].(float64) != 1 {
		t.Fatalf("after in, rev want 1 got %v", m["rev"])
	}
	if m["canUndo"] != true {
		t.Fatalf("canUndo want true after punch")
	}

	// 休憩開始。
	code, b = do(t, h, http.MethodPost, "/api/timecard/punch", map[string]interface{}{"staffId": sKoike, "action": "break_start"})
	if code != http.StatusOK {
		t.Fatalf("break_start: status=%d body=%s", code, b)
	}
	m = decodeMap(t, b)
	if m["state"] != "break" {
		t.Fatalf("after break_start, state want break got %v", m["state"])
	}
	if m["rev"].(float64) != 2 {
		t.Fatalf("rev want 2 got %v", m["rev"])
	}

	// 不正遷移: 休憩中に出勤 → 400。
	code, b = do(t, h, http.MethodPost, "/api/timecard/punch", map[string]interface{}{"staffId": sKoike, "action": "in"})
	if code != http.StatusBadRequest {
		t.Fatalf("illegal in during break: want 400 got %d body=%s", code, b)
	}
}

func TestPunchUnknownStaff(t *testing.T) {
	h := newTestServer(t)
	code, _ := do(t, h, http.MethodPost, "/api/timecard/punch", map[string]interface{}{"staffId": "nope", "action": "in"})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", code)
	}
}

func TestPunchBadAction(t *testing.T) {
	h := newTestServer(t)
	code, _ := do(t, h, http.MethodPost, "/api/timecard/punch", map[string]interface{}{"staffId": sKoike, "action": "teleport"})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", code)
	}
}

// ---- month ----

func TestTimecardMonth(t *testing.T) {
	h := newTestServer(t)
	ym := today()[:7]
	code, b := do(t, h, http.MethodGet, "/api/timecard/month?staffId="+sKoike+"&month="+ym, nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	if m["month"] != ym {
		t.Fatalf("month want %s got %v", ym, m["month"])
	}
	if len(m["roster"].([]interface{})) == 0 {
		t.Fatalf("roster empty")
	}
	if len(m["days"].([]interface{})) < 28 {
		t.Fatalf("days too few: %d", len(m["days"].([]interface{})))
	}
	if _, ok := m["config"].(map[string]interface{}); !ok {
		t.Fatalf("config missing")
	}
	if _, ok := m["canUndo"]; !ok {
		t.Fatalf("canUndo missing")
	}
}

func TestTimecardMonthMissingParam(t *testing.T) {
	h := newTestServer(t)
	// month 欠落 → 400。
	code, _ := do(t, h, http.MethodGet, "/api/timecard/month?staffId="+sKoike, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("missing month: want 400 got %d", code)
	}
	// staffId 欠落 → 400。
	code, _ = do(t, h, http.MethodGet, "/api/timecard/month?month="+today()[:7], nil)
	if code != http.StatusBadRequest {
		t.Fatalf("missing staffId: want 400 got %d", code)
	}
}

// ---- day（打刻修正 + 409） ----

func TestTimecardDayEditAndConflict(t *testing.T) {
	h := newTestServer(t)
	d := today()
	rev0 := 0

	// 新規作成（rev 0 → 1）。
	code, b := do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": d, "in": "09:00", "out": "18:00",
		"breaks": []interface{}{}, "attendanceNote": "", "expectedRev": rev0,
	})
	if code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	if m["rev"].(float64) != 1 {
		t.Fatalf("after create rev want 1 got %v", m["rev"])
	}

	// stale rev（0）で再送 → 409 + currentDay/currentRev。
	code, b = do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": d, "in": "10:00", "out": "19:00",
		"breaks": []interface{}{}, "attendanceNote": "", "expectedRev": rev0,
	})
	if code != http.StatusConflict {
		t.Fatalf("stale rev: want 409 got %d body=%s", code, b)
	}
	m = decodeMap(t, b)
	if m["currentRev"].(float64) != 1 {
		t.Fatalf("409 currentRev want 1 got %v", m["currentRev"])
	}
	if _, ok := m["currentDay"].(map[string]interface{}); !ok {
		t.Fatalf("409 currentDay missing: %s", b)
	}
	if _, ok := m["error"]; !ok {
		t.Fatalf("409 error missing")
	}
}

func TestTimecardDayMissingExpectedRevIs409(t *testing.T) {
	h := newTestServer(t)
	d := today()
	// まず rev を 1 にする。
	do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": d, "in": "09:00", "out": "18:00", "expectedRev": 0,
	})
	// expectedRev フィールドを省略 → 409（番兵 -1）。
	code, b := do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": d, "in": "09:00", "out": "18:00",
	})
	if code != http.StatusConflict {
		t.Fatalf("missing expectedRev: want 409 got %d body=%s", code, b)
	}
}

func TestTimecardDayValidation400(t *testing.T) {
	h := newTestServer(t)
	// 未来日 → 400。
	code, _ := do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": "2999-01-01", "in": "09:00", "out": "18:00", "expectedRev": 0,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("future date: want 400 got %d", code)
	}
	// 非正規時刻（"9:05"）→ 400。
	code, _ = do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sInoue, "date": today(), "in": "9:05", "out": "18:00", "expectedRev": 0,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad time: want 400 got %d", code)
	}
}

// ---- config ----

func TestTimecardConfig(t *testing.T) {
	h := newTestServer(t)
	code, b := do(t, h, http.MethodPost, "/api/timecard/config", map[string]interface{}{
		"roundUnit": 15, "roundDir": "nearest", "standardMinutes": 480,
	})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, b)
	}
	m := decodeMap(t, b)
	cfg := m["config"].(map[string]interface{})
	if cfg["roundUnit"].(float64) != 15 {
		t.Fatalf("roundUnit want 15 got %v", cfg["roundUnit"])
	}
	if _, ok := m["canUndo"]; !ok {
		t.Fatalf("canUndo missing")
	}
}

func TestTimecardConfigInvalid(t *testing.T) {
	h := newTestServer(t)
	code, _ := do(t, h, http.MethodPost, "/api/timecard/config", map[string]interface{}{
		"roundUnit": 7, "roundDir": "nearest", "standardMinutes": 480,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad roundUnit: want 400 got %d", code)
	}
	code, _ = do(t, h, http.MethodPost, "/api/timecard/config", map[string]interface{}{
		"roundUnit": 15, "roundDir": "sideways", "standardMinutes": 480,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad roundDir: want 400 got %d", code)
	}
}

// ---- export.csv ----

func csvHeaderLine(t *testing.T, b []byte) string {
	t.Helper()
	// BOM を剥がす。
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	} else {
		t.Fatalf("missing UTF-8 BOM")
	}
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], "\r")
	}
	return strings.TrimRight(s, "\r")
}

func TestExportCSV_RoundUnit1_OmitsRefColumn(t *testing.T) {
	h := newTestServer(t)
	d := today()
	// 完全な1日を作る。
	do(t, h, http.MethodPost, "/api/timecard/day", map[string]interface{}{
		"staffId": sHama, "date": d, "in": "09:00", "out": "18:00", "expectedRev": 0,
	})
	ym := d[:7]

	req := httptest.NewRequest(http.MethodGet, "/api/timecard/export.csv?month="+ym+"&staffId="+sHama, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("csv status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type=%s", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "timecard_"+ym+"_"+sHama+".csv") {
		t.Fatalf("content-disposition=%s", cd)
	}
	header := csvHeaderLine(t, rec.Body.Bytes())
	if !strings.Contains(header, "スタッフ") || !strings.Contains(header, "実働") {
		t.Fatalf("header missing expected cols: %s", header)
	}
	// roundUnit==1（既定）→ 参考列は無い。
	if strings.Contains(header, "実働(丸め・参考)") {
		t.Fatalf("ref column must be omitted at roundUnit=1: %s", header)
	}
	if !strings.Contains(rec.Body.String(), "18:00") {
		t.Fatalf("expected out time in body")
	}
	if !strings.Contains(rec.Body.String(), "合計") {
		t.Fatalf("expected total row")
	}
}

func TestExportCSV_RoundUnit15_IncludesRefColumn(t *testing.T) {
	h := newTestServer(t)
	// 丸め 15 分を設定。
	do(t, h, http.MethodPost, "/api/timecard/config", map[string]interface{}{
		"roundUnit": 15, "roundDir": "floor", "standardMinutes": 480,
	})
	ym := today()[:7]
	req := httptest.NewRequest(http.MethodGet, "/api/timecard/export.csv?month="+ym, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("csv status=%d", rec.Code)
	}
	// staffId 省略 → 全員。ファイル名にスタッフ無し。
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "timecard_"+ym+".csv") {
		t.Fatalf("content-disposition=%s", cd)
	}
	header := csvHeaderLine(t, rec.Body.Bytes())
	if !strings.Contains(header, "実働(丸め・参考)") {
		t.Fatalf("ref column must be present at roundUnit=15: %s", header)
	}
}

func TestExportCSV_BadMonth400(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/timecard/export.csv?month=nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad month: want 400 got %d", rec.Code)
	}
}
