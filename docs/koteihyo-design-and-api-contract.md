# AMO BARBER 工程表 — 設計 & API 契約書 (v1)

このドキュメントは実装の唯一の正（source of truth）。バックエンド／フロントエンド／使い方ガイドの各担当はこれに従う。

## 0. ゴール / 制約

- **AMO BARBER 用の社内「工程表」アプリ**。今は紙（週間グリッド）で集計している。
- Windows 11 PC で **ダブルクリック起動の単体 .exe**。データはその PC のファイルに保存（外部サーバー無し）。
- 同一 LAN 上の **iPad / iPhone / 他のPCのブラウザ**から `http://<PCのIP>:<port>` で同じデータにアクセス・操作できる。
- **セキュリティは考慮不要**（社内・LAN内のみ）。
- ボタンは大きく、タッチで使いやすく。バーバーのスタッフが迷わず使える。
- **技術: Go 標準ライブラリのみ（外部モジュール禁止）＋ JSON ファイル永続化。** フロントは素の HTML/CSS/JS（ビルド不要、フレームワーク無し）。
- 既存 `BarberAmo/` デモと同じ和モダン・紙質感のデザイン言語を踏襲する。

## 1. 紙の工程表（再現対象）

- 行 = **工程**（単価つき）。初期データ:
  - シャンプー ¥600 / シェービング ¥600 / ヘッドスパ ¥2500 / 肩マッサージ ¥400 / パック ¥500 / ピーリング ¥500 / 造顔マッサージ ¥2500
- 列 = **曜日＋日付**（月〜日の7日）＝ **週間ビュー**。
- セル = その工程をその日に行った **件数**（整数）。＋/− ボタンで増減。
- 下部に **備考欄**。

## 2. 主要要件（必須）

1. **カレンダー形式**で日付を選ぶ → その週の工程×曜日グリッドを表示・入力。
2. **＋ / −（削除）ボタン**でセルの件数を増減。ボタンは大きい。
3. **戻るボタン**＝多段 **undo**（何回でも遡れる）＋ **redo**（やり直し）。
4. **操作履歴を全部保存**（append-only）。再起動しても消えない。**日付で履歴を管理・閲覧・その時点まで復元**できる。
5. **スタッフごとに合計**を計算（件数・売上＝単価×件数）。全体合計も。
6. **工程名・単価を編集**、工程の追加／削除、スタッフの追加／削除。
7. iPad/iPhone/Windows ブラウザすべてで操作可能。

## 3. 便利機能（実装する）

- 起動時に **接続用URL＋QRコード**（iPad/iPhoneはQRを読むだけ）。
- **自動保存**（操作の都度ファイル保存。明示保存ボタン不要）。
- **CSV 書き出し** / **印刷**（紙の代わり）。
- 週ナビ（◀前の週 / 今週 / 次の週▶）、月カレンダー。
- 合計: 工程別・曜日別・スタッフ別・全体。
- 削除はソフト削除（履歴を壊さない）。

## 4. データモデル（JSON: `<dataDir>/koteihyo.json`）

`dataDir` = exe と同じフォルダの `data/`（無ければ作成）。保存は **一時ファイルに書いて rename（atomic）**、全アクセスは単一 `sync.Mutex` で直列化。

```jsonc
{
  "version": 1,
  "staff":     [ { "id": "s_xxx", "name": "田中", "order": 0, "active": true } ],
  "processes": [ { "id": "p_xxx", "name": "シャンプー", "price": 600, "order": 0, "active": true } ],
  // 現在値（派生だがキャッシュ保存）。キー = "<staffId>|<YYYY-MM-DD>|<processId>"
  "counts":    { "s_a|2026-06-08|p_a": 3 },
  // 備考。キー = "<staffId>|<YYYY-MM-DD>"（スタッフ×日ごと）
  "memos":     { "s_a|2026-06-08": "予約集中" },
  // append-only 監査ログ（絶対に削除しない＝「履歴ぜんぶ保存」）
  "events": [ {
      "id": "e_000123",
      "ts": "2026-06-08T10:30:00+09:00",   // 操作時刻（サーバ時計）
      "bizDate": "2026-06-08",              // 対象営業日（履歴の日付管理キー）。設定変更は操作日。
      "staffId": "s_a",                     // 対象スタッフ（設定変更は "" 可）
      "kind": "count_inc",                  // 種別（下記）
      "processId": "p_a",                   // 該当時のみ
      "prev": 2, "next": 3,                 // 変更前後（数値/文字列/JSON）
      "label": "シャンプー +1"               // 人が読める要約（履歴表示用）
  } ],
  "redo": [ /* events と同じ形。undo で取り消したものを redo 用に退避 */ ],
  "seq": 123                                 // id 採番用カウンタ
}
```

