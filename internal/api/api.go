package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"koteihyo/internal/store"
	"koteihyo/internal/update"
)

// Server は HTTP ハンドラ群をまとめる。
type Server struct {
	st      *store.Store
	webFS   fs.FS    // web ディレクトリ（fs.Sub 済み）
	port    int      // 実際に listen しているポート
	lanURL  string   // LAN 用 URL（最有力。http://<ip>:<port>）
	lanURLs []string // 接続候補 URL すべて（つながらない時のフォールバック表示用）
	version string   // アプリ版（§B、bootstrap で配る）
	mgr     *update.Manager
	fileSrv http.Handler
}

// New は API サーバを生成する。webFS は web 配下を指す（fs.Sub の結果）。
// version はアプリ版（ldflags 埋め込み）、mgr は自動アップデート管理（nil 可＝テスト）。
func New(st *store.Store, webFS fs.FS, port int, lanURL string, lanURLs []string, version string, mgr *update.Manager) *Server {
	return &Server{
		st:      st,
		webFS:   webFS,
		port:    port,
		lanURL:  lanURL,
		lanURLs: lanURLs,
		version: version,
		mgr:     mgr,
		fileSrv: http.FileServer(http.FS(webFS)),
	}
}

// Handler は全ルートを登録した http.Handler を返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API（§6）。
	mux.HandleFunc("/api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/api/week", s.handleWeek)
	mux.HandleFunc("/api/count", s.handleCount)
	mux.HandleFunc("/api/memo", s.handleMemo)
	mux.HandleFunc("/api/undo", s.handleUndo)
	mux.HandleFunc("/api/redo", s.handleRedo)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/history/revert", s.handleRevert)
	mux.HandleFunc("/api/totals", s.handleTotals)
	mux.HandleFunc("/api/export.csv", s.handleExportCSV)
	mux.HandleFunc("/api/process", s.handleProcess)
	mux.HandleFunc("/api/process/delete", s.handleProcessDelete)
	mux.HandleFunc("/api/process/reorder", s.handleProcessReorder)
	mux.HandleFunc("/api/staff", s.handleStaff)
	mux.HandleFunc("/api/staff/delete", s.handleStaffDelete)
	mux.HandleFunc("/api/staff/reorder", s.handleStaffReorder)

	// タイムカード（§A7）。
	mux.HandleFunc("/api/timecard/today", s.handleTimecardToday)
	mux.HandleFunc("/api/timecard/punch", s.handleTimecardPunch)
	mux.HandleFunc("/api/timecard/month", s.handleTimecardMonth)
	mux.HandleFunc("/api/timecard/day", s.handleTimecardDay)
	mux.HandleFunc("/api/timecard/export.csv", s.handleTimecardExportCSV)
	mux.HandleFunc("/api/timecard/config", s.handleTimecardConfig)

	// 自動アップデート（§B）。
	mux.HandleFunc("/api/update/status", s.handleUpdateStatus)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)

	// 静的配信（"/" → index.html、その他 web 配下）。
	mux.HandleFunc("/", s.handleStatic)

	return mux
}

// ---- 共通ヘルパ ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeBody は POST の JSON ボディを dst に読み込む。
func decodeBody(r *http.Request, dst interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("ボディがありません")
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("ボディの解析に失敗しました: %v", err)
	}
	return nil
}

// requireMethod は許可メソッド以外を 405 で弾く。
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeErr(w, http.StatusMethodNotAllowed, "許可されていないメソッドです: "+r.Method)
		return false
	}
	return true
}

// ---- 静的配信 ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	// 未登録の /api/* は静的配信ではなく契約§6のエラー形（JSON）で 404 を返す。
	// （登録済みの /api/... は各 HandleFunc が処理するためここには来ない。）
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// API 以外はすべて埋め込み web 配下を配信する。
	// "/" は http.FileServer の既定動作で web/index.html の中身が返る。
	// 重要: "/" を "/index.html" に書き換えないこと。FileServer は
	// "/index.html" を "./" へ 301 リダイレクトするため、書き換えると
	// ブラウザで "/" ⇄ "/" のリダイレクトループになりアプリが開けない。
	s.fileSrv.ServeHTTP(w, r)
}

