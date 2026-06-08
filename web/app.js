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
          ["count_inc", "count_dec", "count_set", "memo_set"].includes(e.kind)
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
    state.view = view;
    if (view === "grid") {
      await loadGrid();
    } else if (view === "history") {
      await safe(loadHistory);
    } else if (view === "calendar") {
      await safe(loadMonthMarks);
    }
    render();
    // Poll only while a multi-device-sensitive view (grid/history) is showing.
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
      </main>
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
   * Master render
   * ========================================================== */
  function render() {
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
      case "calendar":
      default:
        root.innerHTML = renderCalendarView();
        break;
    }
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

  /* ============================================================
   * Event delegation
   * ========================================================== */
  root.addEventListener("click", (event) => {
    const target = event.target.closest("[data-action]");
    if (!target) return;
    const action = target.dataset.action;

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

      default:
        return;
    }
  });

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
    if (state.view !== "grid" && state.view !== "history") return;
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
      }
    } finally {
      refreshing = false;
    }
  }

  // Start/stop the poll loop to match the current view.
  function syncPolling() {
    const wantPolling = state.view === "grid" || state.view === "history";
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
    } else {
      refreshCurrentView();
      syncPolling();
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
  }

  boot();
})();
