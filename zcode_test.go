package main

import (
	"encoding/json"
	"testing"
	"time"
)

// x-query-id 要和官方客户端同形状：v7 UUID，前 48 位是毫秒时间戳。
func TestUUIDv7Shape(t *testing.T) {
	id := uuidV7()
	if len(id) != 36 {
		t.Fatalf("length = %d, want 36 (%q)", len(id), id)
	}
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Fatalf("missing dash at %d: %q", pos, id)
		}
	}
	if id[14] != '7' {
		t.Fatalf("version nibble = %q, want 7 (%q)", id[14], id)
	}
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("variant nibble = %q, want 8/9/a/b (%q)", id[19], id)
	}
}

// 时间前缀要能排出先后：跨毫秒生成的两个 id 字典序必须递增。
func TestUUIDv7SortsByTime(t *testing.T) {
	first := uuidV7()
	time.Sleep(3 * time.Millisecond)
	second := uuidV7()
	if first >= second {
		t.Fatalf("uuidv7 should sort by time: %q >= %q", first, second)
	}
}

// 同一个会话必须永远得到同一个上游会话 id，否则前缀缓存和粘性都没意义。
func TestUpstreamSessionIDStable(t *testing.T) {
	first := upstreamSessionID("conv-1")
	if second := upstreamSessionID("conv-1"); first != second {
		t.Fatalf("same session key must give same id: %q vs %q", first, second)
	}
	if first == upstreamSessionID("conv-2") {
		t.Fatal("different session keys must give different ids")
	}
	if len(first) != 36 {
		t.Fatalf("session id should look like a UUID: %q", first)
	}
	// 没有会话信息时退回随机：两次不同，形状仍然合法。
	x, y := upstreamSessionID(""), upstreamSessionID("")
	if x == y || len(x) != 36 {
		t.Fatalf("empty key should fall back to a random UUID: %q %q", x, y)
	}
}

// metadata.user_id 要和官方客户端同形状：JSON blob，三个字段齐全。
func TestUpstreamUserIDBlob(t *testing.T) {
	acc := &Account{User: UserInfo{ID: "user-123"}, DeviceMid: "mid-1"}
	var blob map[string]string
	if err := json.Unmarshal([]byte(upstreamUserID(acc, "sess-1")), &blob); err != nil {
		t.Fatalf("metadata.user_id should be a JSON blob: %v", err)
	}
	if blob["device_id"] != "mid-1" || blob["account_uuid"] != "user-123" || blob["session_id"] != "sess-1" {
		t.Fatalf("blob wrong: %#v", blob)
	}
	// 账号档案里的 device mid 更具体，优先用它。
	acc.Profile = &DeviceProfile{DeviceMid: "mid-2"}
	if err := json.Unmarshal([]byte(upstreamUserID(acc, "sess-1")), &blob); err != nil {
		t.Fatal(err)
	}
	if blob["device_id"] != "mid-2" {
		t.Fatalf("profile device mid should win: %#v", blob)
	}
}