- **kind 一覧**: `count_inc` `count_dec` `count_set` `memo_set` `process_add` `process_rename` `price_set` `process_delete` `process_reorder` `staff_add` `staff_rename` `staff_delete` `staff_reorder` `undo` `redo` `revert`。
- **undo/redo モデル**:
  - 通常操作 = `counts`/`memos`/`processes`/`staff` を更新し、対応イベントを `events` に append、`redo` をクリア。
  - **undo** = `events` 内の「まだ取り消されていない最後の実操作」を逆適用し、それを `redo` へ移動。さらに `kind:"undo"` の監査イベントを `events` に append（履歴には“戻した”ことも残る＝全保存）。
  - **redo** = `redo` の先頭を再適用し、`kind:"redo"` を append。
  - **revert（ここまで戻す）** = 指定 event 以降の実操作を新しい順に undo していく（複数 undo の糖衣）。
- **件数は 0 未満にしない**（dec の下限 0）。
- **ソフト削除**: `active=false`。集計・グリッドからは除外、履歴は保持。

## 5. 集計（売上・件数）

- セル件数 `c`、工程単価 `price` →
  - 工程別合計(件数) = Σ_日 c ; 工程別売上 = Σ_日 c×price
  - 曜日別(日別)合計 = Σ_工程 c ; 日別売上 = Σ_工程 c×price
  - **スタッフ別合計** = そのスタッフの週内 Σ c（件数）, Σ c×price（売上）
  - 全体合計 = Σ_スタッフ
- 週ビューの集計はフロントが `/api/week` の結果から計算してよい。任意期間/月/CSV はサーバ `/api/totals`・`/api/export.csv` を使う。

## 6. REST API（全て JSON, 認証なし, `Content-Type: application/json`）

エラーは HTTP 4xx/5xx ＋ `{ "error": "message" }`。日付は全て `YYYY-MM-DD`（JST 基準）。週の起点は **月曜**。

| Method | Path | Body / Query | 返り値 |
|---|---|---|---|
| GET | `/api/bootstrap` | — | `{ staff[], processes[], today, weekStart, port, lanURL }` |
| GET | `/api/week` | `?start=YYYY-MM-DD&staffId=ID` | `{ start, dates[7], staffId, counts: { "<processId>": [7 ints] }, memos: [7 strings], canUndo, canRedo }` |
| POST | `/api/count` | `{ staffId, date, processId, op:"inc"|"dec"|"set", value? }` | `{ value, canUndo, canRedo }` |
| POST | `/api/memo` | `{ staffId, date, text }` | `{ ok:true, canUndo, canRedo }` |
| POST | `/api/undo` | — | `{ applied:bool, label?, bizDate?, staffId?, canUndo, canRedo }` |
| POST | `/api/redo` | — | `{ applied:bool, label?, canUndo, canRedo }` |
| GET | `/api/history` | `?date=YYYY-MM-DD`（省略=最新200件） | `{ days: [ { date, events: [ {id, ts, label, kind, staffName} ] } ] }` (新しい順) |
| POST | `/api/history/revert` | `{ eventId }` | `{ revertedCount, canUndo, canRedo }` |
| GET | `/api/totals` | `?start=&end=&staffId?` | `{ perStaff:[{staffId,name,count,sales}], perProcess:[{processId,name,count,sales}], grand:{count,sales} }` |
| GET | `/api/export.csv` | `?start=&end=` | `text/csv`（ダウンロード, UTF-8 BOM 付, ヘッダ日本語） |
| POST | `/api/process` | `{ id?, name, price }` | `{ process }`（id 無=追加, 有=更新） |
| POST | `/api/process/delete` | `{ id }` | `{ ok:true }`（ソフト削除） |
| POST | `/api/process/reorder` | `{ ids:[...] }` | `{ ok:true }` |
| POST | `/api/staff` | `{ id?, name }` | `{ staff }` |
| POST | `/api/staff/delete` | `{ id }` | `{ ok:true }` |
| POST | `/api/staff/reorder` | `{ ids:[...] }` | `{ ok:true }` |

- ルートとアセット: `GET /` → `index.html`、`/styles.css` `/app.js` `/qrcode.min.js` を配信（`go:embed web/*`）。
- すべての変更系 API は応答に `canUndo` `canRedo` を含め、フロントが戻る/やり直しボタンの活性を更新できるようにする。

## 7. サーバ起動仕様（main.go）

- ポート: 既定 `8080`、使用中なら +1 して空きを探す。
- `0.0.0.0` で listen（LAN 公開）。LAN IP は `net.Interfaces()` から最初の非ループバック IPv4 を選ぶ。
- 起動時:
  1. 既定ブラウザを `http://localhost:<port>/` で自動オープン（Windows: `rundll32 url.dll,FileProtocolHandler` か `cmd /c start`。mac: `open`）。
  2. **コンソールに大きく**接続情報を表示（日本語）:
     ```
     ============================================
       AMO BARBER 工程表 が起動しました
       このウィンドウは閉じないでください
       iPad / iPhone は下のURLかQRコードでどうぞ
         →  http://192.168.x.x:<port>
     ============================================
     ```
- `web/index.html` 内の「接続」パネルが `lanURL` を表示し、`qrcode.min.js` で QR を描画（クライアント側生成・オフライン可）。
- Windows ビルドは **コンソール表示あり**（`-H windowsgui` は付けない）＝サーバ稼働とURLが見えるように。

## 8. 画面（フロント, 1ページSPA）

