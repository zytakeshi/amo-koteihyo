# [FABLE5] koteihyo — タイムカードシステム + 自動アップデート + ファイアウォール対策 実装計画

> Status: PLAN v6 FINAL — codex-verified implementation-ready 93/100 (5 review iterations, 23+14+10+6+4 findings folded in). Source of truth for the /build-loop execution.
> Base design contract: `docs/koteihyo-design-and-api-contract.md` (v1) — this plan extends it; where they conflict, this plan wins for the new features, v1 wins for existing behavior.
> Constraints inherited: Go stdlib only (no external modules), vanilla HTML/CSS/JS frontend, JSON file persistence, single exe, LAN-only, no auth.

## 0. Scope / priority

| # | Feature | Priority | Approach |
|---|---------|----------|----------|
| A | タイムカードシステム (timecard) | ★ 最重要 | New view + store domain inside the existing app/exe |
| B | 自動アップデート (GitHub Releases) | 高 | Startup check + user prompt + staged self-replace |
| C | Windows Defender / Firewall blocking | 任意→**推奨** (real bug found in current code) | Fix `ensureWindowsFirewall()` per §C |

Owner decisions already made (2026-07-18):
- Timecard lives **inside** the koteihyo app (same exe, same JSON store, same staff list, same LAN/QR access).
- Punch types: **出勤・退勤 + 休憩(開始/終了、複数回可)**.
- Outputs: **月次集計 + CSV/印刷**, **打刻修正 (admin edit, full history)**, **丸め表示(参考)**, plus good-to-have extras (§A6, §A9).

## 1. Existing architecture (recon summary, verified file:line)

- `internal/store/store.go` — single `sync.Mutex`, atomic tmp+rename JSON save, append-only `Events []*Event` with `Prev/Next interface{}` snapshots, undo = `lastUndoableIndex()` balance-scan + `reverseApply()`, redo stack cleared on every real op via `appendEvent` (store.go:557), audit kinds `undo/redo/revert`. `normalize()` (store.go:110) nil-safes maps on load → **new DB fields are backward-compatible without a schema migration**. `isRealOp` (store.go:381) treats every non-audit kind as real — new kinds need no registration there (keep as-is; do NOT convert to allowlist).
- `internal/api/api.go` — flat `ServeMux` paths, `requireMethod` → `decodeBody`(anon struct) → store call → `writeErr`/`writeJSON` **flat envelopes (no `{data:…}` wrapper)**; every mutating response carries `canUndo/canRedo`.
- `web/app.js` (1699 lines, IIFE) — `state` + `render()` switch + `data-action` event delegation; `goView()` loads per-view data; settings data comes from bootstrap (app.js:207); 12s polling on multi-device views (`syncPolling` app.js:1660) with full-innerHTML re-render; `renderKeepingScroll()` for wide tables; toast for errors. Calendar "input" dots use an **allowlisted event-kind predicate** (app.js:253) — new kinds must be added there. Mobile topbar nav is a **hard-coded 4-column grid** (styles.css:1014).
- `main.go` — port 8080→+1 fallback (max 50, main.go:152), `0.0.0.0` listen, LAN IP pick w/ virtual-NIC filter, browser auto-open, console banner, `ensureWindowsFirewall()` (main.go:299) — **buggy, see §C**.
- Release state: GitHub `zytakeshi/amo-koteihyo`, one release `v1.0.0` (asset `AMO-koteihyo.exe`, ASCII name deliberate). No version stamping exists yet. `build/build-windows.sh` builds only (no ldflags, no checksum).

---

# A. タイムカードシステム

## A1. Concept

A digital replacement for the paper punch card: an iPad by the door (or the PC) shows a big clock + big per-staff punch buttons; the store records raw punch facts; the monthly per-staff sheet (screen/CSV/print) replaces the paper card for payroll. All mutations flow through the existing event log → undo/redo/history/revert work for punches exactly like counts.

**Payroll-correctness principle (codex F6, MHLW guidance)**: per-day rounding of worked time (e.g. 15-min truncation) is improper underpayment under JP labor guidance. Therefore: **raw minutes are the payroll numbers**. Rounded values are strictly 参考表示 (reference display), clearly labeled, default OFF (`roundUnit:1`).

## A2. Data model (store)

New fields on `DB` (model.go) — all nil-safed in `normalize()`, `Version` stays `1`:

```jsonc
{
  // key = "<staffId>|<YYYY-MM-DD>"
  "attendance": {
    "s_8|2026-07-18": {
      "in": "09:58",                    // "HH:MM" server-time punch or admin edit; "" = none
      "out": "19:04",                   // "" = not clocked out yet
      "breaks": [ { "start": "12:30", "end": "13:10" } ],  // end "" = on break (only legal on the open/current day)
      "note": "遅刻(電車遅延)",          // 勤怠メモ — DISTINCT from existing memos (業務メモ); API field name attendanceNote
      "flags": ["midnight_clamped"]     // provenance; cleared only when 打刻修正 changes time/break content (note-only edits preserve)
    }
  },
  // per-day revision counter for optimistic concurrency (§A7 /timecard/day).
  // NOT event-sourced, NOT part of tc_set snapshots; incremented on EVERY mutation
  // of that key (punch, admin edit, undo, redo, revert). Persisted as derived cache.
  "attendanceRev": { "s_8|2026-07-18": 4 },
  "tcConfig": {
    "roundUnit": 1,                     // minutes: 1 (=丸めなし, DEFAULT) | 5 | 10 | 15 | 30 — 参考表示 only
    "roundDir": "floor",                // "floor" | "nearest" (half-up) | "ceil" — applied to daily worked minutes, display only
    "standardMinutes": 480              // 所定労働時間/日 → 所定時間超過(参考) = max(0, rawWorked − これ)
  }
}
```

