package main

import (
	"os"
	"path/filepath"
	"testing"
)

// UpdateAccount 的返回值只回答「账号是否存在」：落盘失败已经记日志，
// 不能变成 false 让调用方误判成账号不存在。
func TestUpdateAccountReturnSemantics(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	acc := &Account{ID: "u1", Label: "u1", Enabled: true, Models: map[string]*UsageTotals{}}
	if err := store.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	if !store.UpdateAccount("u1", func(a *Account) { a.Label = "renamed" }) {
		t.Fatal("existing account should report true")
	}
	if got := store.Accounts()[0].Label; got != "renamed" {
		t.Fatalf("mutation lost: %q", got)
	}
	if store.UpdateAccount("missing", func(a *Account) {}) {
		t.Fatal("missing account should report false")
	}
}

// 落盘失败时设置必须回滚：否则界面显示改成功、进程按新值跑，重启后又变回
// 旧值，用户会以为设置随机丢失。
func TestUpdateSettingsRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// 把落盘用的 tmp 路径占成目录，WriteFile 必定失败（root 也绕不过）。
	if err := os.Mkdir(filepath.Join(dir, "accounts.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := store.Settings().ProxyToken
	store.UpdateSettings(func(st *Settings) { st.ProxyToken = "changed" })
	if got := store.Settings().ProxyToken; got != before {
		t.Fatalf("settings should roll back on save failure, got %q want %q", got, before)
	}
}

// 正常路径要真的写进 accounts.json：重开 store 能读回来。
func TestUpdateSettingsPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateSettings(func(st *Settings) { st.ProxyToken = "tok-x" })
	if got := store.Settings().ProxyToken; got != "tok-x" {
		t.Fatalf("settings not applied in memory: %q", got)
	}
	_ = store.Close()
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.Settings().ProxyToken; got != "tok-x" {
		t.Fatalf("settings not persisted: %q", got)
	}
}
