package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"koteihyo/internal/store"
)

// Server は HTTP ハンドラ群をまとめる。
type Server struct {
	st      *store.Store
	webFS   fs.FS    // web ディレクトリ（fs.Sub 済み）
	port    int      // 実際に listen しているポート
	lanURL  string   // LAN 用 URL（最有力。http://<ip>:<port>）
	lanURLs []string // 接続候補 URL すべて（つながらない時のフォールバック表示用）
	fileSrv http.Handler
}

// New は API サーバを生成する。webFS は web 配下を指す（fs.Sub の結果）。
func New(st *store.Store, webFS fs.FS, port int, lanURL string, lanURLs []string) *Server {
	return &Server{
		st:      st,
		webFS:   webFS,
		port:    port,
		lanURL:  lanURL,
		lanURLs: lanURLs,
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
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"staff":     b.Staff,
		"processes": b.Processes,
		"today":     b.Today,
		"weekStart": b.WeekStart,
		"port":      s.port,
		"lanURL":    s.lanURL,
		"lanURLs":   s.lanURLs,
	})
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
		writeErr(w, http.StatusBadRequest, err.Error())
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