Design rules:
- **Raw punches are never rounded at write time**, and raw values drive all totals/CSV payroll columns. Rounding is a pure read-time computation for the 参考 columns only.
- Times are `"HH:MM"` local (JST, server clock). One timezone, no TZ math.
- Go types: `DayAttendance{In, Out string; Breaks []BreakSpan; Note string; Flags []string}`, `BreakSpan{Start, End string}`, `TCConfig{RoundUnit int; RoundDir string; StandardMinutes int}`.
- `nil` attendance entry means "absent"; an existing record with empty fields (e.g. note-only) is a distinct valid state — decoders and delete semantics must keep the distinction (codex F9).
- Existing `memos` domain (grid view 業務メモ) stays untouched and separate; the attendance note is a different field with different labels (勤怠メモ) and its own event coverage via `tc_set` (codex F19).

### Midnight / business-date rule (codex F7, F8)

- Business date = server calendar date at punch time, EXCEPT the carryover case:
- **Carryover**: if staff S has yesterday's record with `in!=""`, `out==""`, and **no open break**, then S's effective state is 勤務中(前日); `/api/timecard/today` reports it (`carryover:true, bizDate:<yesterday>`), the UI shows a 退勤 button labeled 「退勤（前日分）」, and the punch applies to **yesterday** with `out:"23:59"` + flag `midnight_clamped`. New 出勤 for today is **blocked until the carryover is resolved** (退勤(前日分) or admin correction).
- If yesterday's record has an **open break**, punch buttons are locked for S with message 「前日の休憩が終了していません。月次画面の打刻修正で直してください」 — admin correction only.
- **Precedence (iter-2 F5)**: (1) yesterday-or-today record `invalid` (corrupt facts) → all punch buttons locked, admin correction only; (2) unresolved yesterday (carryover or open-break-lock) → carryover state dominates; creating/mutating **today's** record for S via punch is rejected until resolved (admin edit of today stays allowed — it's the repair tool); (3) otherwise derive today normally. `/today` returns the effective state plus, when in carryover, both `bizDate` (yesterday) and today's date so the UI is unambiguous.
- `midnight_clamped` (and any future flags) live in the snapshot → survive undo/redo. 打刻修正 clears flags **only when a time/break field actually changed** (`in`, `out`, or semantic break content, with `nil`≡`[]`); note-only edits preserve them (iter-4 P2). Never reconstructed heuristically.

## A3. Event-log integration (undo/redo/history)

**One real-op kind covers every timecard day mutation**: `tc_set`. Config: `tc_config_set`.

- `Prev`/`Next` = full JSON snapshot of the `DayAttendance` for `staffId|date` (`null` = absent). Inverse = "set attendance[key] to Prev (delete when null)" — trivially correct.
- `BizDate` = attendance date; `StaffID` = the staff; history grouping/labels work unchanged. `Label` per action: `"出勤 09:58"`, `"休憩開始 12:30"`, `"休憩終了 13:10"`, `"退勤 19:04"`, `"退勤(前日分) 23:59"`, `"打刻修正"`, `"勤怠メモ変更"`.
- `tc_config_set`: `Prev/Next` = `TCConfig` snapshot, `BizDate` = op date, `StaffID` = `""`, label `"タイムカード設定変更"`.
- Reuse `appendEvent` (store.go:557) so Seq/ID/TS assignment and redo-clearing match existing conventions exactly.
- **No-op saves are no-ops**: an admin save or config save identical to current state appends no event and does not clear redo (mirror `CountOp`/`MemoSet` early-return convention, store.go:481) (codex F9).

Implementation requirements (codex F9 — all mandatory):
1. **Deep-copy snapshots**: `cloneDay()` copies `Breaks` and `Flags` slices (`append([]T(nil), s...)`) — a shallow struct copy aliases the slices and can corrupt `Prev` after later mutation.
2. Snapshot decoders `decodeDay(v any) (*DayAttendance, error)` / `decodeTCConfig(v any) (*TCConfig, error)` (JSON re-marshal round-trip, placed next to `toInt/toStr/toStrSlice` store.go:1389) **return errors** — never silently zero-value.
3. Extend `reverseApply`/`forwardApply` for the new kinds; for these kinds decode errors must **fail loudly** (undo/redo/revert returns the error to the API layer → 500 with message) rather than silently applying garbage. (Existing kinds keep their current semantics — no refactor of the existing switch beyond adding cases.)
4. `attendanceRev[key]++` on every mutation path of a key: punch, admin edit, and inside reverse/forward apply for `tc_set`.
5. **Transaction safety (iter-2 F1, iter-3 F1)**: undo/redo/revert and every timecard mutation must be all-or-nothing under the mutex. Because Undo/Redo/Revert can touch ANY domain (counts, memos, staff, processes, order, attendance, revs, config, Events/Redo/Seq), the checkpoint is a **full-DB deep clone** (`cloneDB` via JSON marshal/unmarshal round-trip — cheap at this data size): `before := cloneDB(s.db)` → decode/validate all inputs (for Revert: decode every target snapshot up front) → mutate → `save()`; on ANY apply error or save error, `s.db = before` and return the error. Rev check → no-op comparison → mutation → rev++ → event append → save all happen inside one mutex hold. Test: injected save failure after a Revert spanning `staff_rename + count_set + tc_set + tc_config_set` leaves the marshaled DB byte-identical to before.
6. Tests: undo/redo round-trip restores byte-identical snapshots (incl. note+flags); undo→new-punch clears redo; Revert across interleaved `tc_set`/`count_*`/`tc_config_set` events; decoder rejection of corrupt snapshots; checkpoint-restore on injected save failure leaves state unchanged.
- Staff soft-delete: attendance is retained forever; roster filtering is read-time (§A7).

## A4. Punch state machine (server-authoritative)

`POST /api/timecard/punch {staffId, action}` — server time decides the stamp; client sends **no timestamp**.

