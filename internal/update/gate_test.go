package update

import (
	"context"
	"testing"
	"time"
)

// TestFirewallGate_BusyWhileRepairHeld は「修復がゲートを保持している間、適用は
// bounded wait で確保できず busy になる」ことを直接検証する（finding 2）。
func TestFirewallGate_BusyWhileRepairHeld(t *testing.T) {
	m := New("v1.0.0", "owner/repo", "/tmp/AMO-koteihyo.exe", make(chan Plan, 1), 8080)
	ctx := context.Background()

	// 修復がゲートを取得（保持）。
	if !m.TryAcquireFirewallGate(ctx, 0) {
		t.Fatal("初回のゲート取得に失敗した")
	}
	// 保持中は bounded wait でも取得できない → 適用ハンドラは busy を返せる。
	if m.TryAcquireFirewallGate(ctx, 50*time.Millisecond) {
		t.Fatal("保持中にゲートを取得できてしまった（busy にならない）")
	}
	// 解放後は再取得できる。
	m.ReleaseFirewallGate()
	if !m.TryAcquireFirewallGate(ctx, 0) {
		t.Fatal("解放後にゲートを取得できない")
	}
	m.ReleaseFirewallGate()
}

// TestFirewallGate_CtxCancelAborts はキャンセル済み ctx で bounded wait が即座に
// 失敗することを確認する（シャットダウン中に修復/適用を張り付かせない）。
func TestFirewallGate_CtxCancelAborts(t *testing.T) {
	m := New("v1.0.0", "owner/repo", "/tmp/AMO-koteihyo.exe", make(chan Plan, 1), 8080)
	if !m.TryAcquireFirewallGate(context.Background(), 0) {
		t.Fatal("初回のゲート取得に失敗した")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.TryAcquireFirewallGate(ctx, 2*time.Second) {
		t.Fatal("キャンセル済み ctx で取得できてしまった")
	}
	m.ReleaseFirewallGate()
}
