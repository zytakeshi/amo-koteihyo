package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 旧フォーマット（attendance 列なし）を読むと、正規化前の生バイトが一度だけ
// 退避される（§B3.5-1）。
func TestPreUpgradeBackupCreated(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "koteihyo.json")
	old := []byte(`{"version":1,"staff":[{"id":"s_1","name":"小池","order":0,"active":true}],"processes":[],"counts":{"s_1|2026-07-01|p_1":3},"memos":{},"events":[],"redo":[],"seq":1}`)
	if err := os.WriteFile(dataPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	backup := filepath.Join(dir, "backup", preUpgradeBackupName)
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("pre-upgrade backup missing: %v", err)
	}
	if !bytes.Equal(got, old) {
		t.Fatalf("backup should be original bytes verbatim")
	}
}

// 既存のアップグレード前バックアップは決して上書きしない（§B3.5-1）。
func TestPreUpgradeBackupNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "koteihyo.json")
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte(`{"sentinel":true}`)
	if err := os.WriteFile(filepath.Join(dir, "backup", preUpgradeBackupName), sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"version":1,"staff":[],"processes":[],"counts":{},"memos":{}}`)
	if err := os.WriteFile(dataPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "backup", preUpgradeBackupName))
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("existing pre-upgrade backup was overwritten")
	}
}

// タイムカード対応（attendance 列あり）データでは退避しない。
func TestPreUpgradeBackupSkippedWhenPresent(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "koteihyo.json")
	newFmt := []byte(`{"version":1,"staff":[],"processes":[],"counts":{},"memos":{},"attendance":{},"attendanceRev":{},"tcConfig":{"roundUnit":1,"roundDir":"floor","standardMinutes":480},"events":[],"redo":[]}`)
	if err := os.WriteFile(dataPath, newFmt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup", preUpgradeBackupName)); !os.IsNotExist(err) {
		t.Fatalf("pre-upgrade backup should not be created when attendance present")
	}
}

// 新規（seed）ストアの最初の save で当日のローリングバックアップが作られる（§B3.5-2）。
func TestRollingBackupCreatedOnSave(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "koteihyo.json")
	if _, err := Open(dataPath); err != nil { // seed → save
		t.Fatalf("Open: %v", err)
	}
	today := time.Now().In(JST).Format("20060102")
	if _, err := os.Stat(filepath.Join(dir, "backup", "koteihyo-"+today+".json")); err != nil {
		t.Fatalf("rolling backup for today missing: %v", err)
	}
}

func TestIsRollingBackupName(t *testing.T) {
	ok := []string{"koteihyo-20260718.json", "koteihyo-19991231.json"}
	no := []string{"koteihyo.pre-v1.1.0.json", "koteihyo.json", "koteihyo-2026071.json", "koteihyo-2026071x.json", "other-20260718.json"}
	for _, n := range ok {
		if !isRollingBackupName(n) {
			t.Errorf("%q should be a rolling backup name", n)
		}
	}
	for _, n := range no {
		if isRollingBackupName(n) {
			t.Errorf("%q should NOT be a rolling backup name", n)
		}
	}
}

// 剪定は新しい 14 件だけ残し、アップグレード前バックアップには触れない。
func TestPruneRollingBackups(t *testing.T) {
	dir := t.TempDir()
	// 20 日分 + pre-upgrade + 無関係ファイル。
	for i := 1; i <= 20; i++ {
		name := "koteihyo-202607" + twoDigit(i) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, preUpgradeBackupName), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(dir, "koteihyo.json"), []byte("keep"), 0o644)

	pruneRollingBackups(dir, 14)

	entries, _ := os.ReadDir(dir)
	rolling := 0
	for _, e := range entries {
		if isRollingBackupName(e.Name()) {
			rolling++
		}
	}
	if rolling != 14 {
		t.Fatalf("want 14 rolling backups kept, got %d", rolling)
	}
	// 新しい方（07..20）が残る: 最古の koteihyo-20260701 は消えているはず。
	if _, err := os.Stat(filepath.Join(dir, "koteihyo-20260701.json")); !os.IsNotExist(err) {
		t.Fatalf("oldest rolling backup should be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "koteihyo-20260720.json")); err != nil {
		t.Fatalf("newest rolling backup should be kept")
	}
	// 別名ファイルは無傷。
	if _, err := os.Stat(filepath.Join(dir, preUpgradeBackupName)); err != nil {
		t.Fatalf("pre-upgrade backup must survive prune")
	}
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