```
状態:  未出勤 ──出勤──▶ 勤務中 ──休憩開始──▶ 休憩中
                          ▲                    │
                          └──── 休憩終了 ◀─────┘
        勤務中 ──退勤──▶ 退勤済
  (前日 carryover: 勤務中(前日) ──退勤(前日分)──▶ 前日=退勤済(clamped) → 今日の出勤が解禁)
```

| Effective state (derived per §A2 incl. carryover) | Allowed actions |
|---|---|
| carryover 勤務中(前日) | 退勤(前日分) のみ (out→yesterday, 23:59 clamp + flag) |
| carryover 休憩中(前日) | なし (admin correction only) |
| 未出勤 (`in==""`, no carryover) | 出勤 |
| 勤務中 (`in!=""`, no open break, `out==""`) | 休憩開始, 退勤 |
| 休憩中 (`breaks[last].end==""`) | 休憩終了 |
| 退勤済 (`out!=""`) | なし (再出勤は打刻修正で) |

- Invalid action for state → 400 with Japanese message (`"すでに出勤済みです"`, `"休憩終了を先に押してください"` etc.). Frontend disables illegal buttons; the server check is the invariant.
- Punch for inactive staff → 400.
- Concurrent duplicate punch from two devices: mutex serializes; the loser gets the state-machine 400 → toast; no double punch.

## A5. Worked-time computation (store, pure functions)

```
rawWorked   = (out − in) − Σ(break.end − break.start)      // minutes; incomplete day (no out / open break) → no total
refRounded  = roundUnit==1 ? rawWorked : round(rawWorked, roundUnit, roundDir)   // 参考表示 only
overStd     = max(0, rawWorked − standardMinutes)          // 「所定時間超過(参考)」— informational, NOT statutory 残業 (codex F15)
```

- Rounding `nearest` = half-up on the minute boundary; boundary table tests required (0/half/exact-unit ± 1).
- Incomplete day: listed with ⚠, excluded from totals, never silently dropped.
- Display `H:MM`; CSV additionally exports decimal hours (2 fixed decimals, e.g. `8.43`).
- **Totals (screen + CSV) are computed from rawWorked.** The rounded column is labeled 「実働(丸め・参考)」 and appears only when `roundUnit != 1`.

## A6. Anomaly detection

Server-computed, returned by today/month endpoints:
- **退勤忘れ**: past date (`< today`, and not the §A2 carryover-eligible yesterday) with `in!=""`, `out==""`.
- **休憩閉じ忘れ**: past date with an open break.
- **midnight_clamped** days: shown with ⚠ until admin-corrected.
- **clock_warp** days (§A10 clamp occurred): shown with ⚠ until admin-corrected (iter-3 F7 — kind included in the API anomalies list).
- **Today's incomplete record is NOT an anomaly** (it's just an open day) (codex F14).
- Corrupt loaded facts (malformed times, out-without-in in the JSON): `normalize()` does **not** rewrite facts — it only nil-safes containers; corrupt day records are surfaced as flagged anomalies (`kind:"invalid"`) for admin correction (fail-visible, not fail-silent).

## A7. REST API

Conventions identical to existing handlers: `requireMethod` / `decodeBody` / `writeErr` / **flat envelopes** / `canUndo`+`canRedo` on every response listed below (GET included, so views can initialize undo buttons — codex F12).

| Method | Path | Body / Query | Returns |
|---|---|---|---|
| GET | `/api/timecard/today` | — | `{ date, serverNow (RFC3339), staff:[{staffId,name,state,carryover,bizDate,in,out,breaks,attendanceNote,flags}], anomalies:[{staffId,staffName,date,kind}], canUndo, canRedo }` |
| POST | `/api/timecard/punch` | `{ staffId, action:"in"\|"break_start"\|"break_end"\|"out" }` | `{ day:{…}, state, rev, canUndo, canRedo }` |
| GET | `/api/timecard/month` | `?staffId=&month=YYYY-MM` | `{ month, staffId, roster:[{staffId,name,inactive}], days:[{date, weekday, in, out, breaks, attendanceNote, flags, workedRaw, workedRef, overStd, complete, rev}], totals:{days, workedRaw, workedRef, overStd}, config:{…}, canUndo, canRedo }` — `roster` = active staff ∪ inactive staff with attendance in the month, so the 月次 tabs can show departed staff (iter-2 F7) |
| POST | `/api/timecard/day` | `{ staffId, date, in, out, breaks:[{start,end}], attendanceNote, expectedRev }` | `{ day:{…}, rev, canUndo, canRedo }`; **rev mismatch → 409 `{ error, currentDay, currentRev }`** (codex F10). Full-day replace; all-empty deletes the record; flags cleared **only when `in`/`out`/semantic break content changed** (note-only edit preserves them; `nil` and `[]` breaks compare equal) — see §A2 flag rules (iter-4 P2). |
| GET | `/api/timecard/export.csv` | `?month=YYYY-MM&staffId=`(省略=全員) | `text/csv` UTF-8 BOM, §A8 |
| POST | `/api/timecard/config` | `{ roundUnit, roundDir, standardMinutes }` | `{ config, canUndo, canRedo }` |

