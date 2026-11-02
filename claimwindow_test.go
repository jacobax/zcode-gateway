package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 老文件没有 claimWindow 字段时，要补上默认的 19,20；
// 而显式写成 "" 的（用户故意设成不限时段）必须保留成空。
func TestClaimWindowDefaultBackfill(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
	}{
		{"老文件无字段", `{"version":1,"settings":{"quotaIntervalSec":30},"accounts":[]}`, "19,20"},
		{"显式空=不限", `{"version":1,"settings":{"claimWindow":""},"accounts":[]}`, ""},
		{"显式设置", `{"version":1,"settings":{"claimWindow":"9,10"},"accounts":[]}`, "9,10"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := store.Settings().ClaimWindow; got != c.want {
			t.Fatalf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// 新库（没有 accounts.json）也应该是默认值
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Settings().ClaimWindow; got != "19,20" {
		t.Fatalf("新库默认值: got %q", got)
	}
	// 序列化后能被前端读到
	raw, _ := json.Marshal(store.Settings())
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if back["claimWindow"] != "19,20" {
		t.Fatalf("JSON 字段名不对: %v", back["claimWindow"])
	}
}