`BarberAmo/app.js` と同じ流儀: `state` オブジェクト＋`render()` で `#app.innerHTML` を差し替え、イベント委譲。`fetch` で上記 API を叩く。画面（ビュー）:

1. **calendar（ホーム）**: 月カレンダー。今日を強調。日付タップ→その週の grid へ（`state.weekStart` を含む週）。前月/次月送り。各日に「入力あり/履歴あり」ドット。上部にロゴ＋「接続」「履歴」「設定」へのボタン。
2. **grid（入力・メイン）**: 上部に **← 戻る(カレンダーへ)**、**週ナビ ◀ M/D〜M/D ▶**、**スタッフ切替タブ**（大きい）、**⟲ 元に戻す / ⟳ やり直し**。本体は 工程(行)×曜日(列)。各セルに大きい `［−］ [数] ［＋］`。各曜日列ヘッダに日付。最下部 **備考欄**（スタッフ×日、選択日のメモ編集）。下に **合計バー**: 「<スタッフ>合計 N件 / ¥xxx」＋「全体 N件 / ¥xxx」。工程別行合計・曜日別列合計も表示。
3. **history（履歴）**: 日付ごとにグループ表示（新しい順）。各操作の時刻・要約・スタッフ。日付フィルタ。各行に「ここまで戻す」。上部に大きい ⟲元に戻す/⟳やり直し も。
4. **settings（設定）**: 工程の一覧（名前編集・単価編集・並べ替え・追加・削除）、スタッフの一覧（追加・名前編集・削除・並べ替え）。CSV書き出し・印刷ボタン。
5. **connect（接続/起動案内）**: lanURL を大きく＋QRコード＋「iPad/iPhoneでこのURLを開いてね」。calendar から開けるダイアログでも可。

- 戻るボタンは2種類を明確に: **画面の「← 戻る」**(=前の画面へ) と **操作の「⟲ 元に戻す」**(=undo)。ラベルで区別（混同させない）。
- 触りやすさ: タップ領域 ≥ 56px、＋/− ボタンは ≥ 56×56px、数字は大きく。iPad 横/縦・iPhone 縦で破綻しないレスポンシブ。長押しでの連続増減は任意（あれば尚良）。

## 9. デザイン言語（BarberAmo から踏襲）

- フォント: 見出し `Noto Serif JP`(700/900)、本文 `Zen Kaku Gothic New`。Google Fonts を `index.html` で読込（オフライン時はシステムfallback: Hiragino/Meiryo/Yu Mincho）。
- カラートークン（CSS `:root`、oklch）:
  - `--paper: oklch(96% 0.018 82)` 背景 / `--paper-2: oklch(92% 0.028 74)`
  - `--ink: oklch(25% 0.018 52)` 文字・濃色 / `--muted: oklch(48% 0.025 56)`
  - `--line: oklch(83% 0.028 70)` 罫線
  - `--rose: oklch(62% 0.17 11)` 差し色（＋/選択） / `--rose-deep: oklch(44% 0.14 12)`
  - `--green: oklch(62% 0.11 155)`（合計/プラス系）, `--white: oklch(99% 0.006 82)`
  - `--radius: 8px`、紙の方眼背景（`linear-gradient` 72px グリッド）を踏襲。
- ボタン: 角丸ピル/8px、`--ink` 背景＝主操作、ローズ＝＋や強調、淡色＝副操作。`min-height` を大きめに（タッチ）。
- 雰囲気: 落ち着いた和モダン・紙質感。装飾過多にしない。視認性最優先。

## 10. ファイル構成 / 担当境界

```
koteihyo/
  go.mod                         (module koteihyo / go 1.26 / 標準ライブラリのみ)
  main.go                        [Backend] 起動・ポート探索・LAN IP・ブラウザ起動・コンソール表示・embed・ルーティング
  internal/store/model.go        [Backend] 型定義
  internal/store/store.go        [Backend] JSON load/save・mutex・atomic write・events/undo/redo/revert・totals・seed初期データ
  internal/api/api.go            [Backend] http ハンドラ群（§6 全エンドポイント）・CSV
  web/index.html                 [Frontend] マークアップ土台・フォント・スクリプト読込
  web/styles.css                 [Frontend] §9 デザイン・全画面のスタイル・レスポンシブ
  web/app.js                     [Frontend] SPA(state/render/イベント委譲)・API client・全画面
  web/qrcode.min.js              [Frontend] MIT の QR 生成ライブラリ（kazuhikoarase/qrcode-generator 系の最小実装を同梱）
  docs/                          使い方ガイド等
  build/build-windows.sh         クロスコンパイル: GOOS=windows GOARCH=amd64 go build -o build/AMO工程表.exe .
  README.md
```

- **初期シードデータ**: データファイルが無い初回起動時、§1 の7工程＋単価を投入。スタッフは **小池・井上・浜田** の3名を用意（施主指定。設定で改名/追加/削除可）。
- 並行作業の境界: Backend=`main.go`/`internal/**`/`go.mod`/`build/`、Frontend=`web/**`。API契約(§6)とembedパス(`web/index.html` 等)が両者の唯一の結合点。