- `serverNow` is RFC3339 (not `HH:MM`) so the client clock can tick seconds and re-anchor per poll (codex F18).
- **Bootstrap additions (explicit)**: `/api/bootstrap` response gains `version` (app version, §B) and `tcConfig` (settings view reads config from bootstrap like everything else — codex F12).
- **Roster rule (codex F11)**: `/today` = active staff only. `/month` + CSV = active staff **plus inactive staff having any attendance in the requested month**, marked `inactive:true` → UI badge 「退職/無効」. Payroll history is never silently dropped.
- 打刻修正 validation (§A10) runs before the rev check response shape matters: validation 400s cite the field.
- **Error classification (iter-3 F5)**: store returns typed errors — `ValidationError{Msg}` → 400, sentinel `ErrConflict` → 409 (+`currentDay`/`currentRev` payload), anything else (decode corruption, save failure) → 500. Handlers map via `errors.As`/`errors.Is`; never blanket-400 store errors for timecard endpoints.
- **Flag preservation (iter-3 F7)**: `/timecard/day` clears `flags` only when a time/break field actually changed; a note-only edit preserves existing flags (provenance must survive annotations).
- **Config validation (iter-3 F8)**: `roundUnit ∈ {1,5,10,15,30}`, `roundDir ∈ {floor,nearest,ceil}`, `0 ≤ standardMinutes ≤ 1440` — field-specific 400s; identical config = no-op (no event, redo untouched). `attendanceNote` limit = 500 Unicode code points (not bytes).

## A8. CSV / print

CSV (Excel-JP: BOM + `encoding/csv` in-memory buffer, mirroring api.go:322). Raw drives payroll; 参考 columns clearly labeled (codex F13 — example numbers below are consistent: 09:58–19:04 − 0:40 = 8:26 raw; 15分切捨て参考 = 8:15; 所定480分 → 超過 0:26):

```
スタッフ,日付,曜日,出勤,退勤,休憩(分),実働,実働(時間),実働(丸め・参考),所定超過(参考),備考,フラグ
浜田,2026-07-01,水,09:58,19:04,40,8:26,8.43,8:15,0:26,,
…
浜田,合計,,,,,168:30,168.50,,,出勤20日,
```

- 「実働(丸め・参考)」column omitted entirely when `roundUnit==1`. 合計 row sums raw only.
- **CSV formula-injection guard (iter-3 F9, iter-4 P2)**: one `csvSafe()` helper — any user-controlled text cell beginning with `=`, `+`, `-`, `@`, TAB, or CR is prefixed with `'`. Applied to staff names, **process names** (editable too, api.go:347), and 勤怠メモ, in BOTH the timecard CSV and the existing 工程表 CSV. Tests: `=HYPERLINK(...)`, `+SUM(...)`, TAB, CR in staff and process names.
- Filename `timecard_<YYYY-MM>[_<staff>].csv`; all-staff export = per-staff blocks separated by a blank row, inactive staff included per §A7 roster rule.
- **Print = currently selected staff only** (v1). The 月次 view gets `@media print` treatment (hide chrome, A4 portrait). Print-all-staff is explicitly deferred (would require pre-rendering all staff sections; note as future work) (codex F20).

## A9. Frontend (web/app.js + styles.css)

New topbar nav button **タイムカード** → view `timecard`, sub-tabs **打刻** (default) / **月次**. Timecard config panel goes into the existing settings view.

**Topbar/CSS**: adding a 5th nav button requires updating the hard-coded 4-column mobile nav grid (styles.css:1014) → 5 columns ≤820px or a 3+2 wrap; pick whichever keeps ≥44px tap height at 390px width (codex F17).

**New state fields** (codex F17): `timecardTab ("punch"|"month")`, `tcToday`, `tcMonth`, `tcMonthStaffId`, `tcMonthYM`, `tcEditDraft` (per-row edit buffer incl. `expectedRev`), `tcEditDirty (bool)`, `tcClockAnchor` (serverNow + local perf timestamp).

**Lifecycle rules**:
- Clock: single `setInterval` 1s ticking from `tcClockAnchor`, governed by one central `syncTCClock()` (idempotent create/clear) whose run-condition is `state.view==="timecard" && state.timecardTab==="punch" && !document.hidden` — called from `goView`, sub-tab switches, and `visibilitychange` (iter-2 F12).
- Draft protection (iter-2 F12): one central guard `canLeaveTCEdit()` — when `tcEditDirty`, ALL draft-destroying actions route through it (nav, sub-tab switch, staff-tab switch, month ◀▶, undo/redo/revert buttons): `confirm("編集中の内容を破棄しますか？")`-style dialog (custom, not window.confirm — existing app has no dialogs; a small inline confirm bar is fine); poll re-render stays suppressed while dirty (fetch continues, render deferred).
- 409 on save → toast 「他の端末で更新されました。最新を確認してください」 + reload row + re-open draft against new rev.
- `beforeunload` handler active while `tcEditDirty` (browser reload/close protection); extend `refreshAfterMutation()` (app.js:1198) with branches for both timecard tabs so undo/redo/revert from any view refreshes timecard data (iter-3 F10).

### 打刻 screen (door iPad)

