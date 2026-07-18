package store

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// preUpgradeBackupName はダウングレード安全網の固定名（§B3.5-1）。
const preUpgradeBackupName = "koteihyo.pre-v1.1.0.json"

// rollingBackupKeep は日次バックアップの保持数（§B3.5-2）。
const rollingBackupKeep = 14

// backupDir は data ファイルと同階層の backup/ を返す（AppData フォールバックも尊重）。
func backupDir(dataPath string) string {
	return filepath.Join(filepath.Dir(dataPath), "backup")
}

// maybePreUpgradeBackup は「タイムカード対応ビルドが、タイムカード列を持たない
// 古い JSON を初めて読んだ」場合に、正規化前の生バイトを一度だけ退避する（§B3.5-1）。
//
// 検出は normalize 前の生バイトで行う（normalize 後は「欠落」と「空」が区別でき
// ないため）。map[string]json.RawMessage に解いて "attendance" キーの有無を見る。
// 既存のバックアップは決して上書きしない。失敗は起動中止（唯一のダウングレード
// 安全網なので沈黙スキップは不可）。
func maybePreUpgradeBackup(dataPath string, raw []byte) error {
	var top map[string]json.RawMessage
	// 解析できない（壊れた）ファイルでも attendance 無し扱いで退避しておく方が安全。
	_ = json.Unmarshal(raw, &top)
	if _, ok := top["attendance"]; ok {
		return nil // 既にタイムカード対応のデータ → 退避不要。
	}
	dir := backupDir(dataPath)
	dest := filepath.Join(dir, preUpgradeBackupName)
	if _, err := os.Stat(dest); err == nil {
		return nil // 既存のアップグレード前バックアップは上書きしない。
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("アップグレード前バックアップの確認に失敗: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("バックアップフォルダ作成失敗: %w", err)
	}
	if err := atomicWriteBytes(dest, raw); err != nil {
		return fmt.Errorf("アップグレード前バックアップの書き込み失敗: %w", err)
	}
	log.Printf("アップグレード前バックアップを作成しました: %s", dest)
	return nil
}

// rollingBackup は「その暦日で最初の save」のときに、保存済みバイトを
// backup/koteihyo-YYYYMMDD.json へ複製する（§B3.5-2）。今日の分をコミットして
// から 14 世代より古いものを剪定する。失敗は記録のみ（非致命）。
func (s *Store) rollingBackup(data []byte) {
	today := time.Now().In(JST).Format("20060102")
	if s.lastRollingDate == today {
		return // 本日分は処理済み（同日の 2 回目以降を stat せず抜ける）。
	}
	dir := backupDir(s.path)
	dest := filepath.Join(dir, "koteihyo-"+today+".json")
	if _, err := os.Stat(dest); err == nil {
		// 既に本日分がある（プロセス再起動後など）。剪定だけ確認して終える。
		s.lastRollingDate = today
		pruneRollingBackups(dir, rollingBackupKeep)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("日次バックアップフォルダの作成に失敗しました（続行）: %v", err)
		return
	}
	if err := atomicWriteBytes(dest, data); err != nil {
		log.Printf("日次バックアップの作成に失敗しました（続行）: %v", err)
		return
	}
	s.lastRollingDate = today
	// 本日分をコミットしてから剪定する（順序重要、§B3.5-2）。
	pruneRollingBackups(dir, rollingBackupKeep)
}

// pruneRollingBackups は koteihyo-YYYYMMDD.json のうち新しい keep 件だけ残す。
// アップグレード前バックアップ（別名）は対象外。
func pruneRollingBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var dated []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isRollingBackupName(e.Name()) {
			dated = append(dated, e.Name())
		}
	}
	if len(dated) <= keep {
		return
	}
	// 名前は koteihyo-YYYYMMDD.json なので辞書順＝日付順。新しい順に並べ替え。
	sort.Sort(sort.Reverse(sort.StringSlice(dated)))
	for _, name := range dated[keep:] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("古い日次バックアップの削除に失敗しました（続行）: %v", err)
		}
	}
}

// isRollingBackupName は "koteihyo-YYYYMMDD.json" 形式かを判定する。
func isRollingBackupName(name string) bool {
	const prefix = "koteihyo-"
	const suffix = ".json"
	if len(name) != len(prefix)+8+len(suffix) {
		return false
	}
	if name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return false
	}
	for _, r := range name[len(prefix) : len(prefix)+8] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// atomicWriteBytes は temp → write → sync → close → rename → dir-sync で原子的に
// 書き込む（save() と同じ耐久パターン）。バックアップ書き込み専用のヘルパ。
func atomicWriteBytes(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