// ---- /api/bootstrap ----

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	b := s.st.Bootstrap()
	// 応答本体を「書き切れたことを確認」してから .old 後始末を合図する（finding 10）。
	// エンコード/書込に失敗したら合図しない（クライアントは版一致を確認できないため、
	// 旧 exe を消してはならない）。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]interface{}{
		"staff":     b.Staff,
		"processes": b.Processes,
		"today":     b.Today,
		"weekStart": b.WeekStart,
		"port":      s.port,
		"lanURL":    s.lanURL,
		"lanURLs":   s.lanURLs,
		// タイムカード設定（設定画面は bootstrap から読む — §A7）。
		"tcConfig": s.st.TimecardConfig(),
		// アプリ版（§B。更新バナー／再起動後の版一致判定に使う）。
		"version": s.version,
	}); err != nil {
		// 書き切れていない → 後始末は合図しない（次回の bootstrap 200 で再試行）。
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 再起動モードで最初の bootstrap 200 応答後、条件が揃えば旧 exe を後始末する
	// （§B3.4、sync.Once で一度きり）。
	if s.mgr != nil {
		s.mgr.NotifyBootstrapServed()
	}
}

// ---- /api/week ----

func (s *Server) handleWeek(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	start := r.URL.Query().Get("start")
	staffID := r.URL.Query().Get("staffId")
	if start == "" {
		start, _ = store.MondayOf(store.Today())
	} else {
		// 与えられた日付を含む週の月曜に正規化（任意日でも安全に動く）。
		if m, err := store.MondayOf(start); err == nil {
			start = m
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if staffID == "" {
		writeErr(w, http.StatusBadRequest, "staffId は必須です")
		return
	}
	wd, err := s.st.Week(start, staffID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wd)
}

// ---- /api/count ----

func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		StaffID   string `json:"staffId"`
		Date      string `json:"date"`
		ProcessID string `json:"processId"`
		Op        string `json:"op"`
		Value     *int   `json:"value"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	val := 0
	if req.Value != nil {
		val = *req.Value
	}
	next, canUndo, canRedo, err := s.st.CountOp(req.StaffID, req.Date, req.ProcessID, req.Op, val)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"value":   next,
		"canUndo": canUndo,
		"canRedo": canRedo,
	})
}

// ---- /api/memo ----

func (s *Server) handleMemo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		StaffID string `json:"staffId"`
		Date    string `json:"date"`
		Text    string `json:"text"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	canUndo, canRedo, err := s.st.MemoSet(req.StaffID, req.Date, req.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"canUndo": canUndo,
		"canRedo": canRedo,
	})
}

// ---- /api/undo ----

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	res, err := s.st.Undo()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- /api/redo ----

func (s *Server) handleRedo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	res, err := s.st.Redo()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- /api/history ----

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	date := r.URL.Query().Get("date")
	days, err := s.st.History(date)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if days == nil {
		days = []store.HistoryDay{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"days": days})
}

// ---- /api/history/revert ----

func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		EventID string `json:"eventId"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	count, canUndo, canRedo, err := s.st.Revert(req.EventID)
	if err != nil {
		// 未知/欠落 ID（ValidationError）→ 400、復号破損・保存失敗 → 500（finding 6）。
		writeTimecardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"revertedCount": count,
		"canUndo":       canUndo,
		"canRedo":       canRedo,
	})
}

// ---- /api/totals ----

func (s *Server) handleTotals(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	start := q.Get("start")
	end := q.Get("end")
	staffID := q.Get("staffId")
	if start == "" || end == "" {
		writeErr(w, http.StatusBadRequest, "start と end は必須です")
		return
	}
	t, err := s.st.Totals(start, end, staffID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ---- /api/export.csv ----

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	start := q.Get("start")
	end := q.Get("end")
	if start == "" || end == "" {
		writeErr(w, http.StatusBadRequest, "start と end は必須です")
		return
	}
	rows, err := s.st.ExportCSVData(start, end)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var buf bytes.Buffer
	// UTF-8 BOM（Excel が日本語を正しく開けるように）。
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(&buf)
	// 日本語ヘッダ。
	_ = cw.Write([]string{"スタッフ", "工程", "単価", "件数", "売上"})
	var totalCount, totalSales int
	for _, row := range rows {
		_ = cw.Write([]string{
			row.StaffName,
			row.ProcessName,
			strconv.Itoa(row.Price),
			strconv.Itoa(row.Count),
			strconv.Itoa(row.Sales),
		})
		totalCount += row.Count
		totalSales += row.Sales
	}
	// 合計行。
	_ = cw.Write([]string{"合計", "", "", strconv.Itoa(totalCount), strconv.Itoa(totalSales)})
	cw.Flush()
	if err := cw.Error(); err != nil {
		writeErr(w, http.StatusInternalServerError, "CSV生成に失敗しました")
		return
	}

	filename := fmt.Sprintf("koteihyo_%s_%s.csv", start, end)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// ---- /api/process ----

func (s *Server) handleProcess(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Price int    `json:"price"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.st.ProcessUpsert(req.ID, req.Name, req.Price)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"process": p, "canUndo": cu, "canRedo": cr})
}