```
┌──────────────────────────────────────────────┐
│ [logo]  カレンダー 履歴 設定 タイムカード …  │
│ ┌──────────────────────────────────────────┐ │
│ │ [打刻] [月次]                            │ │
│ ├──────────────────────────────────────────┤ │
│ │        7月18日(土)  14:23:07  ← 大時計    │ │
│ │ ⚠ 7/17 浜田さんの退勤打刻がありません     │ │
│ ├──────────────────────────────────────────┤ │
│ │ 小池   [勤務中 09:58〜]                  │ │
│ │   (出09:58)   [休憩開始]  [退勤]         │ │
│ ├──────────────────────────────────────────┤ │
│ │ 井上   [休憩中 12:30〜]                  │ │
│ │   [休憩終了]                             │ │
│ ├──────────────────────────────────────────┤ │
│ │ 浜田   [勤務中(前日)]                    │ │
│ │   [退勤（前日分）]                        │ │
│ └──────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

- One card per active staff: state badge (未出勤 gray / 勤務中 green / 休憩中 brass / 退勤済 ink / 前日 rose) + today's punch summary + only state-legal buttons, ≥64px tall. 出勤 `.btn-rose`, 退勤 `.btn-ink`, 休憩 `.btn-soft`.
- Tap → immediate punch → toast `"小池さん 出勤 09:58"` → card refresh. Mistakes: existing ⟲ undo (works on `tc_set`).
- No visual change to existing screens except the new topbar button + new settings panel.

### 月次 screen

```
│ [打刻] [月次]                                │
│ [小池][井上][浜田 (退職)]  ← staff-tabs      │
│  ◀ 2026年7月 ▶   [CSV] [印刷]               │
│ ┌──────────────────────────────────────────┐ │
│ │ 日 曜 出勤  退勤  休憩 実働 超過(参考) ✎ │ │
│ │ 1  水 09:58 19:04 0:40 8:26 0:26       ✎ │ │
│ │ 2  木  —     —     —    —    —         ✎ │ │
│ │ 17 金 10:02  ⚠—   0:35  —    —         ✎ │ │
│ ├──────────────────────────────────────────┤ │
│ │ 出勤20日   実働 168:30   超過 4:15       │ │
│ └──────────────────────────────────────────┘ │
```

- `✎` expands an inline editor row (`.settings-row` pattern): `<input type="time">` in/out, break pairs add/remove, 勤怠メモ text, 保存/取消; save posts `/api/timecard/day` with `expectedRev`.
- Wide table in `.grid-scroll`; `renderKeepingScroll()` on refresh; empty month → `.empty-state`.
- Calendar view dots: add `tc_set` to the input-kind allowlist predicate (app.js:253) so punch days get input dots (codex F16).

### Settings panel (existing settings view)

```
│ ┌─ タイムカード設定 ─────────────────────┐ │
│ │ 丸め表示(参考) [なし|5分|10分|15分|30分] │ │
│ │ 丸め方向 [切り捨て|四捨五入|切り上げ]    │ │
│ │ 所定労働時間 [ 8 ]時間[ 0 ]分 /日        │ │
│ │ ※給与計算は常に実測(丸めなし)の値を使用  │ │
│ └────────────────────────────────────────┘ │
```

## A10. Admin-edit validation matrix (codex F14)

`POST /api/timecard/day` rejects with a specific Japanese message when:
1. `date` malformed, or **in the future** (`> today`).
2. Any time not `^(?:[01]\d|2[0-3]):[0-5]\d$` — canonical zero-padded `HH:MM` only (`9:05` rejected); internally parse to minutes and re-serialize `%02d:%02d` so equality/ordering/no-op checks are consistent (iter-2 F13).
3. `out` set without `in`; any break present without `in`.
4. Ordering violated: `in ≤ break₁.start < break₁.end ≤ break₂.start … ≤ out` (equal minutes allowed at pair boundaries; a zero-length break `start==end` is rejected).
5. Breaks overlap, or a closed break lies outside `[in, out]` when `out` set.
6. Open break (`end==""`): only allowed when `date == today` AND `out == ""` AND it is the last break.
7. `out==""` allowed only for `date == today` (past days must be complete or all-empty=delete; use the anomaly flow otherwise).
8. `attendanceNote` > 500 chars; breaks > 10 per day.
9. `expectedRev` missing or ≠ current → 409 (not 400).
10. Staff unknown (inactive staff IS editable — payroll fixes for departed staff are legitimate).
- System-clock rollback (iter-2 F6): punch API computes the action-specific minimum legal time from the existing record (`break_end` ≥ its `start`+1 minute; `out`/`break_start` ≥ preceding boundary, equal allowed) and clamps `now` up to it; when clamping occurred, add flag `clock_warp` (listed in §A6 anomalies, shown with ⚠). If the minimum exceeds `23:59`, reject with admin-correction message. Never writes out-of-order times, never creates a zero-length break.

---

# B. 自動アップデート (GitHub Releases)

## B1. Version stamping (codex F22)

- `var version = "dev"` in main.go; shown in banner, settings footer, `/api/bootstrap`.
- `build/build-windows.sh`: release mode **requires a clean tree and an exact `vMAJOR.MINOR.PATCH` tag** (`git describe --tags --exact-match` + `git status --porcelain` empty) — else it aborts (dev builds via a `--dev` flag stamp `dev-<shorthash>` instead). Emits `AMO-koteihyo.exe.sha256` (64-hex, filename-less), supporting both `shasum -a 256` and `sha256sum` (codex F21). Add `/build/*.sha256` to `.gitignore` (currently only `*.exe` is ignored — a generated sidecar would break the next clean-tree preflight, iter-2 F14).
- Semver compare: strict `v(\d+)\.(\d+)\.(\d+)` parse of both sides, numeric per-component compare (so `v1.9.0 < v1.10.0`); **if the current version is non-canonical (`dev*`, dirty, hash), update-check still reports latest but auto-apply is disabled** (banner says 手動更新: manual download link) — no arbitrary behavior on malformed versions. Table tests: downgrade, equal, missing components, leading junk, prerelease tag on remote (skipped — `/releases/latest` already excludes prereleases per GitHub API contract, but a malformed `tag_name` must parse-fail → treated as not-newer.
- Repo privacy prerequisite: anonymous Releases API requires the repo (or at least releases) to be public — verify `zytakeshi/amo-koteihyo` visibility during Stage 4; if private, releases must be made public or the plan gains a baked read-only token (owner decision; default = make repo public, it contains no secrets).

## B2. Check & prompt

- **Updater manager** (`internal/update`, new): mutex-protected state `{current, latest, notes, available, phase(idle|checking|downloading|staged|applying), err}`; all API access goes through it; **one apply at a time — concurrent apply → 409** (codex F21).
- **`canApply` requires a writable exe dir** (iter-2 F9): startup probe = create+rename+delete a temp file in `exeDir` (the app already supports read-only exe locations via the data-path fallback, main.go:96 — those installs must not advertise an in-app update). Probe fails → `canApply:false, reason:"manual_required"`, banner shows a manual download link instead of 今すぐ更新.
- Check on startup (goroutine, never blocks startup) + every 24h ticker + lazy on `/api/update/status`. HTTP: `http.Client{Timeout: 8s}` for the API call; separate bounded download client (`Timeout: 120s`); `User-Agent: AMO-koteihyo/<version>`, `Accept: application/vnd.github+json`; response caps: API body 1MB, notes 8KB, sidecar 4KB, **exe 100MiB** — reject `Content-Length > cap` up front AND stream through `io.LimitReader(body, cap+1)` rejecting overflow (iter-2 F8); non-200 → skip. Any failure → silent skip + one console log line (expected external failure, bounded, observable).
- Asset matching: **both** `AMO-koteihyo.exe` and `AMO-koteihyo.exe.sha256` must exist **in the same release**; else treat as no-update.
- `GET /api/update/status` → `{ current, latest, available, notes, phase, canApply }` (`canApply=false` on darwin/dev builds). Frontend: dismissible banner on calendar view — `"新しいバージョン v1.1.0 があります [今すぐ更新] [あとで]"`; confirm dialog warns the app restarts for everyone (~10s). 「あとで」= session dismiss.

## B3. Apply — staged swap with safe ordering (codex F1, F2, F3)

`POST /api/update/apply` (Windows only; darwin/dev → 400 「手動で更新してください」):

1. **Stage (inside the handler)**: download asset → `os.CreateTemp(exeDir, "AMO-koteihyo-*.tmp")` → stream copy → `f.Sync()` + close → verify sha256 (exact 64-hex sidecar match) + `MZ` magic + size > 1MB. Failure → delete tmp, 500 with message, phase reset. Success → phase=`staged`.
2. **Respond, then hand off to main (iter-2 F2, iter-3 F6 wording)**: the apply handler writes+flushes `{ok, restarting:true, newVersion}` (`http.Flusher`), **then signals the buffered apply channel, then returns** (a returned handler can't signal; the buffered send doesn't block, and `Shutdown`'s graceful drain waits for the handler to finish writing). The channel is owned by `main()`. `main` runs `Serve` in a goroutine (`serveCh <- httpServer.Serve(ln)`) and `select`s on `{applyCh, serveCh}`; on apply it calls `httpServer.Shutdown(ctx 5s)`, drains `serveCh`, then performs the swap **synchronously on the main goroutine** — never in a goroutine that dies when `main` returns:
   ```go
   select {
   case plan := <-applyCh:
       _ = httpServer.Shutdown(ctx)
       <-serveCh
       update.SwapAndStart(plan)   // synchronous; only path that exits 0
   case err := <-serveCh:
       // normal server exit/error path
   }
   ```
   b. **Swap state machine (iter-2 F3, iter-3 F6)** in `SwapAndStart` — every rename checked, never restore over an existing destination, every failed branch prints the exact manual-recovery paths and exits nonzero: (i) leftover `.old` exists → delete; **delete failure → abort before touching the live exe** (nothing changed). (ii) `os.Rename(exe → exe.old)`; failure → abort (nothing changed). (iii) `os.Rename(tmp → exe)`; failure → rollback `exe.old → exe`; rollback failure → **delete nothing**, print both paths, exit 1. (iv) `exec.Command(exe).Start()` with env below; **spawn failure** → move new exe aside to a collision-safe `AMO-koteihyo.exe.failed-<ver>-<pid>` (if that rename fails, leave it and say so) → restore `exe.old → exe` **only if the exe name is now free** (if restore fails: delete nothing, print paths, exit 1) → attempt relaunch of the restored old exe (relaunch failure: still exit nonzero with instructions "AMO-koteihyo.exe をダブルクリックして起動し直してください"). Exit 0 only after a successful `Start()`. Renames are back-to-back, no I/O between (residual power-loss risk documented in README: recovery = rename `.old` back).
   c. Spawn env: `AMO_UPDATE_RESTART=1 AMO_EXPECT_PORT=<port> AMO_EXPECT_VERSION=<newVersion>`.
   - Note: renaming a mapped/running exe is legal on NTFS (Go issue #21997 documents the pattern); the listener is already closed by Shutdown, and the rollback ladder covers AV-lock edges. No batch relauncher needed — the exiting process performs the swap itself, serializing port release.
3. **New process (restart mode)**: when `AMO_UPDATE_RESTART=1`, retry `net.Listen` on `AMO_EXPECT_PORT` for up to 10s (500ms steps) and **never fall through to the +1 port fallback** (URL/QR must stay stable; if the port never frees, print a clear error and exit — only possible if a third program grabbed it mid-restart). Normal launches keep the existing +1 fallback (main.go:152). Suppress browser auto-open in restart mode. **Bind-first ordering (iter-4 P2 cross-cutting)**: in restart mode, listen and serve BEFORE firewall validation/repair — the elevated repair can block on an unanswered UAC prompt and would otherwise defeat the 10s-bind/90s-frontend recovery contract (current code runs firewall setup before listen, main.go:46 — restart mode reorders; normal mode may too, it's strictly better: the rule matters to LAN clients, not to binding). Firewall check runs async after serve in both modes.
4. **`.old` cleanup, tightly gated (codex F3 + iter-2 F10)**: `.old` is deleted only when ALL hold: restart mode env present, running `version == AMO_EXPECT_VERSION`, the process bound the port, and the first `/api/bootstrap` returned 200 — guarded by `sync.Once`; deletion failure is logged, retried next update cycle via 2b(i). A normal/manual launch (even of the new exe) never deletes `.old`; a recovered OLD exe (relaunched after rollback) never deletes it either. A crashing new build always leaves `AMO-koteihyo.exe.old` for recovery.
5. **Frontend during restart**: after apply returns, full-screen 「更新中… そのままお待ちください」; poll `/api/bootstrap` every 1s up to 90s; reload **only when `bootstrap.version == newVersion`** (a plain 200 might still be the old process shutting down — codex F2); timeout → 「PCの黒い画面を確認してください」.
6. Firewall interaction: the Windows Firewall rule is program-path-bound; in-place swap keeps the path → rule stays valid (§C verify step).
7. MotW (codex F23): a Go-downloaded file is not expected to carry browser MotW, but SmartScreen / Smart App Control / AV heuristics on a new unsigned hash remain possible — treat as environmental, document in guide.

## B3.5 Data-file compatibility across versions (iter-3 F2 — HIGH)

An old exe (rolled-back `.old`, or a manually copied v1.0.0) loads the new JSON, **drops the unknown fields** (`attendance`, `attendanceRev`, `tcConfig`) on its next save, and silently destroys payroll data. Mitigations (all included):
1. **Pre-upgrade backup** (iter-4 P1 mechanics): detection happens on the **raw bytes before `normalize()`** — unmarshal into `map[string]json.RawMessage` and check for the `attendance` key (after normalize, "absent" and "empty" are indistinguishable, store.go:53). If absent and this build has timecard support: write the **original file bytes** to `<dataDir>/backup/koteihyo.pre-v1.1.0.json` via temp → write → sync → close → rename → dir-sync (same atomic pattern as `save()`); **never overwrite an existing pre-v1.1.0 backup**; **failure aborts startup with a clear console error** (a silent skip would defeat the only downgrade safety net). Backup dir anchored at `filepath.Dir(dataPath)` (respects the AppData fallback).
2. **Rolling backups**: on the first save of each calendar day, copy the current file to `<dataDir>/backup/koteihyo-YYYYMMDD.json` (same atomic pattern); **prune older-than-newest-14 only after today's backup has been committed**. Rolling-backup failure is logged, non-fatal (unlike the pre-upgrade one).
3. **README/guide**: rollback instructions say explicitly — after reverting to `.old`/an older exe, restore the matching backup from `data/backup/`; running an old exe over new data loses timecard entries.
4. **Stated plainly in the plan and release notes**: v1.0.0 has no updater, so the v1.0.0→v1.1.0 hop is one final manual exe replacement; auto-update begins for v1.1.0→future.

## B4. Release checklist (README)

tag `vX.Y.Z` (clean tree) → `bash build/build-windows.sh` → verify it printed the tag as version → GitHub release with **both** assets (`AMO-koteihyo.exe`, `AMO-koteihyo.exe.sha256`) → verify `/releases/latest` shows it.

---

# C. Windows Defender / Firewall(格上げ: 現行コードに実バグ)

## C1. Diagnosis (codex verdict, folded)

Since the app runs and listens fine locally and disabling the firewall fixed LAN access, ranked causes:
1. **Rule never installed or installed broken, while the marker file says OK** — the current code has three real bugs (main.go:299-333):
   - Marker existence short-circuits every check (main.go:303) — written once, never re-validated; a same-**name** rule is accepted without validating program path / enabled / action / profile (main.go:311).
   - The elevated launch lacks `-PassThru -Wait` on `Start-Process`, so Go observes PowerShell's own exit code, **not** netsh's — a declined UAC or failed netsh can still write the marker (main.go:328).
   - `Start-Process -ArgumentList` re-joins/unquotes arguments — an exe path containing spaces can split and produce a rule bound to a wrong path.
   - The fallback `build/ファイアウォール許可.bat` always prints `[OK]` regardless of errorlevel (BAT:22).
2. Stale rule after the exe was moved/renamed (path-bound rule + location-blind marker).
3. Profile-level hard block (`blockinboundalways` / explicit block rule / group-policy no-local-rule-merge).
4. Network category Public alone is NOT the cause (`profile=any` covers it) unless combined with "block all incoming" toggles.
5. SmartScreen/Defender AV: unlikely for this symptom (they block execution, not only LAN packets) — keep as a separate troubleshooting track in the guide.

## C2. Fix (Stage 5)

- **Unique, versioned rule name**: `AMO-koteihyo v2 <hash8>` where `hash8` = first 8 hex of SHA-256 over the lowercase canonical absolute exe path (UTF-8; canonicalize via `filepath.Clean` + `filepath.EvalSymlinks` best-effort, same algorithm everywhere incl. the .bat) — makes stale-path rules detectable and old-name rules safely deletable (iter-2 F11).
- **Live validation every launch — locale-independent (iter-2 F4, iter-3 F3)**: `netsh` output is localized (target machine is Japanese Windows) — never parse it. Validate via non-elevated PowerShell **structured objects**, matching on enum/property values, requiring **exactly one** matching rule:
  ```powershell
  $r = @(Get-NetFirewallRule -DisplayName $env:AMO_FW_RULE -ErrorAction SilentlyContinue)
  if ($r.Count -ne 1) { exit 2 }
  $app   = @(($r[0] | Get-NetFirewallApplicationFilter).Program)
  $addr  = @(($r[0] | Get-NetFirewallAddressFilter).RemoteAddress)
  $proto = @(($r[0] | Get-NetFirewallPortFilter).Protocol)
  $ok = $r[0].Enabled -eq 'True' -and $r[0].Direction -eq 'Inbound' -and $r[0].Action -eq 'Allow' `
    -and [string]$r[0].Profile -eq 'Any' `
    -and $app.Count -eq 1 -and $app[0] -eq $env:AMO_FW_EXE `
    -and $addr.Count -eq 1 -and $addr[0] -eq 'LocalSubnet' `
    -and $proto.Count -eq 1 -and $proto[0] -eq 'TCP'
  if ($ok) { exit 0 } else { exit 3 }
  ```
  Rule name and exe path are passed via **environment variables** (`AMO_FW_RULE`, `AMO_FW_EXE` on the `exec.Cmd`) — never string-interpolated into the script (iter-4 P1). `Profile` must equal `Any` (a Private-only same-name rule must fail validation); array-valued filters require exactly one element. Go runs `powershell -NoProfile -NonInteractive -Command <fixed script>` and branches on exit code. Any nonzero → treat as not-installed → elevated repair. The marker never skips this live check — it exists only so the banner can explain state; UAC is prompted at most once per launch.
- **There is exactly ONE installer path** (iter-4 P1): the elevated structured-cmdlet transaction below. No netsh-based add anywhere (netsh remains only as the thing the legacy code used; it is removed).
- **One elevated transaction (iter-2 F11, iter-3 F3/F4)**: the elevated PowerShell script performs, in order: (1) **remove any existing rule with the exact current rule name** (prevents duplicate same-name rules when repairing a mismatched one); (2) remove legacy rules — exact name `AMO-koteihyo` or matching `^AMO-koteihyo v2 [0-9a-f]{8}$` with hash ≠ current, never anything else; (3) `New-NetFirewallRule -DisplayName $env:AMO_FW_RULE -Direction Inbound -Action Allow -Enabled True -Profile Any -Protocol TCP -RemoteAddress LocalSubnet -Program $env:AMO_FW_EXE` (structured cmdlet, not netsh — locale-safe). Parameter transport is **env-var only, mandated** (iter-5 P2): the fixed script reads `$env:AMO_FW_RULE`/`$env:AMO_FW_EXE`; it is launched elevated via `Start-Process powershell -Verb RunAs -Wait -PassThru -ArgumentList @('-NoProfile','-NonInteractive','-EncodedCommand',$encoded)` where `$encoded` = Base64/Unicode of that fixed script; the elevated child inherits the parent's env. No value is ever interpolated into script text; (4) re-run the structured validation from above; exit code = validation result (legacy-cleanup failures logged, non-fatal). Only after exit 0 AND a post-hoc non-elevated re-verify does Go write marker JSON `{schema:2, ruleName, exePath}` atomically.
- UAC declined / add failed → no marker, banner keeps showing the manual `.bat` hint; retry next launch (at most one UAC prompt per launch).
- Fix the `.bat` to install the **identical rule** (iter-3 F4 — no separate "manual" name, otherwise live validation would never accept it and UAC nags forever): the .bat derives the same `hash8` via an embedded PowerShell one-liner over the same canonicalized `%~dp0AMO-koteihyo.exe` path and calls the same `New-NetFirewallRule` parameters; `errorlevel` checked after each step; `[OK]` only on success. App-side validation therefore accepts app-installed and .bat-installed rules identically.
- 使い方ガイド: 「ファイアウォールを再有効化する手順」 — re-enable firewall → run the fixed .bat as admin (or launch app once and accept UAC) → confirm with the shown URL from the iPad. Troubleshooting box: check `blockinboundalways`, explicit block rules, policy merge; SmartScreen 「詳細情報→実行」 note kept separate. **Never advise disabling the firewall or AV.**
- Long-term (out of scope, owner call): code-signing cert.

---

# D. Execution plan (/build-loop stages)

Branch `feat/timecard-updater`, commit per stage. Version target: `v1.1.0`.

| Stage | Content | Files |
|---|---|---|
| 1 | Store: types (`DayAttendance`/`BreakSpan`/`TCConfig`), normalize additions, `tc_set`/`tc_config_set` + clone/decoders + error-returning apply, punch state machine incl. carryover, rev counters, month computation (raw/ref/overStd), anomalies incl. invalid-record surfacing, CSV data, **unit tests** (state machine incl. carryover+clock-warp, rounding boundary table, undo/redo/revert round-trips, rev semantics, validation matrix A10) | `internal/store/*` |
| 2 | API: 6 timecard endpoints (+409 paths), bootstrap addition `tcConfig` (bootstrap `version` wiring moves to Stage 4 with the version var), `httptest` coverage | `internal/api/api.go` |
| 3 | Frontend: timecard view (打刻/月次 incl. dirty-guard, clock lifecycle, 409 flow), settings panel, topbar 5-button CSS fix, calendar-dot allowlist addition, polling, print CSS | `web/*` |
| 4 | Updater: version stamping + strict build script (+sha256 sidecar, gitignore), `internal/update` manager, `/api/update/*`, staged swap + restart mode + `.old` health-gated cleanup, pre-upgrade + rolling DB backups (§B3.5), bootstrap `version`, frontend banner/restart flow; verify repo/release visibility | `main.go`, `internal/update/`, `internal/store/`, `internal/api/api.go`, `web/*`, `build/build-windows.sh`, `.gitignore` |
| 5 | Firewall §C2 (rule-name scheme, encoded elevation, verify-then-mark, .bat fix) + guide updates (タイムカード章, 更新章, FW再有効化手順) | `main.go`, `build/*.bat`, `docs/*`, `README.md` |
| 6 | Verify: `go vet` + `go test ./...` + mac smoke (punch flows incl. carryover simulation via data edit, monthly, CSV, undo/redo/revert, 409). Windows-only paths (swap, PowerShell firewall cmdlets/UAC) reviewed + dry-run flag; real-device verification stays △ until run on the client PC | — |

Status ledger during build: ☐ per stage; △ implemented; ○ only on verified runs (Windows items stay △ until client-PC round).

## D1. Out of scope (explicitly)

- Auth/roles — LAN-trust model unchanged; audit log is the control.
- 有給/シフト管理/給与計算 — the timecard records facts; payroll math beyond 実働/所定超過(参考) stays in the spreadsheet.
- Statutory 残業 classification (weekly limits, holidays) — F15: overStd is informational only, labeled 参考.
- Overnight shifts as first-class data (§A2 carryover+clamp+flag is the model).
- Print-all-staff pagination (F20) — deferred.
- Code-signing cert (§C, owner cost decision).
