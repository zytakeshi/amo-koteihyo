(function () {
  "use strict";

  /* ============================================================
   * Formatting helpers
   * ========================================================== */
  const yen = new Intl.NumberFormat("ja-JP", {
    style: "currency",
    currency: "JPY",
    maximumFractionDigits: 0,
  });

  const WEEKDAYS = ["月", "火", "水", "木", "金", "土", "日"]; // Monday-first
  const SUN_WD = "日";
  const SAT_WD = "土";

  function escapeHtml(value) {
    return String(value == null ? "" : value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#039;");
  }

  // Parse "YYYY-MM-DD" as a JST-anchored Date (avoids TZ drift).
  function parseDate(iso) {
    return new Date(`${iso}T00:00:00+09:00`);
  }

  function pad2(n) {
    return String(n).padStart(2, "0");
  }

  // Build "YYYY-MM-DD" from year/month(1-12)/day numbers.
  function isoFrom(y, m, d) {
    return `${y}-${pad2(m)}-${pad2(d)}`;
  }

  // 0=Mon .. 6=Sun for a given iso date.
  function weekdayIndex(iso) {
    const js = parseDate(iso).getDay(); // 0=Sun..6=Sat
    return (js + 6) % 7;
  }

  // Monday (iso) of the week containing the given iso date.
  function weekStartOf(iso) {
    const d = parseDate(iso);
    const offset = weekdayIndex(iso);
    d.setDate(d.getDate() - offset);
    return isoFrom(d.getFullYear(), d.getMonth() + 1, d.getDate());
  }

  function addDays(iso, days) {
    const d = parseDate(iso);
    d.setDate(d.getDate() + days);
    return isoFrom(d.getFullYear(), d.getMonth() + 1, d.getDate());
  }

  function addMonths(iso, months) {
    const d = parseDate(iso);
    d.setDate(1);
    d.setMonth(d.getMonth() + months);
    return isoFrom(d.getFullYear(), d.getMonth() + 1, d.getDate());
  }

  function shortMD(iso) {
    const d = parseDate(iso);
    return `${d.getMonth() + 1}/${d.getDate()}`;
  }

  function wdOf(iso) {
    return WEEKDAYS[weekdayIndex(iso)];
  }

  /* ============================================================
   * State
   * ========================================================== */
  const state = {
    view: "calendar", // calendar | grid | history | settings | connect
    ready: false,
    error: "",

    // bootstrap
    staff: [],
    processes: [],
    today: "",
    weekStart: "",
    lanURL: "",
    port: 0,

    // calendar
    calMonth: "", // iso of any day in the displayed month
    monthMarks: {}, // "YYYY-MM-DD" -> { input:bool, hist:bool }

    // grid
    staffId: "",
    week: null, // { start, dates[7], staffId, counts:{pid:[7]}, memos:[7], canUndo, canRedo }
    memoDayIdx: 0,
    // Whole-shop totals for the displayed week (authoritative grand total).
    weekTotals: null, // { grand:{count,sales}, ... } from /api/totals

    // global undo/redo flags (kept in sync from every mutating response)
    canUndo: false,
    canRedo: false,

    // history
    history: { days: [] },
    historyFilter: "",

    // settings export range
    exportStart: "",
    exportEnd: "",

    // ---- timecard ----
    timecardTab: "punch", // punch | month
    tcToday: null, // /api/timecard/today response
    tcMonth: null, // /api/timecard/month response
    tcMonthStaffId: "",
    tcMonthYM: "", // "YYYY-MM"
    tcEditDraft: null, // { date, in, out, breaks:[{start,end}], attendanceNote, expectedRev }
    tcEditDirty: false,
    tcClockAnchor: null, // { serverMs, perf }
    tcConfig: null, // { roundUnit, roundDir, standardMinutes } from bootstrap/config
    tcConfirmOpen: false, // inline 破棄 confirm bar
    tcPending: null, // deferred draft-destroying action (run on 破棄)
    tcDeleteConfirmOpen: false, // inline「この日の打刻を全部消す」confirm bar

    // ---- 自動アップデート（§B） ----
    version: "", // running app version (bootstrap)
    updateStatus: null, // /api/update/status: { current, latest, available, notes, phase, canApply }
    updateDismissed: false, // session-only 「あとで」
    updateConfirmOpen: false, // inline 更新確認バー
    updating: false, // 更新中フルスクリーン
    updateTarget: "", // newVersion being applied (poll bootstrap.version == this)
    updateTimedOut: false, // restart poll exceeded 90s
  };

  const root = document.querySelector("#app");
  let toastTimer = null;

  /* ============================================================
   * API client (same-origin relative paths)
   * ========================================================== */
  async function api(path, options) {
    const res = await fetch(path, {
      headers: { "Content-Type": "application/json" },
      ...options,
    });
    let body = null;
    const text = await res.text();
    if (text) {
      try {
        body = JSON.parse(text);
      } catch (e) {
        body = null;
      }
    }
    if (!res.ok) {
      const msg = (body && body.error) || `エラー (${res.status})`;
      throw new Error(msg);
    }
    return body;
  }

  function post(path, payload) {
    return api(path, { method: "POST", body: JSON.stringify(payload || {}) });
  }

  function get(path) {
    return api(path, { method: "GET" });
  }

  // POST that surfaces the raw status + parsed body (never throws on non-2xx),
  // so callers can branch on 409 vs 400 (timecard 打刻修正 optimistic-concurrency).
  async function postRaw(path, payload) {
    const res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload || {}),
    });
    let body = null;
    const text = await res.text();
    if (text) {
      try {
        body = JSON.parse(text);
      } catch (e) {
        body = null;
      }
    }
    return { ok: res.ok, status: res.status, body };
  }

  // Track undo/redo flags from any mutating response.
  function syncUndoFlags(resp) {
    if (!resp) return;
    if (typeof resp.canUndo === "boolean") state.canUndo = resp.canUndo;
    if (typeof resp.canRedo === "boolean") state.canRedo = resp.canRedo;
  }

  /* ============================================================
   * Toast
   * ========================================================== */
  function toast(message, isError) {
    let el = document.querySelector(".toast");
    if (!el) {
      el = document.createElement("div");
      el.className = "toast";
      document.body.appendChild(el);
    }
    el.textContent = message;
    el.classList.toggle("is-error", !!isError);
    // reflow to restart transition
    void el.offsetWidth;
    el.classList.add("is-shown");
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      el.classList.remove("is-shown");
    }, isError ? 3200 : 1800);
  }

  function reportError(err) {
    console.error(err);
    toast(err && err.message ? err.message : "通信に失敗しました", true);
  }

  /* ============================================================
   * Lookups
   * ========================================================== */
  function activeProcesses() {
    return state.processes.filter((p) => p.active !== false);
  }

  function activeStaff() {
    return state.staff.filter((s) => s.active !== false);
  }

  function staffName(id) {
    const s = state.staff.find((x) => x.id === id);
    return s ? s.name : "";
  }

  function currentStaffName() {
    return staffName(state.staffId) || "スタッフ";
  }

  /* ============================================================
   * Data loading
   * ========================================================== */
  async function loadBootstrap() {
    const data = await get("/api/bootstrap");
    state.staff = Array.isArray(data.staff) ? data.staff : [];
    state.processes = Array.isArray(data.processes) ? data.processes : [];
    state.today = data.today || "";
    state.weekStart = data.weekStart || weekStartOf(data.today || "");
    state.lanURL = data.lanURL || "";
    state.lanURLs =
      Array.isArray(data.lanURLs) && data.lanURLs.length
        ? data.lanURLs
        : data.lanURL
          ? [data.lanURL]
          : [];
    state.port = data.port || 0;
    state.version = data.version || "";
    state.tcConfig = data.tcConfig || { roundUnit: 1, roundDir: "floor", standardMinutes: 480 };
    state.calMonth = state.today;
    state.exportStart = state.weekStart;
    state.exportEnd = addDays(state.weekStart, 6);

    const first = activeStaff()[0];
    state.staffId = first ? first.id : "";
    state.ready = true;
  }

  async function loadWeek() {
    if (!state.staffId) {
      state.week = null;
      return;
    }
    const q = `?start=${encodeURIComponent(state.weekStart)}&staffId=${encodeURIComponent(state.staffId)}`;
    const data = await get(`/api/week${q}`);
    state.week = data;
    syncUndoFlags(data);
    if (state.memoDayIdx > 6 || state.memoDayIdx < 0) state.memoDayIdx = 0;
  }

  // Whole-shop grand total for the displayed week (contract §5/§8: the "全体"
  // card must show every staff's total, not just the current staff's). No
  // staffId → server returns grand across all staff for the period.
  async function loadWeekTotals() {
    const start = state.weekStart;
    const end = addDays(start, 6);
    const q = `?start=${encodeURIComponent(start)}&end=${encodeURIComponent(end)}`;
    const data = await get(`/api/totals${q}`);
    state.weekTotals = data && data.grand ? data : { grand: { count: 0, sales: 0 } };
  }

  async function loadMonthMarks() {
    // Mark days that have input or history within the displayed month by
    // scanning history (covers both "input" and "history" since every input
    // produces an event). Cheap + offline-friendly via the history API.
    state.monthMarks = {};
    try {
      const data = await get(`/api/history`);
      const days = (data && data.days) || [];
      const monthPrefix = state.calMonth ? state.calMonth.slice(0, 7) : "";
      days.forEach((day) => {
        if (!day.date) return;
        const hasInput = (day.events || []).some((e) =>
          ["count_inc", "count_dec", "count_set", "memo_set", "tc_set"].includes(e.kind)
        );
        state.monthMarks[day.date] = {
          hist: true,
          input: hasInput,
        };
      });
      void monthPrefix;
    } catch (e) {
      // Non-fatal: dots are a nicety.
      console.warn("month marks load failed", e);
    }
  }

  // 自動アップデートの状態を取得（§B2）。非致命: 失敗しても黙って据え置く。
  async function loadUpdateStatus() {
    try {
      const data = await get("/api/update/status");
      state.updateStatus = data || null;
    } catch (e) {
      // 更新確認はおまけ。失敗しても本体機能に影響させない。
    }
  }

  // 起動直後の非同期確認（サーバ側 goroutine）が終わるまで数回だけ再取得する。
  // これによりリロード無しでバナーが出る（§B2/§B5、finding 5）。phase が "checking"
  // か latest 未取得の間だけ、指数バックオフで最大 retries 回ポーリングする。
  let updateStatusPolling = false;
  async function refreshUpdateStatus(retries) {
    if (updateStatusPolling) return;
    updateStatusPolling = true;
    try {
      await loadUpdateStatus();
    } finally {
      updateStatusPolling = false;
    }
    if (state.view === "calendar") render();
    const u = state.updateStatus;
    const pending = !u || u.phase === "checking" || (!u.available && !u.latest);
    if (pending && retries > 0) {
      setTimeout(() => refreshUpdateStatus(retries - 1), 2000);
    }
  }

  async function loadHistory() {
    const q = state.historyFilter ? `?date=${encodeURIComponent(state.historyFilter)}` : "";
    const data = await get(`/api/history${q}`);
    state.history = data && data.days ? data : { days: [] };
  }

  // Load everything the grid view needs (week cells + whole-shop grand total).
  // Each loader is wrapped with safe() so a totals failure never blanks the grid.
  async function loadGrid() {
    await safe(loadWeek);
    await safe(loadWeekTotals);
  }

  /* ============================================================
   * Navigation helpers
   * ========================================================== */
  async function goView(view) {
    resetTCEdit(); // leaving/entering a top-level view tears down any open editor (codex #2)
    state.view = view;
    if (view === "grid") {
      await loadGrid();
    } else if (view === "history") {
      await safe(loadHistory);
    } else if (view === "calendar") {
      await safe(loadMonthMarks);
      // 更新バナーはカレンダー画面に出す（§B2）。確認は非ブロッキング + 起動確認が
      // まだ走っている場合の再取得（finding 5）。
      refreshUpdateStatus(3);
    } else if (view === "timecard") {
      if (state.timecardTab === "month") {
        await safe(loadTCMonth);
      } else {
        await safe(loadTCToday);
      }
    }
    render();
    // Poll only while a multi-device-sensitive view (grid/history/timecard) is showing.
    syncPolling();
  }

  // Run an async loader, surfacing errors without throwing.
  async function safe(fn) {
    try {
      await fn();
    } catch (e) {
      reportError(e);
    }
  }

  /* ============================================================
   * Render: top bar (shared)
   * ========================================================== */
  function renderTopbar() {
    return `
      <header class="topbar">
        <div class="brand-lockup">
          <span class="brand-mark">A</span>
          <div>
            <p class="eyebrow">AMO BARBER 工程表</p>
            <h1>工程表</h1>
          </div>
        </div>
        <nav class="nav-actions" aria-label="画面切替">
          <button class="btn ${state.view === "calendar" ? "btn-ink" : "btn-ghost"}" data-action="nav" data-view="calendar" type="button">カレンダー</button>
          <button class="btn ${state.view === "history" ? "btn-ink" : "btn-ghost"}" data-action="nav" data-view="history" type="button">履歴</button>
          <button class="btn ${state.view === "settings" ? "btn-ink" : "btn-ghost"}" data-action="nav" data-view="settings" type="button">設定</button>
          <button class="btn ${state.view === "timecard" ? "btn-ink" : "btn-ghost"}" data-action="nav" data-view="timecard" type="button">タイムカード</button>
          <button class="btn ${state.view === "connect" ? "btn-ink" : "btn-ghost"}" data-action="nav" data-view="connect" type="button">接続</button>
        </nav>
      </header>
    `;
  }

  /* ============================================================
   * Render: undo/redo cluster (operation undo, distinct from "back")
   * ========================================================== */
  function renderUndoRedo() {
    return `
      <div class="undo-redo" role="group" aria-label="操作の取り消し / やり直し">
        <button class="btn btn-sm" data-action="undo" type="button" ${state.canUndo ? "" : "disabled"} title="直前の操作を取り消す">⟲ 元に戻す</button>
        <button class="btn btn-sm" data-action="redo" type="button" ${state.canRedo ? "" : "disabled"} title="取り消した操作をやり直す">⟳ やり直し</button>
      </div>
    `;
  }

  /* ============================================================
   * View: Calendar (home)
   * ========================================================== */
  // 更新バナー（§B2）: canApply=true → 今すぐ更新、false → 手動ダウンロード。
  // 更新確認中は inline 確認バーに差し替える（アプリに window.confirm は無い方針）。
  function renderUpdateBanner() {
    const u = state.updateStatus;
    if (!u || !u.available || state.updateDismissed) return "";
    const ver = escapeHtml(u.latest || "");
    if (state.updateConfirmOpen) {
      return `
        <div class="update-banner is-confirm">
          <span class="update-banner-text">更新するとアプリが再起動します。接続中の全員が約10秒つながらなくなります。よろしいですか？</span>
          <span class="update-banner-actions">
            <button class="btn btn-rose btn-sm" data-action="update-confirm" type="button">更新する</button>
            <button class="btn btn-ghost btn-sm" data-action="update-cancel" type="button">キャンセル</button>
          </span>
        </div>`;
    }
    if (u.canApply) {
      return `
        <div class="update-banner">
          <span class="update-banner-text">新しいバージョン ${ver} があります</span>
          <span class="update-banner-actions">
            <button class="btn btn-rose btn-sm" data-action="update-apply" type="button">今すぐ更新</button>
            <button class="btn btn-ghost btn-sm" data-action="update-dismiss" type="button">あとで</button>
          </span>
        </div>`;
    }
    // 手動更新（読取専用フォルダ設置 / 開発ビルド / mac）。
    return `
      <div class="update-banner">
        <span class="update-banner-text">新しいバージョン ${ver} があります（手動更新）</span>
        <span class="update-banner-actions">
          <a class="btn btn-ink btn-sm" href="https://github.com/zytakeshi/amo-koteihyo/releases/latest" target="_blank" rel="noopener">ダウンロード</a>
          <button class="btn btn-ghost btn-sm" data-action="update-dismiss" type="button">あとで</button>
        </span>
      </div>`;
  }

  // 更新中フルスクリーン（§B3 step 5）。bootstrap.version==newVersion まで待つ。
  function renderUpdatingScreen() {
    if (state.updateTimedOut) {
      return `
        <div class="update-screen">
          <div class="update-screen-box">
            <div class="update-screen-title">更新の確認に時間がかかっています</div>
            <div class="update-screen-msg">PCの黒い画面（コンソール）を確認してください。<br>問題がなければ下のボタンで再読み込みしてください。</div>
            <button class="btn btn-rose" data-action="update-reload" type="button">再読み込み</button>
          </div>
        </div>`;
    }
    return `
      <div class="update-screen">
        <div class="update-screen-box">
          <div class="update-screen-spinner" aria-hidden="true"></div>
          <div class="update-screen-title">更新中… そのままお待ちください</div>
          <div class="update-screen-msg">新しいバージョン ${escapeHtml(state.updateTarget || "")} に更新しています。<br>アプリが自動で再起動します（約10秒）。</div>
        </div>
      </div>`;
  }

  function renderCalendarView() {
    const base = parseDate(state.calMonth || state.today);
    const year = base.getFullYear();
    const month = base.getMonth() + 1; // 1-12
    const firstIso = isoFrom(year, month, 1);
    const lead = weekdayIndex(firstIso); // Mon-first leading blanks
    const daysInMonth = new Date(year, month, 0).getDate();

    const weekStart = state.weekStart;
    const weekEnd = addDays(weekStart, 6);

    const cells = [];
    // leading days (previous month)
    for (let i = 0; i < lead; i++) {
      const iso = addDays(firstIso, i - lead);
      cells.push(calCell(iso, true, weekStart, weekEnd));
    }
    for (let d = 1; d <= daysInMonth; d++) {
      const iso = isoFrom(year, month, d);
      cells.push(calCell(iso, false, weekStart, weekEnd));
    }
    // trailing to complete the last row
    while (cells.length % 7 !== 0) {
      const lastIso = isoFrom(year, month, daysInMonth);
      const iso = addDays(lastIso, cells.length - (lead + daysInMonth) + 1);
      cells.push(calCell(iso, true, weekStart, weekEnd));
    }

    const weekdayHeads = WEEKDAYS.map((wd) => {
      const cls = wd === SUN_WD ? "is-sun" : wd === SAT_WD ? "is-sat" : "";
      return `<div class="cal-weekday ${cls}">${wd}</div>`;
    }).join("");

    return `
      ${renderTopbar()}
      <main>
        ${renderUpdateBanner()}
        <section class="panel">
          <div class="cal-nav">
            <button class="btn btn-ghost" data-action="cal-prev" type="button" aria-label="前の月">◀ 前月</button>
            <div class="cal-title">${year}年 ${month}月</div>
            <button class="btn btn-ghost" data-action="cal-next" type="button" aria-label="次の月">次月 ▶</button>
          </div>
          <button class="btn btn-soft btn-sm" data-action="cal-today" type="button">今日へ</button>
          <div class="cal-grid" style="margin-top:12px;">
            ${weekdayHeads}
            ${cells.join("")}
          </div>
          <div class="legend-row">
            <span><i style="background:var(--rose)"></i> 入力あり</span>
            <span><i style="background:var(--brass)"></i> 履歴あり</span>
            <span><i style="background:var(--rose);box-shadow:0 0 0 2px var(--rose)"></i> 今日</span>
            <span style="color:var(--rose-deep)">日付をタップ → その週の入力へ</span>
          </div>
        </section>
      </main>
    `;
  }

  function calCell(iso, other, weekStart, weekEnd) {
    const wd = wdOf(iso);
    const d = parseDate(iso).getDate();
    const isToday = iso === state.today;
    const inWeek = iso >= weekStart && iso <= weekEnd;
    const wdCls = wd === SUN_WD ? "is-sun" : wd === SAT_WD ? "is-sat" : "";
    const marks = state.monthMarks[iso] || {};
    const dots = [];
    if (marks.input) dots.push('<span class="cal-dot"></span>');
    if (marks.hist) dots.push('<span class="cal-dot hist"></span>');
    return `
      <button
        class="cal-cell ${other ? "is-other" : ""} ${isToday ? "is-today" : ""} ${inWeek ? "in-week" : ""} ${wdCls}"
        data-action="pick-date" data-date="${iso}" type="button">
        <span class="day-num">${d}</span>
        <span class="cal-dots">${dots.join("")}</span>
      </button>
    `;
  }

  /* ============================================================
   * View: Grid (input / main)
   * ========================================================== */
  function renderGridView() {
    const week = state.week;
    const dates = (week && week.dates) || [];
    const procs = activeProcesses();
    const weekStart = state.weekStart;
    const weekEnd = addDays(weekStart, 6);

    // ---- Front-end totals from /api/week counts ----
    const priceById = {};
    procs.forEach((p) => (priceById[p.id] = Number(p.price) || 0));

    const counts = (week && week.counts) || {};
    const dayCount = [0, 0, 0, 0, 0, 0, 0];
    const daySales = [0, 0, 0, 0, 0, 0, 0];
    let grandCount = 0;
    let grandSales = 0;

    const rowsHtml = procs
      .map((p) => {
        const arr = counts[p.id] || [0, 0, 0, 0, 0, 0, 0];
        let rowCount = 0;
        const cells = dates
          .map((iso, i) => {
            const c = Number(arr[i]) || 0;
            rowCount += c;
            dayCount[i] += c;
            daySales[i] += c * priceById[p.id];
            return renderCountCell(p.id, iso, c);
          })
          .join("");
        const rowSales = rowCount * priceById[p.id];
        grandCount += rowCount;
        grandSales += rowSales;
        return `
          <tr>
            <th class="col-process" scope="row">
              <strong>${escapeHtml(p.name)}</strong>
              <small>${yen.format(priceById[p.id])}</small>
            </th>
            ${cells}
            <td class="col-total row-total">
              <strong>${rowCount}件</strong>
              <small>${yen.format(rowSales)}</small>
            </td>
          </tr>
        `;
      })
      .join("");

    const headCells = dates
      .map((iso, i) => {
        const wd = wdOf(iso);
        const wdCls = wd === SUN_WD ? "is-sun" : wd === SAT_WD ? "is-sat" : "";
        const sel = i === state.memoDayIdx ? "is-selected" : "";
        return `<th class="day-head ${wdCls} ${sel}"><span class="dh-wd">${wd}</span><span class="dh-date">${shortMD(iso)}</span></th>`;
      })
      .join("");

    const dayTotalCells = dates
      .map((iso, i) => {
        void iso;
        return `<td><span class="dt-count">${dayCount[i]}件</span><span class="dt-sales">${yen.format(daySales[i])}</span></td>`;
      })
      .join("");

    const procEmpty = procs.length === 0;
    const staffEmpty = activeStaff().length === 0;

    const gridBody = staffEmpty
      ? `<div class="empty-state">スタッフがいません。<br>「設定」からスタッフを追加してください。</div>`
      : procEmpty
        ? `<div class="empty-state">工程がありません。<br>「設定」から工程を追加してください。</div>`
        : `
          <div class="grid-scroll">
            <table class="koteihyo-grid">
              <thead>
                <tr>
                  <th class="col-process">工程 / 単価</th>
                  ${headCells}
                  <th class="col-total">行合計</th>
                </tr>
              </thead>
              <tbody>
                ${rowsHtml}
                <tr class="day-total-row">
                  <th class="col-process" scope="row">日別合計</th>
                  ${dayTotalCells}
                  <td class="col-total"><span class="dt-count">${grandCount}件</span><span class="dt-sales">${yen.format(grandSales)}</span></td>
                </tr>
              </tbody>
            </table>
          </div>
          ${renderMemoBlock(dates)}
        `;

    return `
      ${renderTopbar()}
      <main>
        <div class="action-bar">
          <div class="group">
            <button class="btn btn-ghost" data-action="nav" data-view="calendar" type="button" title="カレンダー画面へ戻る">← 戻る</button>
          </div>
          ${renderUndoRedo()}
        </div>

        <div class="week-nav">
          <button class="btn btn-ghost" data-action="week-prev" type="button" aria-label="前の週">◀ 前週</button>
          <div class="week-label">${shortMD(weekStart)}〜${shortMD(weekEnd)}</div>
          <button class="btn btn-ghost" data-action="week-next" type="button" aria-label="次の週">次週 ▶</button>
          <button class="btn btn-soft btn-sm" data-action="week-this" type="button">今週</button>
        </div>

        <div class="staff-tabs" role="tablist" aria-label="スタッフ切替">
          ${activeStaff()
            .map(
              (s) =>
                `<button class="staff-tab ${s.id === state.staffId ? "is-active" : ""}" data-action="pick-staff" data-staff-id="${s.id}" type="button" role="tab" aria-selected="${s.id === state.staffId}">${escapeHtml(s.name)}</button>`
            )
            .join("")}
        </div>

        <section class="panel">
          ${gridBody}
        </section>

        ${renderTotalsBar(grandCount, grandSales, procs, counts, priceById)}
      </main>
    `;
  }

  function renderCountCell(processId, iso, count) {
    const zero = count === 0;
    return `
      <td class="count-cell">
        <div class="stepper">
          <button class="step-btn minus" data-action="count" data-op="dec" data-process-id="${processId}" data-date="${iso}" type="button" aria-label="減らす" ${zero ? "disabled" : ""}>−</button>
          <span class="count-num ${zero ? "is-zero" : ""}" data-count-cell="${processId}|${iso}">${count}</span>
          <button class="step-btn plus" data-action="count" data-op="inc" data-process-id="${processId}" data-date="${iso}" type="button" aria-label="増やす">＋</button>
        </div>
      </td>
    `;
  }

  function renderMemoBlock(dates) {
    const memos = (state.week && state.week.memos) || [];
    const idx = state.memoDayIdx;
    const dayTabs = dates
      .map((iso, i) => {
        const has = memos[i] && String(memos[i]).trim() ? "has-memo" : "";
        const active = i === idx ? "is-active" : "";
        return `<button class="memo-day-tab ${has} ${active}" data-action="pick-memo-day" data-idx="${i}" type="button">${wdOf(iso)} ${shortMD(iso)}</button>`;
      })
      .join("");
    const currentMemo = memos[idx] || "";
    const currentDate = dates[idx] || "";
    return `
      <div class="memo-block">
        <label for="memo-text">備考<small>${currentStaffName()} ・ ${currentDate ? `${wdOf(currentDate)} ${shortMD(currentDate)}` : ""}</small></label>
        <div class="memo-day-tabs">${dayTabs}</div>
        <textarea id="memo-text" data-action="memo-input" data-date="${currentDate}" placeholder="この日の備考を入力（自動保存）">${escapeHtml(currentMemo)}</textarea>
      </div>
    `;
  }

  function renderTotalsBar(grandCount, grandSales, procs, counts, priceById) {
    // Current staff weekly total (= grand of this week's grid for the selected
    // staff): count = Σc, sales = Σ price×c.
    let staffCount = 0;
    let staffSales = 0;
    procs.forEach((p) => {
      const arr = counts[p.id] || [];
      const c = arr.reduce((a, b) => a + (Number(b) || 0), 0);
      staffCount += c;
      staffSales += c * priceById[p.id];
    });
    void grandCount;
    void grandSales;

    // Whole-shop grand total for the week (all staff), from /api/totals.
    // Fall back to the current staff's total until totals are fetched so the
    // card never renders a misleadingly-low value (it can only ever be ≥ staff).
    const wt = state.weekTotals && state.weekTotals.grand ? state.weekTotals.grand : null;
    const grandKnown = !!wt;
    const allCount = grandKnown ? Number(wt.count) || 0 : staffCount;
    const allSales = grandKnown ? Number(wt.sales) || 0 : staffSales;
    return `
      <div class="totals-bar">
        <div class="total-card">
          <span class="tc-label">${escapeHtml(currentStaffName())} 週合計</span>
          <span class="tc-count">${staffCount}<small style="font-size:14px"> 件</small></span>
          <span class="tc-sales">${yen.format(staffSales)}</span>
        </div>
        <div class="total-card is-grand">
          <span class="tc-label">全体（全スタッフ）週合計</span>
          <span class="tc-count">${allCount}<small style="font-size:14px"> 件</small></span>
          <span class="tc-sales">${yen.format(allSales)}</span>
        </div>
      </div>
      <p class="hint-text">※ 「全体」は全スタッフの合計です。任意期間の集計やCSV書き出しは「設定」をご利用ください。</p>
    `;
  }

  /* ============================================================
   * View: History
   * ========================================================== */
  function renderHistoryView() {
    const days = (state.history && state.history.days) || [];
    const body = days.length
      ? days.map(renderHistoryDay).join("")
      : `<div class="empty-state">該当する履歴がありません。</div>`;

    return `
      ${renderTopbar()}
      <main>
        <div class="action-bar">
          <div class="group">
            <button class="btn btn-ghost" data-action="nav" data-view="calendar" type="button" title="カレンダー画面へ戻る">← 戻る</button>
          </div>
          ${renderUndoRedo()}
        </div>

        <section class="panel">
          <div class="section-heading">
            <h2>操作履歴</h2>
          </div>
          <div class="history-filter">
            <label for="hist-date" style="font-weight:900">日付で絞り込み</label>
            <input id="hist-date" type="date" value="${escapeHtml(state.historyFilter)}" data-action="history-filter" />
            <button class="btn btn-soft btn-sm" data-action="history-clear" type="button">解除</button>
          </div>
          ${body}
        </section>
      </main>
    `;
  }

  function renderHistoryDay(day) {
    const events = day.events || [];
    const list = events.map(renderHistoryEvent).join("");
    return `
      <div class="history-day">
        <h3>${escapeHtml(day.date)} <span>${wdOf(day.date)}曜・${events.length}件</span></h3>
        <div class="history-list">${list}</div>
      </div>
    `;
  }

  function renderHistoryEvent(ev) {
    const time = (ev.ts || "").slice(11, 16);
    const kind = ev.kind || "";
    const isMeta = ["undo", "redo", "revert"].includes(kind);
    const kindTag = isMeta
      ? `<span class="he-kind-tag">${kind === "undo" ? "取消" : kind === "redo" ? "やり直し" : "復元"}</span>`
      : "";
    const staff = ev.staffName ? `<small>${escapeHtml(ev.staffName)}</small>` : "";
    // Only real operations can be a revert target.
    const canRevert = ev.id && !isMeta;
    const revertBtn = canRevert
      ? `<button class="btn btn-ghost btn-sm" data-action="revert" data-event-id="${escapeHtml(ev.id)}" type="button">ここまで戻す</button>`
      : `<span></span>`;
    return `
      <div class="history-event kind-${escapeHtml(kind)}">
        <span class="he-time">${escapeHtml(time)}</span>
        <span class="he-label">
          <strong>${escapeHtml(ev.label || kind)}${kindTag}</strong>
          ${staff}
        </span>
        ${revertBtn}
      </div>
    `;
  }

  /* ============================================================
   * View: Settings
   * ========================================================== */
  function renderSettingsView() {
    const procs = state.processes.filter((p) => p.active !== false);
    const staff = state.staff.filter((s) => s.active !== false);

    const procRows = procs
      .map((p, i) => {
        const first = i === 0;
        const last = i === procs.length - 1;
        return `
          <div class="settings-row" data-process-id="${p.id}">
            <div class="reorder-controls">
              <button data-action="proc-up" data-id="${p.id}" type="button" aria-label="上へ" ${first ? "disabled" : ""}>▲</button>
              <button data-action="proc-down" data-id="${p.id}" type="button" aria-label="下へ" ${last ? "disabled" : ""}>▼</button>
            </div>
            <input class="name-input" type="text" value="${escapeHtml(p.name)}" data-action="proc-name" data-id="${p.id}" aria-label="工程名" />
            <input class="price-input" type="number" inputmode="numeric" min="0" step="50" value="${Number(p.price) || 0}" data-action="proc-price" data-id="${p.id}" aria-label="単価" />
            <div class="row-actions">
              <button class="btn btn-danger btn-sm" data-action="proc-delete" data-id="${p.id}" data-name="${escapeHtml(p.name)}" type="button">削除</button>
            </div>
          </div>
        `;
      })
      .join("");

    const staffRows = staff
      .map((s, i) => {
        const first = i === 0;
        const last = i === staff.length - 1;
        return `
          <div class="settings-row staff-row" data-staff-id="${s.id}">
            <div class="reorder-controls">
              <button data-action="staff-up" data-id="${s.id}" type="button" aria-label="上へ" ${first ? "disabled" : ""}>▲</button>
              <button data-action="staff-down" data-id="${s.id}" type="button" aria-label="下へ" ${last ? "disabled" : ""}>▼</button>
            </div>
            <input class="name-input" type="text" value="${escapeHtml(s.name)}" data-action="staff-name" data-id="${s.id}" aria-label="スタッフ名" />
            <div class="row-actions">
              <button class="btn btn-danger btn-sm" data-action="staff-delete" data-id="${s.id}" data-name="${escapeHtml(s.name)}" type="button">削除</button>
            </div>
          </div>
        `;
      })
      .join("");

    return `
      ${renderTopbar()}
      <main>
        <div class="action-bar">
          <div class="group">
            <button class="btn btn-ghost" data-action="nav" data-view="calendar" type="button" title="カレンダー画面へ戻る">← 戻る</button>
          </div>
          ${renderUndoRedo()}
        </div>

        <section class="panel">
          <div class="section-heading"><h2>工程の設定</h2></div>
          <div class="settings-list">
            ${procRows || `<div class="empty-state">工程がありません。</div>`}
          </div>
          <div class="add-form">
            <input class="name-input" type="text" id="new-proc-name" placeholder="工程名（例: シャンプー）" aria-label="新しい工程名" />
            <input class="price-input" type="number" inputmode="numeric" min="0" step="50" id="new-proc-price" placeholder="単価" aria-label="新しい工程の単価" />
            <button class="btn btn-rose" data-action="proc-add" type="button">＋ 工程を追加</button>
          </div>
        </section>

        <section class="panel">
          <div class="section-heading"><h2>スタッフの設定</h2></div>
          <div class="settings-list">
            ${staffRows || `<div class="empty-state">スタッフがいません。</div>`}
          </div>
          <div class="add-form">
            <input class="name-input" type="text" id="new-staff-name" placeholder="スタッフ名（例: 田中）" aria-label="新しいスタッフ名" />
            <button class="btn btn-rose" data-action="staff-add" type="button">＋ スタッフを追加</button>
          </div>
        </section>

        ${renderTimecardSettings()}

        <section class="panel">
          <div class="section-heading"><h2>書き出し・印刷</h2></div>
          <div class="export-range">
            <label>開始 <input type="date" value="${escapeHtml(state.exportStart)}" data-action="export-start" /></label>
            <label>終了 <input type="date" value="${escapeHtml(state.exportEnd)}" data-action="export-end" /></label>
          </div>
          <div class="export-actions">
            <button class="btn btn-ink" data-action="export-csv" type="button">CSV書き出し</button>
            <button class="btn btn-ghost" data-action="print" type="button">印刷</button>
          </div>
          <p class="hint-text">CSV は UTF-8（BOM付）。Excel でそのまま開けます。期間内の工程別・スタッフ別の集計を出力します。</p>
        </section>

        <p class="app-version">バージョン ${escapeHtml(state.version || "不明")}</p>
      </main>
    `;
  }

  // タイムカード設定パネル（設定画面内）。
  function renderTimecardSettings() {
    const cfg = state.tcConfig || { roundUnit: 1, roundDir: "floor", standardMinutes: 480 };
    const unitOpts = [
      [1, "なし"],
      [5, "5分"],
      [10, "10分"],
      [15, "15分"],
      [30, "30分"],
    ]
      .map(
        ([v, lbl]) =>
          `<option value="${v}" ${Number(cfg.roundUnit) === v ? "selected" : ""}>${lbl}</option>`
      )
      .join("");
    const dirOpts = [
      ["floor", "切り捨て"],
      ["nearest", "四捨五入"],
      ["ceil", "切り上げ"],
    ]
      .map(
        ([v, lbl]) =>
          `<option value="${v}" ${cfg.roundDir === v ? "selected" : ""}>${lbl}</option>`
      )
      .join("");
    const stdH = Math.floor((Number(cfg.standardMinutes) || 0) / 60);
    const stdM = (Number(cfg.standardMinutes) || 0) % 60;
    return `
      <section class="panel">
        <div class="section-heading"><h2>タイムカード設定</h2></div>
        <div class="tc-settings">
          <div class="tc-settings-row">
            <label for="tc-round-unit">丸め表示(参考)</label>
            <select id="tc-round-unit" data-action="tc-cfg-round-unit">${unitOpts}</select>
          </div>
          <div class="tc-settings-row">
            <label for="tc-round-dir">丸め方向</label>
            <select id="tc-round-dir" data-action="tc-cfg-round-dir">${dirOpts}</select>
          </div>
          <div class="tc-settings-row">
            <label>所定労働時間 /日</label>
            <div class="tc-std-inputs">
              <input type="number" inputmode="numeric" min="0" max="24" value="${stdH}" data-action="tc-cfg-std-hours" aria-label="所定労働時間（時）" /><span>時間</span>
              <input type="number" inputmode="numeric" min="0" max="59" value="${stdM}" data-action="tc-cfg-std-min" aria-label="所定労働時間（分）" /><span>分</span>
            </div>
          </div>
        </div>
        <p class="hint-text">※給与計算は常に実測（丸めなし）の値を使用します。丸め表示・所定超過は参考です。</p>
      </section>
    `;
  }

  /* ============================================================
   * View: Connect
   * ========================================================== */
  function renderConnectView() {
    return `
      ${renderTopbar()}
      <main>
        <div class="action-bar">
          <div class="group">
            <button class="btn btn-ghost" data-action="nav" data-view="calendar" type="button" title="カレンダー画面へ戻る">← 戻る</button>
          </div>
        </div>
        <section class="panel">
          <div class="section-heading"><h2>接続用URL</h2></div>
          <div class="connect-wrap">
            <div class="qr-box" id="qr-box" aria-label="接続用QRコード"></div>
            <div>
              <p class="eyebrow">iPad / iPhone で開く</p>
              <span class="connect-url">${escapeHtml(state.lanURL || "（取得中…）")}</span>
              <p class="hint-text">
                同じ Wi-Fi（LAN）につないだ iPad / iPhone / 他のPCで、上のURLを開くか、左のQRコードをカメラで読み取ってください。<br>
                このPCのアプリを起動したままにしておけば、どの端末からでも同じデータを編集できます。
              </p>
              ${
                state.lanURLs && state.lanURLs.filter((u) => u !== state.lanURL).length
                  ? `<p class="hint-text" style="margin-top:10px">
                      つながらない時は、こちらのアドレスも試してください：<br>
                      ${state.lanURLs
                        .filter((u) => u !== state.lanURL)
                        .map((u) => `<code>${escapeHtml(u)}</code>`)
                        .join("<br>")}
                    </p>`
                  : ""
              }
              <p class="hint-text" style="margin-top:10px">
                📱 スマホで開けない時は、PC側で Windows の「許可」が必要なことがあります。<br>
                その場合は同じフォルダの「ファイアウォール許可.bat」を<b>右クリック→「管理者として実行」</b>してください（初回だけ）。
              </p>
            </div>
          </div>
        </section>
      </main>
    `;
  }

  function drawQR() {
    const box = document.querySelector("#qr-box");
    if (!box) return;
    box.innerHTML = "";
    const url = state.lanURL;
    if (!url || typeof QRCode === "undefined") {
      box.innerHTML = `<span style="color:var(--muted);font-weight:900">QRを表示できません</span>`;
      return;
    }
    try {
      // eslint-disable-next-line no-new
      new QRCode(box, {
        text: url,
        width: 200,
        height: 200,
        correctLevel: QRCode.CorrectLevel.M,
      });
    } catch (e) {
      console.error(e);
      box.innerHTML = `<span style="color:var(--muted);font-weight:900">QRを表示できません</span>`;
    }
  }

  /* ============================================================
   * View: Timecard (打刻 / 月次)
   * ========================================================== */

  // "HH:MM" -> minutes since midnight.
  function hmToMin(hhmm) {
    const parts = String(hhmm || "").split(":");
    const h = Number(parts[0]) || 0;
    const m = Number(parts[1]) || 0;
    return h * 60 + m;
  }

  // minutes -> "H:MM" (null/undefined -> "—").
  function fmtMin(min) {
    if (min == null) return "—";
    const h = Math.floor(min / 60);
    const m = min % 60;
    return `${h}:${pad2(m)}`;
  }

  // Sum of closed breaks (both start+end set), in minutes.
  function breakMinutes(breaks) {
    let sum = 0;
    (breaks || []).forEach((b) => {
      if (b && b.start && b.end) sum += hmToMin(b.end) - hmToMin(b.start);
    });
    return sum;
  }

  function tcYMLabel(ym) {
    const p = String(ym || "").split("-");
    return p.length === 2 ? `${p[0]}年${Number(p[1])}月` : ym;
  }

  // "YYYY-MM" arithmetic (delta in months).
  function addYM(ym, delta) {
    const p = String(ym || "").split("-");
    let y = Number(p[0]) || 0;
    let m = (Number(p[1]) || 1) + delta;
    while (m < 1) {
      m += 12;
      y -= 1;
    }
    while (m > 12) {
      m -= 12;
      y += 1;
    }
    return `${y}-${pad2(m)}`;
  }

  /* ---- data loaders ---- */
  async function loadTCToday() {
    const data = await get("/api/timecard/today");
    state.tcToday = data;
    syncUndoFlags(data);
    if (data && data.serverNow) {
      const ms = new Date(data.serverNow).getTime();
      if (!isNaN(ms)) {
        state.tcClockAnchor = { serverMs: ms, perf: performance.now() };
      }
    }
  }

  function fetchTCMonth(sid, month) {
    const q = `?staffId=${encodeURIComponent(sid)}&month=${encodeURIComponent(month)}`;
    return get(`/api/timecard/month${q}`);
  }

  // Request-generation token so a slow response can't clobber a newer selection
  // (poll vs navigation vs staff switch), finding 1.
  let tcMonthReqSeq = 0;

  async function loadTCMonth() {
    if (!state.tcMonthYM) state.tcMonthYM = (state.today || "").slice(0, 7);
    // 選択が空のときだけ先頭の active スタッフを既定にする。空でなければサーバの
    // roster に検証を委ねる（退職者でも当月出勤があれば roster に載り選択可能、finding 2）。
    let sid = state.tcMonthStaffId;
    if (!sid) {
      const first = activeStaff()[0];
      sid = first ? first.id : "";
    }
    if (!sid) {
      state.tcMonth = null;
      state.tcMonthStaffId = "";
      return;
    }
    const myReq = ++tcMonthReqSeq;
    const month = state.tcMonthYM;
    let data = await fetchTCMonth(sid, month);
    if (myReq !== tcMonthReqSeq) return; // superseded by a newer load — drop it.
    // 選択が返却 roster に含まれない場合（例: 当月出勤の無い退職者）だけ先頭へ退避する。
    const roster = (data && data.roster) || [];
    if (roster.length && !roster.some((r) => r.staffId === sid)) {
      const fallback = roster.find((r) => !r.inactive) || roster[0];
      if (fallback && fallback.staffId !== sid) {
        sid = fallback.staffId;
        data = await fetchTCMonth(sid, month);
        if (myReq !== tcMonthReqSeq) return;
      }
    }
    state.tcMonth = data;
    state.tcMonthStaffId = sid;
    state.tcMonthYM = month;
    syncUndoFlags(data);
  }

  /* ---- server-anchored punch clock ---- */
  let tcClockTimer = null;

  function tickTCClock() {
    const el = document.querySelector("#tc-clock-time");
    if (!el || !state.tcClockAnchor) return;
    const elapsed = performance.now() - state.tcClockAnchor.perf;
    const now = new Date(state.tcClockAnchor.serverMs + elapsed);
    el.textContent = `${pad2(now.getHours())}:${pad2(now.getMinutes())}:${pad2(now.getSeconds())}`;
  }

  // Idempotent create/clear governed by the exact §A9 run-condition.
  function syncTCClock() {
    const shouldRun =
      state.view === "timecard" && state.timecardTab === "punch" && !document.hidden;
    if (shouldRun) {
      if (!tcClockTimer) tcClockTimer = setInterval(tickTCClock, 1000);
      tickTCClock();
    } else if (tcClockTimer) {
      clearInterval(tcClockTimer);
      tcClockTimer = null;
    }
  }

  /* ---- draft-protection guard ---- */
  // Central guard: all draft-destroying actions route through interceptForDraft.
  // While dirty, defer the action behind an inline 破棄 confirm bar.
  const DRAFT_DESTROYERS = new Set([
    "nav",
    "tc-subtab",
    "tc-roster",
    "tc-month-prev",
    "tc-month-next",
    "tc-month-this",
    "undo",
    "redo",
    "revert",
    "tc-edit-open",
  ]);

  function interceptForDraft(action, target) {
    if (!state.tcEditDirty) return false;
    if (!DRAFT_DESTROYERS.has(action)) return false;
    // Re-opening the SAME row's editor is not destructive.
    if (
      action === "tc-edit-open" &&
      state.tcEditDraft &&
      target.dataset.date === state.tcEditDraft.date
    ) {
      return false;
    }
    state.tcPending = () => dispatchClick(action, target);
    state.tcConfirmOpen = true;
    renderKeepingScroll();
    return true;
  }

  function discardDraftAndContinue() {
    const fn = state.tcPending; // capture before resetTCEdit clears tcPending
    resetTCEdit();
    if (fn) fn();
    else renderKeepingScroll();
  }

  function keepEditing() {
    state.tcConfirmOpen = false;
    state.tcPending = null;
    renderKeepingScroll();
  }

  /* ---- master view ---- */
  function renderTimecardView() {
    const sub = state.timecardTab === "month" ? renderTCMonth() : renderTCPunch();
    return `
      ${renderTopbar()}
      <main>
        <div class="action-bar">
          <div class="group">
            <button class="btn btn-ghost" data-action="nav" data-view="calendar" type="button" title="カレンダー画面へ戻る">← 戻る</button>
          </div>
          ${renderUndoRedo()}
        </div>

        <div class="tc-subtabs" role="tablist" aria-label="タイムカード切替">
          <button class="staff-tab ${state.timecardTab === "punch" ? "is-active" : ""}" data-action="tc-subtab" data-tab="punch" type="button" role="tab" aria-selected="${state.timecardTab === "punch"}">打刻</button>
          <button class="staff-tab ${state.timecardTab === "month" ? "is-active" : ""}" data-action="tc-subtab" data-tab="month" type="button" role="tab" aria-selected="${state.timecardTab === "month"}">月次</button>
        </div>

        ${sub}
      </main>
    `;
  }

  /* ---- 打刻 screen ---- */
  const TC_STATE_META = {
    off: { badge: "未出勤", cls: "is-off" },
    working: { badge: "勤務中", cls: "is-working" },
    break: { badge: "休憩中", cls: "is-break" },
    done: { badge: "退勤済", cls: "is-done" },
    carryover: { badge: "勤務中(前日)", cls: "is-carry" },
    carryover_break: { badge: "休憩中(前日)", cls: "is-carry" },
    invalid: { badge: "要確認", cls: "is-invalid" },
  };

  function tcBadgeText(st) {
    const lastBreak = (st.breaks || []).slice(-1)[0];
    switch (st.state) {
      case "working":
        return `勤務中 ${st.in || ""}〜`;
      case "break":
        return `休憩中 ${lastBreak && lastBreak.start ? lastBreak.start : ""}〜`;
      case "done":
        return `退勤済 ${st.out || ""}`;
      case "carryover":
        return "勤務中(前日)";
      case "carryover_break":
        return "休憩中(前日)";
      case "invalid":
        return "要確認";
      default:
        return "未出勤";
    }
  }

  function tcPunchSummary(st) {
    const parts = [];
    if (st.in) parts.push(`出 ${st.in}`);
    const closed = (st.breaks || []).filter((b) => b.start && b.end).length;
    if (closed) parts.push(`休憩${closed}回`);
    if (st.out) parts.push(`退 ${st.out}`);
    return parts.join("　");
  }

  function tcPunchButtons(st) {
    const btn = (action, label, cls) =>
      `<button class="btn ${cls} tc-punch-btn" data-action="tc-punch" data-staff-id="${escapeHtml(st.staffId)}" data-punch="${action}" type="button">${label}</button>`;
    switch (st.state) {
      case "off":
        return btn("in", "出勤", "btn-rose");
      case "working":
        return btn("break_start", "休憩開始", "btn-soft") + btn("out", "退勤", "btn-ink");
      case "break":
        return btn("break_end", "休憩終了", "btn-soft");
      case "carryover":
        return btn("out", "退勤（前日分）", "btn-ink");
      default:
        return "";
    }
  }

  function tcLockedNote(st) {
    if (st.state === "carryover_break") {
      return `<p class="tc-locked">前日の休憩が終了していません。月次画面の打刻修正で直してください。</p>`;
    }
    if (st.state === "invalid") {
      return `<p class="tc-locked">打刻データに不整合があります。月次画面の打刻修正で確認してください。</p>`;
    }
    if (st.state === "done") {
      return `<p class="tc-locked">本日は退勤済みです。（再出勤は月次の打刻修正で）</p>`;
    }
    return "";
  }

  function tcAnomalyText(a) {
    const name = a.staffName || staffName(a.staffId) || "";
    const md = shortMD(a.date);
    switch (a.kind) {
      case "退勤忘れ":
        return `⚠ ${md} ${name}さんの退勤打刻がありません`;
      case "休憩閉じ忘れ":
        return `⚠ ${md} ${name}さんの休憩が終了していません`;
      case "midnight_clamped":
        return `⚠ ${md} ${name}さんの退勤が日をまたいだ可能性があります（要確認）`;
      case "clock_warp":
        return `⚠ ${md} ${name}さんの打刻時刻を自動補正しました（要確認）`;
      case "invalid":
        return `⚠ ${md} ${name}さんの打刻データに不整合があります（打刻修正で確認）`;
      default:
        return `⚠ ${md} ${name}さん（${escapeHtml(a.kind)}）`;
    }
  }

  function renderTCPunch() {
    const tc = state.tcToday;
    if (!tc) {
      return `<section class="panel"><div class="empty-state">読み込み中…</div></section>`;
    }
    const dateLabel = tc.date
      ? `${parseDate(tc.date).getMonth() + 1}月${parseDate(tc.date).getDate()}日(${wdOf(tc.date)})`
      : "";

    const anomalies = tc.anomalies || [];
    const banner = anomalies.length
      ? `<div class="tc-anomaly">${anomalies.map((a) => `<span>${escapeHtml(tcAnomalyText(a))}</span>`).join("")}</div>`
      : "";

    const staff = tc.staff || [];
    const cards = staff.length
      ? staff
          .map((st) => {
            const meta = TC_STATE_META[st.state] || TC_STATE_META.off;
            const buttons = tcPunchButtons(st);
            const locked = tcLockedNote(st);
            const summary = tcPunchSummary(st);
            return `
              <div class="tc-card">
                <div class="tc-card-head">
                  <strong class="tc-name">${escapeHtml(st.name)}</strong>
                  <span class="tc-badge ${meta.cls}">${escapeHtml(tcBadgeText(st))}</span>
                </div>
                ${summary ? `<div class="tc-summary">${escapeHtml(summary)}</div>` : ""}
                ${buttons ? `<div class="tc-punch-actions">${buttons}</div>` : ""}
                ${locked}
              </div>
            `;
          })
          .join("")
      : `<div class="empty-state">スタッフがいません。<br>「設定」からスタッフを追加してください。</div>`;

    return `
      <section class="panel">
        <div class="tc-clock">
          <span class="tc-clock-date">${escapeHtml(dateLabel)}</span>
          <span class="tc-clock-time" id="tc-clock-time">--:--:--</span>
        </div>
        ${banner}
        <div class="tc-cards">
          ${cards}
        </div>
      </section>
    `;
  }

  /* ---- 月次 screen ---- */
  function renderTCMonth() {
    const roster = (state.tcMonth && state.tcMonth.roster) || [];
    const staffEmpty = activeStaff().length === 0 && roster.length === 0;
    if (staffEmpty) {
      return `<section class="panel"><div class="empty-state">スタッフがいません。<br>「設定」からスタッフを追加してください。</div></section>`;
    }

    const rosterTabs = roster
      .map((r) => {
        const active = r.staffId === state.tcMonthStaffId ? "is-active" : "";
        const badge = r.inactive ? `<span class="tc-inactive-badge">退職/無効</span>` : "";
        return `<button class="staff-tab ${active}" data-action="tc-roster" data-staff-id="${escapeHtml(r.staffId)}" type="button" role="tab" aria-selected="${r.staffId === state.tcMonthStaffId}">${escapeHtml(r.name)}${badge}</button>`;
      })
      .join("");

    const confirmBar = state.tcConfirmOpen
      ? `<div class="tc-confirm">編集中の内容を破棄しますか？
          <button class="btn btn-danger btn-sm" data-action="tc-discard" type="button">破棄する</button>
          <button class="btn btn-ghost btn-sm" data-action="tc-keep-editing" type="button">編集を続ける</button>
        </div>`
      : "";

    return `
      <div class="staff-tabs" role="tablist" aria-label="スタッフ切替">${rosterTabs || `<span class="tc-locked">対象スタッフがいません</span>`}</div>
      ${confirmBar}
      <div class="week-nav">
        <button class="btn btn-ghost" data-action="tc-month-prev" type="button" aria-label="前の月">◀ 前月</button>
        <div class="week-label">${escapeHtml(tcYMLabel(state.tcMonthYM))}</div>
        <button class="btn btn-ghost" data-action="tc-month-next" type="button" aria-label="次の月">次月 ▶</button>
        <button class="btn btn-soft btn-sm" data-action="tc-month-this" type="button">今月</button>
        <button class="btn btn-ink btn-sm" data-action="tc-csv" type="button">CSV</button>
        <button class="btn btn-ghost btn-sm" data-action="tc-print" type="button">印刷</button>
      </div>
      <section class="panel tc-month-panel">
        ${renderTCMonthTable()}
      </section>
    `;
  }

  function renderTCMonthTable() {
    const mo = state.tcMonth;
    if (!mo || !state.tcMonthStaffId) {
      return `<div class="empty-state">対象スタッフを選択してください。</div>`;
    }
    const days = mo.days || [];
    const cfg = mo.config || state.tcConfig || { roundUnit: 1 };
    const showRef = cfg.roundUnit !== 1;
    const hasAny = days.some((d) => d.in || d.out || (d.breaks && d.breaks.length) || d.attendanceNote);

    const rows = days
      .map((d) => renderTCMonthRow(d, showRef))
      .join("");

    const totals = mo.totals || { days: 0, workedRaw: 0, overStd: 0 };
    const refFoot =
      showRef && totals.workedRef != null ? `　実働(丸め) ${fmtMin(totals.workedRef)}` : "";
    const footer = `
      <div class="tc-month-footer">
        <span>出勤 ${totals.days}日</span>
        <span>実働 ${fmtMin(totals.workedRaw)}${refFoot}</span>
        <span>超過(参考) ${fmtMin(totals.overStd)}</span>
      </div>
    `;

    const table = `
      <div class="grid-scroll">
        <table class="tc-month-table">
          <thead>
            <tr>
              <th>日</th><th>曜</th><th>出勤</th><th>退勤</th><th>休憩</th><th>実働</th>${showRef ? "<th>実働(丸め・参考)</th>" : ""}<th>超過(参考)</th><th></th>
            </tr>
          </thead>
          <tbody>
            ${rows}
          </tbody>
        </table>
      </div>
      ${footer}
    `;

    if (!hasAny) {
      return `<div class="empty-state" style="margin-bottom:12px">この月の打刻はまだありません。<br>✎ から打刻を追加できます。</div>${table}`;
    }
    return table;
  }

  function renderTCMonthRow(d, showRef) {
    const wd = d.weekday || wdOf(d.date);
    const wdCls = wd === SUN_WD ? "is-sun" : wd === SAT_WD ? "is-sat" : "";
    const dayNum = Number(String(d.date).slice(8, 10)) || d.date;
    const flags = d.flags || [];
    // フラグ由来 or 構造的に不整合な事実（invalid）を ⚠ で顕在化する（finding 4）。
    const flagWarn =
      flags.length || d.invalid ? ' <span class="tc-warn" title="要確認">⚠</span>' : "";
    // Past incomplete day with 出勤 = 退勤/休憩忘れ.
    const pastIncomplete = d.in && !d.complete && d.date < state.today;
    const outCell = d.out
      ? escapeHtml(d.out)
      : pastIncomplete
        ? `<span class="tc-warn">⚠</span>—`
        : "—";
    const inCell = d.in ? escapeHtml(d.in) : "—";
    const brk = breakMinutes(d.breaks);
    const brkCell = brk ? `${brk}分` : "—";
    const workedCell = fmtMin(d.workedRaw);
    const workedRefCell = showRef ? `<td>${fmtMin(d.workedRef)}</td>` : "";
    const overCell = fmtMin(d.overStd);

    // Render the inline editor only when the draft is pinned to THIS staff/month
    // AND this row's date (round-3 P1).
    const editing =
      draftMatchesMonth(state.tcEditDraft, state.tcMonth) && state.tcEditDraft.date === d.date;
    const rowCls = editing ? "is-editing" : "";
    const mainRow = `
      <tr class="${rowCls} ${wdCls}">
        <td>${dayNum}${flagWarn}</td>
        <td class="${wdCls}">${wd}</td>
        <td>${inCell}</td>
        <td>${outCell}</td>
        <td>${brkCell}</td>
        <td>${workedCell}</td>
        ${workedRefCell}
        <td>${overCell}</td>
        <td><button class="tc-edit-btn" data-action="tc-edit-open" data-date="${d.date}" type="button" aria-label="修正">✎</button></td>
      </tr>
    `;
    if (!editing) return mainRow;
    return mainRow + renderTCEditorRow(showRef);
  }

  function renderTCEditorRow(showRef) {
    const d = state.tcEditDraft;
    // 記録のある日だけ削除を出す。開いた時点のサーバ行に打刻があったか（d.hadRecord）で
    // 判定する（可変ドラフトや、過去の削除で >0 になった rev では誤判定するため、codex #3）。
    // 空の日のエディタは保存/取消のみ（全空 POST は no-op で誤解を招くため出さない）。
    const hasRecord = !!d.hadRecord;
    // 削除は破線ガード（tcConfirmOpen）と同じ「ページ内 confirm」方式（window.confirm 不使用）。
    const deleteControl = !hasRecord
      ? ""
      : state.tcDeleteConfirmOpen
        ? `<div class="tc-delete-confirm">この日の打刻を本当に消しますか？
            <button class="btn btn-danger btn-sm" data-action="tc-delete-confirm" type="button">消す</button>
            <button class="btn btn-ghost btn-sm" data-action="tc-delete-cancel" type="button">やめる</button>
          </div>`
        : `<button class="btn btn-danger" data-action="tc-delete-open" type="button">この日の打刻を全部消す</button>`;
    const actions = deleteControl
      ? `<div class="tc-edit-actions tc-edit-actions--split">
              <div class="tc-edit-delete">${deleteControl}</div>
              <div class="tc-edit-actions-main">
                <button class="btn btn-ink" data-action="tc-edit-save" type="button">保存</button>
                <button class="btn btn-ghost" data-action="tc-edit-cancel" type="button">取消</button>
              </div>
            </div>`
      : `<div class="tc-edit-actions">
              <button class="btn btn-ink" data-action="tc-edit-save" type="button">保存</button>
              <button class="btn btn-ghost" data-action="tc-edit-cancel" type="button">取消</button>
            </div>`;
    const breakRows = d.breaks
      .map(
        (b, i) => `
        <div class="tc-break-row">
          <label>休憩 開始 <input type="time" value="${escapeHtml(b.start)}" data-action="tc-edit-break-start" data-idx="${i}" /></label>
          <label>終了 <input type="time" value="${escapeHtml(b.end)}" data-action="tc-edit-break-end" data-idx="${i}" /></label>
          <button class="btn btn-danger btn-sm" data-action="tc-break-del" data-idx="${i}" type="button">削除</button>
        </div>
      `
      )
      .join("");
    return `
      <tr class="tc-edit-row">
        <td colspan="${showRef ? 9 : 8}">
          <div class="tc-editor">
            <div class="tc-edit-times">
              <label>出勤 <input type="time" value="${escapeHtml(d.in)}" data-action="tc-edit-in" /></label>
              <label>退勤 <input type="time" value="${escapeHtml(d.out)}" data-action="tc-edit-out" /></label>
            </div>
            <div class="tc-breaks">
              ${breakRows || `<p class="hint-text" style="margin:0">休憩なし</p>`}
              <button class="btn btn-soft btn-sm" data-action="tc-break-add" type="button">＋ 休憩を追加</button>
            </div>
            <label class="tc-note-label">勤怠メモ
              <textarea data-action="tc-edit-note" placeholder="遅刻・早退などのメモ（任意）">${escapeHtml(d.attendanceNote)}</textarea>
            </label>
            ${actions}
          </div>
        </td>
      </tr>
    `;
  }

  /* ---- timecard actions ---- */
  async function doTCPunch(staffId, action) {
    try {
      const resp = await post("/api/timecard/punch", { staffId, action });
      syncUndoFlags(resp);
      const labelMap = { in: "出勤", break_start: "休憩開始", break_end: "休憩終了", out: "退勤" };
      const day = resp && resp.day ? resp.day : {};
      let t = "";
      if (action === "in") t = day.in || "";
      else if (action === "out") t = day.out || "";
      else if (day.breaks && day.breaks.length) {
        const lb = day.breaks[day.breaks.length - 1];
        t = action === "break_start" ? lb.start || "" : lb.end || "";
      }
      toast(`${staffName(staffId)}さん ${labelMap[action] || ""} ${t}`.trim());
      await safe(loadTCToday);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  // Central editor-teardown: clears the draft AND every piece of confirm/dirty
  // state so a stale 破棄 / delete confirm can never survive an editor exit
  // (nav, sub-tab, staff/month switch, cancel, save, discard) — codex MED #2.
  function resetTCEdit() {
    state.tcEditDraft = null;
    state.tcEditDirty = false;
    state.tcConfirmOpen = false;
    state.tcPending = null;
    state.tcDeleteConfirmOpen = false;
  }

  // Whether a SERVER row actually holds punch data. Pinned once at editor open
  // (into d.hadRecord) so the delete button reflects the record-at-open, not the
  // live (possibly all-cleared) draft or a rev bumped by a prior delete — codex #3.
  function rowHasRecord(r) {
    return !!(
      r &&
      (r.in || r.out || (Array.isArray(r.breaks) && r.breaks.length > 0) || r.attendanceNote)
    );
  }

  function openTCEdit(date) {
    const mo = state.tcMonth;
    if (!mo) return;
    const row = (mo.days || []).find((d) => d.date === date);
    if (!row) return;
    state.tcEditDraft = {
      // Pin the editing context so a save always posts to the right staff/month
      // even if a background poll changes the selection (finding 1).
      staffId: state.tcMonthStaffId,
      month: state.tcMonthYM,
      date,
      in: row.in || "",
      out: row.out || "",
      breaks: (row.breaks || []).map((b) => ({ start: b.start || "", end: b.end || "" })),
      attendanceNote: row.attendanceNote || "",
      expectedRev: row.rev,
      hadRecord: rowHasRecord(row), // record-at-open (codex #3)
    };
    state.tcEditDirty = false;
    state.tcDeleteConfirmOpen = false; // fresh editor never opens mid-confirm
    renderKeepingScroll();
  }

  // True when the pinned draft context matches the currently-loaded month view
  // (same staff + same month) — a draft must never render or save across a
  // switched selection (round-3 P1).
  function draftMatchesMonth(d, mo) {
    return !!d && !!mo && d.staffId === mo.staffId && d.month === mo.month;
  }

  // After an authoritative month refresh (undo/redo/revert/poll), a CLEAN open
  // editor still holds a stale rev/values — rebuild it from the refreshed row, or
  // close it if the row is gone. Dirty drafts are protected by the existing guard
  // and left untouched (finding 8). If the loaded month no longer matches the
  // draft's pinned staff/month, close the editor rather than rebind it (round-3 P1).
  function reconcileCleanDraft() {
    if (!state.tcEditDraft || state.tcEditDirty) return;
    const mo = state.tcMonth;
    if (!draftMatchesMonth(state.tcEditDraft, mo)) {
      resetTCEdit(); // loaded a different staff/month — close it.
      return;
    }
    const row = mo && (mo.days || []).find((d) => d.date === state.tcEditDraft.date);
    if (!row) {
      resetTCEdit();
      return;
    }
    // A background poll/undo that bumped this day's rev must invalidate an open
    // delete confirm — otherwise 消す would delete the NEWER snapshot (codex HIGH #1).
    if (row.rev !== state.tcEditDraft.expectedRev) state.tcDeleteConfirmOpen = false;
    state.tcEditDraft = {
      staffId: state.tcEditDraft.staffId, // preserve pinned context (finding 1)
      month: state.tcEditDraft.month,
      date: row.date,
      in: row.in || "",
      out: row.out || "",
      breaks: (row.breaks || []).map((b) => ({ start: b.start || "", end: b.end || "" })),
      attendanceNote: row.attendanceNote || "",
      expectedRev: row.rev,
      hadRecord: rowHasRecord(row), // re-pin from refreshed server row (codex #3)
    };
  }

  // Single-flight guard: a double-tap on 保存 must not fire two requests against
  // one rev (the second would self-409), finding 4.
  let tcEditSaving = false;

  function setTCSaveDisabled(disabled) {
    const btn = document.querySelector('[data-action="tc-edit-save"]');
    if (btn) btn.disabled = disabled;
  }

  // opts.deleteAll: post all fields empty against the pinned rev — the store's
  // all-empty-deletes contract (tc_set with a null snapshot; undoable). Routes
  // through the same single-flight / 409 / refresh path as a normal save.
  async function saveTCEdit(opts = {}) {
    const d = state.tcEditDraft;
    if (!d || tcEditSaving) return;
    const deleteAll = !!opts.deleteAll;
    // Hard-gate delete execution: a delete may only fire from an OPEN confirm on
    // the month editor whose draft still matches the loaded month. Blocks a delete
    // firing against a context that shifted (poll/nav) between confirm and 消す —
    // codex HIGH #1.
    if (
      deleteAll &&
      !(
        state.tcDeleteConfirmOpen &&
        state.view === "timecard" &&
        state.timecardTab === "month" &&
        draftMatchesMonth(d, state.tcMonth)
      )
    ) {
      state.tcDeleteConfirmOpen = false;
      renderKeepingScroll();
      return;
    }
    const payload = {
      // Post strictly to the pinned staff/date, never the (possibly poll-changed)
      // global selection — no fallback (round-3 P1).
      staffId: d.staffId,
      date: d.date,
      in: deleteAll ? "" : d.in,
      out: deleteAll ? "" : d.out,
      breaks: deleteAll ? [] : d.breaks.filter((b) => b.start || b.end),
      attendanceNote: deleteAll ? "" : d.attendanceNote,
      expectedRev: d.expectedRev,
    };
    tcEditSaving = true;
    setTCSaveDisabled(true);
    let res;
    try {
      res = await postRaw("/api/timecard/day", payload);
    } catch (e) {
      tcEditSaving = false;
      setTCSaveDisabled(false);
      reportError(e);
      return;
    }
    tcEditSaving = false;
    // Stale response: editing context changed while in flight — drop it (finding 4).
    if (state.tcEditDraft !== d) return;
    // Any resolved response closes the delete-confirm bar (success/409/400 all
    // re-render the editor from server truth).
    state.tcDeleteConfirmOpen = false;
    if (res.ok) {
      syncUndoFlags(res.body);
      resetTCEdit(); // full editor teardown on success (codex #2)
      toast(deleteAll ? "打刻を消しました（⟲で戻せます）" : "打刻を保存しました");
      await safe(loadTCMonth);
      renderKeepingScroll();
      return;
    }
    if (res.status === 409) {
      // 他端末が更新済み: 楽観ロック衝突。編集中の値は破棄し、サーバの currentDay で
      // ドラフトを作り直す（古い入力を新 rev に対して上書きさせない、finding 3）。
      toast("他の端末で更新されました。最新を確認してください", true);
      // Refresh the table for the DRAFT's pinned context (not the global
      // selection), and re-check the draft survived the await (round-3 P1).
      try {
        const fresh = await fetchTCMonth(d.staffId, d.month);
        if (state.tcEditDraft !== d) return; // context changed during reload — drop
        if (draftMatchesMonth(d, fresh)) {
          state.tcMonth = fresh;
          state.tcMonthStaffId = d.staffId;
          state.tcMonthYM = d.month;
          syncUndoFlags(fresh);
        }
      } catch (e) {
        if (state.tcEditDraft !== d) return;
      }
      const cur = res.body && res.body.currentDay;
      let newRev = res.body && typeof res.body.currentRev === "number" ? res.body.currentRev : null;
      if (newRev == null) {
        const row = state.tcMonth && (state.tcMonth.days || []).find((x) => x.date === d.date);
        newRev = row ? row.rev : d.expectedRev;
      }
      state.tcEditDraft = {
        staffId: d.staffId, // keep the pinned context (finding 1)
        month: d.month,
        date: d.date,
        in: cur ? cur.in || "" : "",
        out: cur ? cur.out || "" : "",
        breaks:
          cur && Array.isArray(cur.breaks)
            ? cur.breaks.map((b) => ({ start: b.start || "", end: b.end || "" }))
            : [],
        attendanceNote: cur ? cur.attendanceNote || "" : "",
        expectedRev: newRev,
        hadRecord: rowHasRecord(cur), // re-pin from server currentDay (codex #3)
      };
      // 最新値で作り直したので未変更状態。ユーザが改めて編集し直せる。
      // 確認バーは分岐前に閉じ済み（reopen clean without confirm、codex #2）。
      // 破棄確認バー/保留中ナビも消す（409で古い破棄バーが生き残らないように）。
      state.tcEditDirty = false;
      state.tcConfirmOpen = false;
      state.tcPending = null;
      renderKeepingScroll();
      return;
    }
    // Validation (400) / other: keep editing, surface the message.
    setTCSaveDisabled(false);
    toast((res.body && res.body.error) || `エラー (${res.status})`, true);
  }

  // Read a COMPLETE snapshot of the config form so a save never derives a partial
  // payload from stale state (finding 9).
  function readTCConfigForm() {
    const cur = state.tcConfig || { roundUnit: 1, roundDir: "floor", standardMinutes: 480 };
    const unitEl = document.querySelector("#tc-round-unit");
    const dirEl = document.querySelector("#tc-round-dir");
    const hEl = document.querySelector('[data-action="tc-cfg-std-hours"]');
    const mEl = document.querySelector('[data-action="tc-cfg-std-min"]');
    const roundUnit = unitEl ? Number(unitEl.value) || 1 : cur.roundUnit;
    const roundDir = dirEl ? dirEl.value : cur.roundDir;
    const h = hEl ? Number(hEl.value) || 0 : Math.floor((cur.standardMinutes || 0) / 60);
    const m = mEl ? Number(mEl.value) || 0 : (cur.standardMinutes || 0) % 60;
    return { roundUnit, roundDir, standardMinutes: Math.max(0, Math.min(1440, h * 60 + m)) };
  }

  function setTCConfigControlsDisabled(disabled) {
    document
      .querySelectorAll(
        '#tc-round-unit,#tc-round-dir,[data-action="tc-cfg-std-hours"],[data-action="tc-cfg-std-min"]'
      )
      .forEach((el) => {
        el.disabled = disabled;
      });
  }

  // Single-flight: disable the controls while a save is in flight so rapid changes
  // can't launch parallel requests that race (last-response-wins), finding 9.
  let tcConfigSaving = false;
  async function saveTCConfig() {
    if (tcConfigSaving) return;
    const next = readTCConfigForm();
    tcConfigSaving = true;
    setTCConfigControlsDisabled(true);
    try {
      const resp = await post("/api/timecard/config", next);
      syncUndoFlags(resp);
      if (resp && resp.config) state.tcConfig = resp.config;
      toast("タイムカード設定を保存しました");
      render(); // re-renders the settings panel with fresh (re-enabled) controls
    } catch (e) {
      reportError(e);
      setTCConfigControlsDisabled(false);
    } finally {
      tcConfigSaving = false;
    }
  }

  /* ============================================================
   * Master render
   * ========================================================== */
  function render() {
    // 更新中はフルスクリーンを最優先で表示する（他画面へは戻さない）。
    if (state.updating) {
      root.innerHTML = renderUpdatingScreen();
      return;
    }
    if (state.error) {
      root.innerHTML = `${renderTopbar()}<div class="empty-state" style="margin-top:24px">${escapeHtml(state.error)}</div>`;
      return;
    }
    if (!state.ready) {
      root.innerHTML = `<div class="loading">読み込み中…</div>`;
      return;
    }

    switch (state.view) {
      case "grid":
        root.innerHTML = renderGridView();
        break;
      case "history":
        root.innerHTML = renderHistoryView();
        break;
      case "settings":
        root.innerHTML = renderSettingsView();
        break;
      case "connect":
        root.innerHTML = renderConnectView();
        drawQR();
        break;
      case "timecard":
        root.innerHTML = renderTimecardView();
        break;
      case "calendar":
      default:
        root.innerHTML = renderCalendarView();
        break;
    }
    // Keep the punch clock's interval in sync with the freshly-rendered DOM.
    syncTCClock();
  }

  /* ============================================================
   * Scroll preservation (safety net for full render())
   * ----------------------------------------------------------
   * render() replaces #app.innerHTML, which resets the grid's horizontal
   * scroll (.grid-scroll.scrollLeft) AND the window's vertical scroll. Tapping
   * a Sat/Sun cell after scrolling right would otherwise jump back to Monday.
   * Capture before, restore after.
   * ========================================================== */
  function captureScroll() {
    const scroller = document.querySelector(".grid-scroll");
    return {
      left: scroller ? scroller.scrollLeft : 0,
      y: window.scrollY || window.pageYOffset || 0,
    };
  }

  function restoreScroll(snap) {
    if (!snap) return;
    const scroller = document.querySelector(".grid-scroll");
    if (scroller && snap.left) scroller.scrollLeft = snap.left;
    if (snap.y) window.scrollTo(0, snap.y);
  }

  // Full render() that preserves the grid/window scroll position.
  function renderKeepingScroll() {
    const snap = captureScroll();
    render();
    restoreScroll(snap);
  }

  /* ============================================================
   * In-place count update (avoid full re-render flicker)
   * ========================================================== */
  function applyCountLocally(processId, iso, value) {
    if (!state.week || !state.week.dates) return;
    const idx = state.week.dates.indexOf(iso);
    if (idx < 0) return;
    if (!state.week.counts[processId]) {
      state.week.counts[processId] = [0, 0, 0, 0, 0, 0, 0];
    }
    state.week.counts[processId][idx] = value;
  }

  /* ------------------------------------------------------------
   * In-place DOM patch for a single count change.
   * Updates only the changed cell's number + the affected totals
   * (row, day column, day-total grand, totals bar) without touching
   * #app.innerHTML, so scroll position and focus are never disturbed.
   * Returns true on success; false if the DOM/state isn't patchable
   * (caller should fall back to a scroll-preserving full render).
   * ---------------------------------------------------------- */
  function patchCountInPlace(processId, iso) {
    const week = state.week;
    if (!week || !Array.isArray(week.dates)) return false;
    const idx = week.dates.indexOf(iso);
    if (idx < 0) return false;

    const procs = activeProcesses();
    const priceById = {};
    procs.forEach((p) => (priceById[p.id] = Number(p.price) || 0));
    const counts = week.counts || {};

    // 1) The cell number + its minus-button disabled state.
    const arr = counts[processId] || [];
    const cellValue = Number(arr[idx]) || 0;
    const numEl = document.querySelector(`[data-count-cell="${cssAttr(processId)}|${cssAttr(iso)}"]`);
    if (!numEl) return false;
    numEl.textContent = String(cellValue);
    numEl.classList.toggle("is-zero", cellValue === 0);
    const cell = numEl.closest(".count-cell");
    if (cell) {
      const minus = cell.querySelector(".step-btn.minus");
      if (minus) minus.disabled = cellValue === 0;
    }

    // 2) Recompute row/day/grand totals from authoritative week counts.
    const dayCount = [0, 0, 0, 0, 0, 0, 0];
    const daySales = [0, 0, 0, 0, 0, 0, 0];
    const rowCount = {};
    const rowSales = {};
    let grandCount = 0;
    let grandSales = 0;
    procs.forEach((p) => {
      const row = counts[p.id] || [];
      let rc = 0;
      for (let i = 0; i < 7; i++) {
        const c = Number(row[i]) || 0;
        rc += c;
        dayCount[i] += c;
        daySales[i] += c * priceById[p.id];
      }
      rowCount[p.id] = rc;
      rowSales[p.id] = rc * priceById[p.id];
      grandCount += rc;
      grandSales += rowSales[p.id];
    });

    // Row total for the changed process.
    const rowTotalCell = numEl.closest("tr") ? numEl.closest("tr").querySelector(".row-total") : null;
    if (rowTotalCell) {
      const strong = rowTotalCell.querySelector("strong");
      const small = rowTotalCell.querySelector("small");
      if (strong) strong.textContent = `${rowCount[processId]}件`;
      if (small) small.textContent = yen.format(rowSales[processId]);
    }

    // Day column total for the changed day.
    const dayTotalRow = document.querySelector(".day-total-row");
    if (dayTotalRow) {
      const tds = dayTotalRow.querySelectorAll("td");
      // tds: [day0..day6, grand]
      const dayTd = tds[idx];
      if (dayTd) {
        const dc = dayTd.querySelector(".dt-count");
        const ds = dayTd.querySelector(".dt-sales");
        if (dc) dc.textContent = `${dayCount[idx]}件`;
        if (ds) ds.textContent = yen.format(daySales[idx]);
      }
      const grandTd = tds[tds.length - 1];
      if (grandTd) {
        const dc = grandTd.querySelector(".dt-count");
        const ds = grandTd.querySelector(".dt-sales");
        if (dc) dc.textContent = `${grandCount}件`;
        if (ds) ds.textContent = yen.format(grandSales);
      }
    }

    // 3) Totals bar: current-staff card (= this week's grid grand for the staff).
    const cards = document.querySelectorAll(".totals-bar .total-card");
    if (cards[0]) {
      const c = cards[0].querySelector(".tc-count");
      const s = cards[0].querySelector(".tc-sales");
      if (c) c.innerHTML = `${grandCount}<small style="font-size:14px"> 件</small>`;
      if (s) s.textContent = yen.format(grandSales);
    }
    // The "全体" card is refreshed from /api/totals (debounced) elsewhere.
    return true;
  }

  // Escape a value for use inside a CSS attribute selector.
  function cssAttr(value) {
    if (window.CSS && typeof CSS.escape === "function") return CSS.escape(value);
    return String(value).replace(/["\\\]]/g, "\\$&");
  }

  /* ============================================================
   * Action handlers
   * ========================================================== */
  async function doCount(processId, iso, op) {
    // Capture the staff/week this tap belongs to: an in-flight response must
    // not be written into a different staff's grid if the user switches.
    const reqStaffId = state.staffId;
    const reqWeekStart = state.weekStart;
    try {
      const resp = await post("/api/count", { staffId: reqStaffId, date: iso, processId, op });
      syncUndoFlags(resp);
      // Stale response (user switched staff/week mid-flight): keep undo/redo
      // flags but don't touch the now-unrelated grid.
      if (reqStaffId !== state.staffId || reqWeekStart !== state.weekStart) {
        updateUndoRedoButtons();
        return;
      }
      // Authoritative server value overwrites any optimistic display, so
      // double-taps/rapid taps self-heal to the true count.
      applyCountLocally(processId, iso, Number(resp.value) || 0);
      // In-place DOM patch keeps grid horizontal + window vertical scroll.
      // Fall back to a scroll-preserving full render if the cell isn't patchable.
      if (!patchCountInPlace(processId, iso)) {
        renderKeepingScroll();
      }
      updateUndoRedoButtons();
      // The whole-shop "全体" total is authoritative server-side; refresh it
      // debounced so a burst of ± taps results in one correct final value.
      scheduleWeekTotalsRefresh();
    } catch (e) {
      reportError(e);
    }
  }

  // Debounced refresh of the whole-shop grand total card after count changes.
  let weekTotalsTimer = null;
  function scheduleWeekTotalsRefresh() {
    if (weekTotalsTimer) clearTimeout(weekTotalsTimer);
    const reqStaffId = state.staffId;
    const reqWeekStart = state.weekStart;
    weekTotalsTimer = setTimeout(async () => {
      try {
        await loadWeekTotals();
      } catch (e) {
        // Non-fatal: the card keeps its previous value.
        console.warn("week totals refresh failed", e);
        return;
      }
      // Only patch the DOM if we're still on the same grid view/week.
      if (
        state.view === "grid" &&
        reqStaffId === state.staffId &&
        reqWeekStart === state.weekStart
      ) {
        patchGrandTotalCard();
      }
    }, 400);
  }

  // Update just the "全体" card from state.weekTotals without a full render.
  function patchGrandTotalCard() {
    const wt = state.weekTotals && state.weekTotals.grand ? state.weekTotals.grand : null;
    if (!wt) return;
    const cards = document.querySelectorAll(".totals-bar .total-card");
    const grandCard = cards[1];
    if (!grandCard) return;
    const c = grandCard.querySelector(".tc-count");
    const s = grandCard.querySelector(".tc-sales");
    if (c) c.innerHTML = `${Number(wt.count) || 0}<small style="font-size:14px"> 件</small>`;
    if (s) s.textContent = yen.format(Number(wt.sales) || 0);
  }

  // Build a human label like "浜田 6/8 シャンプー +1" from an undo/redo response.
  // Global history means the affected op may belong to another staff/day, so
  // we prefix the staff + bizDate to make clear exactly what was changed.
  function describeMutation(resp) {
    if (!resp) return "";
    const parts = [];
    const sName = resp.staffId ? staffName(resp.staffId) : "";
    if (sName) parts.push(sName);
    if (resp.bizDate) parts.push(shortMD(resp.bizDate));
    if (resp.label) parts.push(resp.label);
    return parts.join(" ").trim();
  }

  async function doUndo() {
    try {
      const resp = await post("/api/undo", {});
      syncUndoFlags(resp);
      if (resp && resp.applied === false) {
        toast("これ以上戻せません");
      } else {
        const desc = describeMutation(resp);
        toast(desc ? `『${desc}』を元に戻しました` : "元に戻しました");
      }
      await refreshAfterMutation(resp);
    } catch (e) {
      reportError(e);
    }
  }

  async function doRedo() {
    try {
      const resp = await post("/api/redo", {});
      syncUndoFlags(resp);
      if (resp && resp.applied === false) {
        toast("やり直す操作がありません");
      } else {
        const desc = describeMutation(resp);
        toast(desc ? `『${desc}』をやり直しました` : "やり直しました");
      }
      await refreshAfterMutation(resp);
    } catch (e) {
      reportError(e);
    }
  }

  async function doRevert(eventId) {
    try {
      const resp = await post("/api/history/revert", { eventId });
      syncUndoFlags(resp);
      const n = resp && typeof resp.revertedCount === "number" ? resp.revertedCount : 0;
      toast(`${n}件の操作を戻しました`);
      // Revert can touch any week/staff/day: refresh the current view's data
      // (grid week+grand / history / calendar marks) so nothing goes stale.
      await refreshAfterMutation(resp);
    } catch (e) {
      reportError(e);
    }
  }

  // Re-pull whatever the current view depends on after an undo/redo/revert.
  // (Contract: undo/redo/revert can affect any view's week/grand/marks/history.)
  // 工程/スタッフ構成だけを取り直す（週・選択スタッフ・日付は保持）。
  // undo/redo/revert は process_add/delete/rename/price/staff_* も巻き戻すため、
  // これを呼ばないと設定画面やタブが古い構成のままになる。
  async function loadConfigPreservingSelection() {
    const cur = state.staffId;
    const data = await get("/api/bootstrap");
    state.staff = Array.isArray(data.staff) ? data.staff : [];
    state.processes = Array.isArray(data.processes) ? data.processes : [];
    if (data.tcConfig) state.tcConfig = data.tcConfig;
    if (!activeStaff().some((s) => s.id === cur)) {
      const first = activeStaff()[0];
      state.staffId = first ? first.id : "";
    }
  }

  async function refreshAfterMutation() {
    // undo/redo/revert は構成変更も戻しうるので、まず工程/スタッフを取り直す。
    await safe(loadConfigPreservingSelection);
    if (state.view === "grid") {
      await loadGrid();
      renderKeepingScroll();
    } else if (state.view === "history") {
      await safe(loadHistory);
      render();
    } else if (state.view === "calendar") {
      await safe(loadMonthMarks);
      render();
    } else if (state.view === "timecard") {
      if (state.timecardTab === "month") {
        await safe(loadTCMonth);
        reconcileCleanDraft(); // clean editor: rebuild from refreshed row (finding 8)
        renderKeepingScroll();
      } else {
        await safe(loadTCToday);
        render();
      }
    } else {
      render();
    }
  }

  // ---- Settings: process ----
  async function procAdd() {
    const nameEl = document.querySelector("#new-proc-name");
    const priceEl = document.querySelector("#new-proc-price");
    const name = nameEl ? nameEl.value.trim() : "";
    const price = priceEl ? Number(priceEl.value) || 0 : 0;
    if (!name) {
      toast("工程名を入力してください", true);
      return;
    }
    try {
      const resp = await post("/api/process", { name, price });
      if (resp && resp.process) state.processes.push(resp.process);
      syncUndoFlags(resp);
      toast(`「${name}」を追加しました`);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function procUpdate(id) {
    const p = state.processes.find((x) => x.id === id);
    if (!p) return;
    try {
      const resp = await post("/api/process", { id, name: p.name, price: Number(p.price) || 0 });
      if (resp && resp.process) Object.assign(p, resp.process);
      syncUndoFlags(resp);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function procDelete(id, name) {
    if (!window.confirm(`工程「${name}」を削除しますか？\n（履歴は残ります）`)) return;
    try {
      const resp = await post("/api/process/delete", { id });
      const p = state.processes.find((x) => x.id === id);
      if (p) p.active = false;
      syncUndoFlags(resp);
      toast(`「${name}」を削除しました`);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function procReorder(id, dir) {
    const procs = state.processes.filter((p) => p.active !== false);
    const idx = procs.findIndex((p) => p.id === id);
    const target = idx + dir;
    if (idx < 0 || target < 0 || target >= procs.length) return;
    const reordered = procs.slice();
    const [moved] = reordered.splice(idx, 1);
    reordered.splice(target, 0, moved);
    const ids = reordered.map((p) => p.id);
    // Reflect order locally: rebuild processes preserving inactive ones at end.
    const inactive = state.processes.filter((p) => p.active === false);
    state.processes = reordered.concat(inactive);
    state.processes.forEach((p, i) => (p.order = i));
    render();
    try {
      const resp = await post("/api/process/reorder", { ids });
      syncUndoFlags(resp);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  // ---- Settings: staff ----
  async function staffAdd() {
    const nameEl = document.querySelector("#new-staff-name");
    const name = nameEl ? nameEl.value.trim() : "";
    if (!name) {
      toast("スタッフ名を入力してください", true);
      return;
    }
    try {
      const resp = await post("/api/staff", { name });
      if (resp && resp.staff) state.staff.push(resp.staff);
      if (!state.staffId && resp && resp.staff) state.staffId = resp.staff.id;
      syncUndoFlags(resp);
      toast(`「${name}」を追加しました`);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function staffUpdate(id) {
    const s = state.staff.find((x) => x.id === id);
    if (!s) return;
    try {
      const resp = await post("/api/staff", { id, name: s.name });
      if (resp && resp.staff) Object.assign(s, resp.staff);
      syncUndoFlags(resp);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function staffDelete(id, name) {
    if (!window.confirm(`スタッフ「${name}」を削除しますか？\n（履歴は残ります）`)) return;
    try {
      const resp = await post("/api/staff/delete", { id });
      const s = state.staff.find((x) => x.id === id);
      if (s) s.active = false;
      if (state.staffId === id) {
        const first = activeStaff()[0];
        state.staffId = first ? first.id : "";
      }
      syncUndoFlags(resp);
      toast(`「${name}」を削除しました`);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  async function staffReorder(id, dir) {
    const staff = state.staff.filter((s) => s.active !== false);
    const idx = staff.findIndex((s) => s.id === id);
    const target = idx + dir;
    if (idx < 0 || target < 0 || target >= staff.length) return;
    const reordered = staff.slice();
    const [moved] = reordered.splice(idx, 1);
    reordered.splice(target, 0, moved);
    const ids = reordered.map((s) => s.id);
    const inactive = state.staff.filter((s) => s.active === false);
    state.staff = reordered.concat(inactive);
    state.staff.forEach((s, i) => (s.order = i));
    render();
    try {
      const resp = await post("/api/staff/reorder", { ids });
      syncUndoFlags(resp);
      render();
    } catch (e) {
      reportError(e);
    }
  }

  // ---- Memo (debounced auto-save) ----
  // Timers keyed by "<staffId>|<date>" so switching staff/day before the
  // debounce fires does NOT cancel another cell's pending save, and each save
  // targets the staff/day captured at edit time (never the now-current ones).
  const memoTimers = new Map();
  function scheduleMemoSave(iso, text) {
    // Capture the staff/date for THIS edit now; the fire-time closure uses
    // these, not state.* (which may have changed by the time it fires).
    const saveStaffId = state.staffId;
    const saveDate = iso;
    const key = `${saveStaffId}|${saveDate}`;
    const existing = memoTimers.get(key);
    if (existing) clearTimeout(existing);
    const timer = setTimeout(async () => {
      memoTimers.delete(key);
      try {
        const resp = await post("/api/memo", { staffId: saveStaffId, date: saveDate, text });
        syncUndoFlags(resp);
        // Update undo/redo button state without nuking the textarea focus.
        updateUndoRedoButtons();
      } catch (e) {
        reportError(e);
      }
    }, 500);
    memoTimers.set(key, timer);
  }

  function updateUndoRedoButtons() {
    const undoBtn = document.querySelector('[data-action="undo"]');
    const redoBtn = document.querySelector('[data-action="redo"]');
    if (undoBtn) undoBtn.disabled = !state.canUndo;
    if (redoBtn) redoBtn.disabled = !state.canRedo;
  }

  /* ============================================================
   * CSV export
   * ========================================================== */
  function exportCsv() {
    const start = state.exportStart || state.weekStart;
    const end = state.exportEnd || addDays(state.weekStart, 6);
    if (start > end) {
      toast("開始日が終了日より後です", true);
      return;
    }
    const url = `/api/export.csv?start=${encodeURIComponent(start)}&end=${encodeURIComponent(end)}`;
    // Trigger a browser download.
    const a = document.createElement("a");
    a.href = url;
    a.download = `koteihyo_${start}_${end}.csv`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  }

  // Timecard monthly CSV (currently-selected staff).
  function tcExportCsv() {
    const month = state.tcMonthYM || (state.today || "").slice(0, 7);
    const sid = state.tcMonthStaffId;
    if (!month) {
      toast("月が選択されていません", true);
      return;
    }
    let url = `/api/timecard/export.csv?month=${encodeURIComponent(month)}`;
    if (sid) url += `&staffId=${encodeURIComponent(sid)}`;
    const a = document.createElement("a");
    a.href = url;
    a.download = sid ? `timecard_${month}_${sid}.csv` : `timecard_${month}.csv`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  }

  /* ============================================================
   * Event delegation
   * ========================================================== */
  /* ============================================================
   * 自動アップデート（§B3）: 適用 → 再起動待ち
   * ========================================================== */
  async function doUpdateApply() {
    state.updateConfirmOpen = false;
    const fallback = state.updateStatus && state.updateStatus.latest;
    const res = await postRaw("/api/update/apply", {});
    if (!res.ok) {
      if (res.status === 409) {
        toast("更新は既に進行中です", true);
      } else {
        toast((res.body && res.body.error) || `更新に失敗しました (${res.status})`, true);
      }
      render();
      return;
    }
    // ステージング成功 → サーバは再起動する。全画面「更新中…」へ。
    state.updating = true;
    state.updateTimedOut = false;
    state.updateTarget = (res.body && res.body.newVersion) || fallback || "";
    render();
    pollForRestart();
  }

  // bootstrap.version が newVersion になるまで 1 秒間隔で最大 90 秒待つ（§B3 step 5）。
  // 単なる 200 応答では旧プロセスの店じまい中かもしれないので版一致まで待つ。
  function pollForRestart() {
    const target = state.updateTarget;
    const start = Date.now();
    const tick = async () => {
      if (!state.updating) return; // 念のため（画面が変わったら停止）
      if (Date.now() - start > 90000) {
        state.updateTimedOut = true;
        render();
        return;
      }
      try {
        const data = await get("/api/bootstrap");
        if (data && data.version && data.version === target) {
          window.location.reload();
          return;
        }
      } catch (e) {
        // 再起動中は接続エラーになる。無視して待つ。
      }
      setTimeout(tick, 1000);
    };
    setTimeout(tick, 1000);
  }

  root.addEventListener("click", (event) => {
    const target = event.target.closest("[data-action]");
    if (!target) return;
    const action = target.dataset.action;
    // Draft protection: intercept destructive actions while a timecard edit is dirty.
    if (interceptForDraft(action, target)) return;
    dispatchClick(action, target);
  });

  function dispatchClick(action, target) {
    switch (action) {
      case "nav":
        goView(target.dataset.view);
        return;

      // ---- Calendar ----
      case "cal-prev":
        state.calMonth = addMonths(state.calMonth, -1);
        safe(loadMonthMarks).then(render);
        return;
      case "cal-next":
        state.calMonth = addMonths(state.calMonth, 1);
        safe(loadMonthMarks).then(render);
        return;
      case "cal-today":
        state.calMonth = state.today;
        safe(loadMonthMarks).then(render);
        return;
      case "pick-date": {
        const iso = target.dataset.date;
        state.weekStart = weekStartOf(iso);
        state.memoDayIdx = weekdayIndex(iso);
        goView("grid");
        return;
      }

      // ---- Week nav ----
      case "week-prev":
        state.weekStart = addDays(state.weekStart, -7);
        loadGrid().then(render);
        return;
      case "week-next":
        state.weekStart = addDays(state.weekStart, 7);
        loadGrid().then(render);
        return;
      case "week-this":
        state.weekStart = weekStartOf(state.today);
        loadGrid().then(render);
        return;

      // ---- Staff tabs ----
      case "pick-staff":
        state.staffId = target.dataset.staffId;
        loadGrid().then(render);
        return;

      // ---- Memo day tabs ----
      case "pick-memo-day":
        state.memoDayIdx = Number(target.dataset.idx) || 0;
        render();
        return;

      // ---- Count ----
      case "count":
        doCount(target.dataset.processId, target.dataset.date, target.dataset.op);
        return;

      // ---- Undo / Redo / Revert ----
      case "undo":
        doUndo();
        return;
      case "redo":
        doRedo();
        return;
      case "revert":
        doRevert(target.dataset.eventId);
        return;

      // ---- History filter ----
      case "history-clear":
        state.historyFilter = "";
        safe(loadHistory).then(render);
        return;

      // ---- Process settings ----
      case "proc-add":
        procAdd();
        return;
      case "proc-delete":
        procDelete(target.dataset.id, target.dataset.name);
        return;
      case "proc-up":
        procReorder(target.dataset.id, -1);
        return;
      case "proc-down":
        procReorder(target.dataset.id, 1);
        return;

      // ---- Staff settings ----
      case "staff-add":
        staffAdd();
        return;
      case "staff-delete":
        staffDelete(target.dataset.id, target.dataset.name);
        return;
      case "staff-up":
        staffReorder(target.dataset.id, -1);
        return;
      case "staff-down":
        staffReorder(target.dataset.id, 1);
        return;

      // ---- Export ----
      case "export-csv":
        exportCsv();
        return;
      case "print":
        window.print();
        return;

      // ---- Timecard ----
      case "tc-subtab": {
        const tab = target.dataset.tab;
        if (tab === state.timecardTab) return;
        state.timecardTab = tab;
        resetTCEdit();
        if (tab === "month") {
          safe(loadTCMonth).then(() => {
            render();
            syncPolling();
          });
        } else {
          safe(loadTCToday).then(() => {
            render();
            syncPolling();
          });
        }
        return;
      }
      case "tc-punch":
        doTCPunch(target.dataset.staffId, target.dataset.punch);
        return;
      case "tc-roster":
        state.tcMonthStaffId = target.dataset.staffId;
        resetTCEdit();
        safe(loadTCMonth).then(renderKeepingScroll);
        return;
      case "tc-month-prev":
        state.tcMonthYM = addYM(state.tcMonthYM || (state.today || "").slice(0, 7), -1);
        resetTCEdit();
        safe(loadTCMonth).then(renderKeepingScroll);
        return;
      case "tc-month-next":
        state.tcMonthYM = addYM(state.tcMonthYM || (state.today || "").slice(0, 7), 1);
        resetTCEdit();
        safe(loadTCMonth).then(renderKeepingScroll);
        return;
      case "tc-month-this":
        state.tcMonthYM = (state.today || "").slice(0, 7);
        resetTCEdit();
        safe(loadTCMonth).then(renderKeepingScroll);
        return;
      case "tc-csv":
        tcExportCsv();
        return;
      case "tc-print":
        window.print();
        return;
      case "tc-edit-open":
        openTCEdit(target.dataset.date);
        return;
      case "tc-edit-save":
        saveTCEdit();
        return;
      case "tc-delete-open":
        state.tcDeleteConfirmOpen = true;
        renderKeepingScroll();
        return;
      case "tc-delete-cancel":
        state.tcDeleteConfirmOpen = false;
        renderKeepingScroll();
        return;
      case "tc-delete-confirm":
        saveTCEdit({ deleteAll: true });
        return;
      case "tc-edit-cancel":
        resetTCEdit();
        renderKeepingScroll();
        return;
      case "tc-break-add":
        if (state.tcEditDraft) {
          state.tcEditDraft.breaks.push({ start: "", end: "" });
          state.tcEditDirty = true;
          renderKeepingScroll();
        }
        return;
      case "tc-break-del":
        if (state.tcEditDraft) {
          state.tcEditDraft.breaks.splice(Number(target.dataset.idx) || 0, 1);
          state.tcEditDirty = true;
          renderKeepingScroll();
        }
        return;
      case "tc-discard":
        discardDraftAndContinue();
        return;
      case "tc-keep-editing":
        keepEditing();
        return;

      // ---- 自動アップデート ----
      case "update-apply":
        state.updateConfirmOpen = true;
        render();
        return;
      case "update-cancel":
        state.updateConfirmOpen = false;
        render();
        return;
      case "update-dismiss":
        state.updateDismissed = true;
        state.updateConfirmOpen = false;
        render();
        return;
      case "update-confirm":
        doUpdateApply();
        return;
      case "update-reload":
        window.location.reload();
        return;

      default:
        return;
    }
  }

  // Text inputs / dates (input event for live, change for commit).
  root.addEventListener("input", (event) => {
    const target = event.target.closest("[data-action]");
    if (!target) return;
    const action = target.dataset.action;

    if (action === "memo-input") {
      const iso = target.dataset.date;
      if (state.week && state.week.dates) {
        const idx = state.week.dates.indexOf(iso);
        if (idx >= 0) {
          if (!state.week.memos) state.week.memos = [];
          state.week.memos[idx] = target.value;
        }
      }
      scheduleMemoSave(iso, target.value);
      return;
    }

    if (action === "proc-name") {
      const p = state.processes.find((x) => x.id === target.dataset.id);
      if (p) p.name = target.value;
      return;
    }
    if (action === "proc-price") {
      const p = state.processes.find((x) => x.id === target.dataset.id);
      if (p) p.price = Number(target.value) || 0;
      return;
    }
    if (action === "staff-name") {
      const s = state.staff.find((x) => x.id === target.dataset.id);
      if (s) s.name = target.value;
      return;
    }
    if (action === "export-start") {
      state.exportStart = target.value;
      return;
    }
    if (action === "export-end") {
      state.exportEnd = target.value;
      return;
    }

    // ---- Timecard inline editor (mutate draft, no re-render to keep focus) ----
    if (!state.tcEditDraft) return;
    if (action === "tc-edit-in") {
      state.tcEditDraft.in = target.value;
      state.tcEditDirty = true;
    } else if (action === "tc-edit-out") {
      state.tcEditDraft.out = target.value;
      state.tcEditDirty = true;
    } else if (action === "tc-edit-note") {
      state.tcEditDraft.attendanceNote = target.value;
      state.tcEditDirty = true;
    } else if (action === "tc-edit-break-start") {
      const i = Number(target.dataset.idx) || 0;
      if (state.tcEditDraft.breaks[i]) {
        state.tcEditDraft.breaks[i].start = target.value;
        state.tcEditDirty = true;
      }
    } else if (action === "tc-edit-break-end") {
      const i = Number(target.dataset.idx) || 0;
      if (state.tcEditDraft.breaks[i]) {
        state.tcEditDraft.breaks[i].end = target.value;
        state.tcEditDirty = true;
      }
    }
  });

  // Commit edits / filters on change (blur for text, immediate for date).
  root.addEventListener("change", (event) => {
    const target = event.target.closest("[data-action]");
    if (!target) return;
    const action = target.dataset.action;

    if (action === "proc-name" || action === "proc-price") {
      procUpdate(target.dataset.id);
      return;
    }
    if (action === "staff-name") {
      staffUpdate(target.dataset.id);
      return;
    }
    if (action === "history-filter") {
      state.historyFilter = target.value;
      safe(loadHistory).then(render);
      return;
    }

    // ---- Timecard settings (config) ----
    // All three controls save a complete current-form snapshot via saveTCConfig()
    // (single-flight, finding 9).
    if (
      action === "tc-cfg-round-unit" ||
      action === "tc-cfg-round-dir" ||
      action === "tc-cfg-std-hours" ||
      action === "tc-cfg-std-min"
    ) {
      saveTCConfig();
    }
  });

  // Submit add-forms on Enter.
  root.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    const target = event.target;
    if (target.id === "new-proc-name" || target.id === "new-proc-price") {
      event.preventDefault();
      procAdd();
    } else if (target.id === "new-staff-name") {
      event.preventDefault();
      staffAdd();
    }
  });

  /* ============================================================
   * Background freshness (multi-device sync)
   * ----------------------------------------------------------
   * With multiple iPads/PCs editing the same data, a tab's local
   * week / canUndo / canRedo / calendar dots drift from the server.
   * Re-pull on visibility regain and on a slow poll while a
   * multi-device-sensitive view (grid/history) is open. Never steal
   * focus from an in-progress memo edit.
   * ========================================================== */
  const POLL_MS = 12000;
  let pollTimer = null;
  let refreshing = false;

  // True while the user is typing in a text field we must not disrupt.
  function isEditingText() {
    const el = document.activeElement;
    if (!el) return false;
    const tag = el.tagName;
    return tag === "TEXTAREA" || tag === "INPUT" || el.isContentEditable === true;
  }

  // Re-pull the current view's authoritative data and re-render in place.
  // Skips entirely while editing text so focus/caret are preserved.
  async function refreshCurrentView() {
    if (refreshing) return;
    if (document.hidden) return;
    if (!state.ready || state.error) return;
    // Only the multi-device-sensitive views need background refresh.
    if (state.view !== "grid" && state.view !== "history" && state.view !== "timecard") return;
    if (isEditingText()) return;
    refreshing = true;
    try {
      if (state.view === "grid") {
        await loadGrid();
        if (isEditingText()) return; // user started typing mid-fetch
        renderKeepingScroll();
      } else if (state.view === "history") {
        await safe(loadHistory);
        if (isEditingText()) return;
        render();
      } else if (state.view === "timecard") {
        if (state.timecardTab === "punch") {
          await safe(loadTCToday);
          render(); // re-anchors the clock via loadTCToday
        } else {
          // Dirty draft: skip the refetch entirely so the poll's roster fallback
          // can't change the staff/month selection under a pending save (finding 1).
          if (state.tcEditDirty) return;
          await safe(loadTCMonth);
          if (state.tcEditDirty) return; // became dirty mid-fetch
          reconcileCleanDraft(); // clean editor: rebuild from refreshed row (finding 8)
          if (isEditingText()) return;
          renderKeepingScroll();
        }
      }
    } finally {
      refreshing = false;
    }
  }

  // Start/stop the poll loop to match the current view.
  function syncPolling() {
    const wantPolling =
      state.view === "grid" || state.view === "history" || state.view === "timecard";
    if (wantPolling && !pollTimer) {
      pollTimer = setInterval(() => {
        refreshCurrentView();
      }, POLL_MS);
    } else if (!wantPolling && pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      // Pause polling while hidden; resume + immediate refresh on return.
      if (pollTimer) {
        clearInterval(pollTimer);
        pollTimer = null;
      }
      syncTCClock(); // stops the punch clock while hidden
    } else {
      refreshCurrentView();
      syncPolling();
      syncTCClock(); // restart the punch clock on return
    }
  });

  // Browser reload/close protection while a timecard edit is unsaved.
  window.addEventListener("beforeunload", (event) => {
    if (state.tcEditDirty) {
      event.preventDefault();
      event.returnValue = "";
      return "";
    }
  });

  /* ============================================================
   * Boot
   * ========================================================== */
  async function boot() {
    try {
      await loadBootstrap();
      await safe(loadMonthMarks);
    } catch (e) {
      state.error = `起動データの取得に失敗しました: ${e && e.message ? e.message : e}`;
    }
    render();
    syncPolling();
    // 更新確認は起動をブロックしない。起動時の非同期確認が終わるまで数回再取得して
    // リロード無しでバナーを反映する（finding 5）。
    refreshUpdateStatus(3);
  }

  boot();
})();