func (s *Server) handleProcessDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.ProcessDelete(req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "canUndo": cu, "canRedo": cr})
}

func (s *Server) handleProcessReorder(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.ProcessReorder(req.IDs); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "canUndo": cu, "canRedo": cr})
}

// ---- /api/staff ----

func (s *Server) handleStaff(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st, err := s.st.StaffUpsert(req.ID, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"staff": st, "canUndo": cu, "canRedo": cr})
}

func (s *Server) handleStaffDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.StaffDelete(req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "canUndo": cu, "canRedo": cr})
}

func (s *Server) handleStaffReorder(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.StaffReorder(req.IDs); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "canUndo": cu, "canRedo": cr})
}

// ---- タイムカード（§A7） ----

// writeTimecardErr はストアの型付きエラーを §A7 のエラー分類で振り分ける。
//
//	*store.ConflictError（= sentinel ErrConflict）→ 409（currentDay/currentRev 同送）
//	*store.ValidationError                        → 400
//	その他（復号破損・保存失敗）                  → 500
func writeTimecardErr(w http.ResponseWriter, err error) {
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": ce.Error(),
			// currentDay も API DTO（attendanceNote）で返す（finding 13）。
			"currentDay": store.NewDayDTO(ce.CurrentDay),
			"currentRev": ce.CurrentRev,
		})
		return
	}
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// ---- /api/timecard/today ----

func (s *Server) handleTimecardToday(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.st.TimecardToday()
	if err != nil {
		writeTimecardErr(w, err)
		return
	}
	// TCTodayData は canUndo/canRedo を含む平坦な構造。
	writeJSON(w, http.StatusOK, data)
}

// ---- /api/timecard/punch ----

func (s *Server) handleTimecardPunch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		StaffID string `json:"staffId"`
		Action  string `json:"action"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.st.Punch(req.StaffID, req.Action)
	if err != nil {
		writeTimecardErr(w, err)
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"day":     store.NewDayDTO(res.Day), // API DTO（attendanceNote）、finding 13。
		"state":   res.State,
		"rev":     res.Rev,
		"canUndo": cu,
		"canRedo": cr,
	})
}

// ---- /api/timecard/month ----

func (s *Server) handleTimecardMonth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	staffID := q.Get("staffId")
	month := q.Get("month")
	data, err := s.st.TimecardMonth(staffID, month)
	if err != nil {
		writeTimecardErr(w, err)
		return
	}
	// TCMonthData は roster/config/canUndo/canRedo を含む平坦な構造。
	writeJSON(w, http.StatusOK, data)
}

// ---- /api/timecard/day ----

func (s *Server) handleTimecardDay(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		StaffID        string            `json:"staffId"`
		Date           string            `json:"date"`
		In             string            `json:"in"`
		Out            string            `json:"out"`
		Breaks         []store.BreakSpan `json:"breaks"`
		AttendanceNote string            `json:"attendanceNote"`
		ExpectedRev    *int              `json:"expectedRev"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// expectedRev 欠落は必ず衝突扱い（§A10-9）: 実 rev は常に 0 以上なので、
	// 番兵 -1 を渡すとストアが必ず 409（currentDay/currentRev 同送）を返す。
	expected := -1
	if req.ExpectedRev != nil {
		expected = *req.ExpectedRev
	}
	res, err := s.st.TimecardDay(req.StaffID, req.Date, req.In, req.Out, req.Breaks, req.AttendanceNote, expected)
	if err != nil {
		writeTimecardErr(w, err)
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"day":     store.NewDayDTO(res.Day), // API DTO（attendanceNote）、finding 13。
		"rev":     res.Rev,
		"canUndo": cu,
		"canRedo": cr,
	})
}

// ---- /api/timecard/config ----

func (s *Server) handleTimecardConfig(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		RoundUnit       int    `json:"roundUnit"`
		RoundDir        string `json:"roundDir"`
		StandardMinutes int    `json:"standardMinutes"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := s.st.TimecardConfigSet(req.RoundUnit, req.RoundDir, req.StandardMinutes)
	if err != nil {
		writeTimecardErr(w, err)
		return
	}
	cu, cr := s.st.CanUndoRedo()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":  cfg,
		"canUndo": cu,
		"canRedo": cr,
	})
}

// ---- /api/timecard/export.csv ----

func (s *Server) handleTimecardExportCSV(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	month := q.Get("month")
	staffID := q.Get("staffId") // 省略で全員（roster 順）。
	data, err := s.st.TimecardExportData(month, staffID)
	if err != nil {
		writeTimecardErr(w, err)
		return
	}

	// 参考（丸め）列は roundUnit==1 のとき列ごと省く（§A8）。
	includeRef := data.RoundUnit != 1

	var buf bytes.Buffer
	// UTF-8 BOM（Excel が日本語を正しく開けるように）。
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(&buf)

	// ヘッダ（§A8）。
	header := []string{"スタッフ", "日付", "曜日", "出勤", "退勤", "休憩(分)", "実働", "実働(時間)"}
	if includeRef {
		header = append(header, "実働(丸め・参考)")
	}
	header = append(header, "所定超過(参考)", "備考", "フラグ")
	_ = cw.Write(header)

	for bi, block := range data.Staff {
		if bi > 0 {
			// スタッフブロックの区切り（空行）。
			_ = cw.Write([]string{})
		}
		for _, d := range block.Days {
			row := []string{
				block.StaffName, // csvSafe 済み（store）。
				d.Date,
				d.Weekday,
				d.In,
				d.Out,
				strconv.Itoa(d.BreakMinutes),
				d.WorkedRaw,
				d.WorkedDecimal,
			}
			if includeRef {
				row = append(row, d.WorkedRef)
			}
			row = append(row, d.OverStd, d.Note /* csvSafe 済み */, d.Flags)
			_ = cw.Write(row)
		}
		// 合計行（生ベース。参考列は空）。
		total := []string{block.StaffName, "合計", "", "", "", "", block.TotalWorked, block.TotalDecimal}
		if includeRef {
			total = append(total, "")
		}
		total = append(total, "", block.TotalNoteText, "")
		_ = cw.Write(total)
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		writeErr(w, http.StatusInternalServerError, "CSV生成に失敗しました")
		return
	}

	filename := fmt.Sprintf("timecard_%s.csv", data.Month)
	if staffID != "" {
		filename = fmt.Sprintf("timecard_%s_%s.csv", data.Month, staffID)
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// ---- 自動アップデート（§B） ----

// handleUpdateStatus は現在の更新状態を返す（§B2）。lazy 確認は Manager 側で行う。
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.mgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "自動更新は利用できません")
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Status())
}

// handleUpdateApply は更新を適用する（§B3）。Windows 以外・dev は 400。
// ステージング（DL + sha256/MZ/サイズ検証）成功後、レスポンスを flush してから
// main の apply チャネルへシグナルし、return する（返ったハンドラは flush 済み → 安全）。
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if runtime.GOOS != "windows" {
		writeErr(w, http.StatusBadRequest, "手動で更新してください")
		return
	}
	if s.mgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "自動更新は利用できません")
		return
	}
	// ステージング前にファイアウォール修復ゲートを確保する（finding 2）。修復(UAC)が
	// 進行中で時間内に確保できなければ busy を返し、更新 UI には入らない。応答後に main が
	// この保持済みゲートに依存してスワップするため、成功時はここで解放しない
	// （プロセスがスワップで終了する）。ステージング失敗時のみ解放する。
	if !s.mgr.AcquireApplyGate(r.Context()) {
		writeErr(w, http.StatusConflict, "ファイアウォール設定の確認中です。しばらくして再試行してください")
		return
	}
	plan, newVersion, err := s.mgr.Stage()
	if err != nil {
		s.mgr.ReleaseFirewallGate() // 失敗 → 保持ゲートを解放（修復の再試行を妨げない）。
		if errors.Is(err, update.ErrApplyInProgress) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		var nae *update.NotApplicableError
		if errors.As(err, &nae) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// ダウンロード/検証の失敗など → 500。
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// レスポンスを書いて flush → その後にシグナル → return（§B3-2）。
	// ゲートは保持したまま（main がスワップ、finding 2）。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]interface{}{
		"ok":         true,
		"restarting": true,
		"newVersion": newVersion,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.mgr.Signal(plan)
}
