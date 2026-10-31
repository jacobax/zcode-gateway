package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoginThrottleBlocksAfterFailures(t *testing.T) {
	throttle := newLoginThrottle()
	ip := "203.0.113.7"
	for i := 0; i < loginMaxFails-1; i++ {
		throttle.fail(ip)
		if _, blocked := throttle.blocked(ip); blocked {
			t.Fatalf("blocked too early after %d failures", i+1)
		}
	}
	throttle.fail(ip)
	wait, blocked := throttle.blocked(ip)
	if !blocked || wait <= 0 || wait > loginLockout {
		t.Fatalf("expected lockout after %d failures, got blocked=%v wait=%v", loginMaxFails, blocked, wait)
	}
	// 成功登录会清空计数，别的 IP 不受影响。
	throttle.reset(ip)
	if _, blocked := throttle.blocked(ip); blocked {
		t.Fatal("reset should clear the lockout")
	}
	if _, blocked := throttle.blocked("198.51.100.9"); blocked {
		t.Fatal("other clients must not be affected")
	}
}

func TestClientIPPrefersTunnelHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	if got := clientIP(req); got != "127.0.0.1" {
		t.Fatalf("fallback should be RemoteAddr host, got %q", got)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("X-Forwarded-For first hop expected, got %q", got)
	}
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	if got := clientIP(req); got != "203.0.113.10" {
		t.Fatalf("Cf-Connecting-Ip should win, got %q", got)
	}
}

func TestParseNextAt(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		value any
		want  int64
	}{
		{"秒", float64(now.Unix()), now.Unix()},
		{"毫秒", float64(now.UnixMilli()), now.Unix()},
		{"字符串秒", "1893456000", 1893456000},
		{"RFC3339", "2030-01-01T00:00:00Z", 1893456000},
		{"空", "", 0},
		{"零", float64(0), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNextAt(tc.value); got != tc.want {
				t.Fatalf("parseNextAt(%v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// 面板 token 不回显明文：GET 只给掩码，明文走单独接口。
func TestSettingsMasksProxyToken(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-mask-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = os.RemoveAll(dir)
	})
	// 拼出来而不是写字面量：仓库里的密钥扫描会拦住长得像真 key 的字符串。
	token := "sk-zc-" + strings.Repeat("0123456789", 4)
	store.UpdateSettings(func(st *Settings) { st.ProxyToken = token })
	server := NewServer(store, mustUpstream(t), "http://test")

	rec := httptest.NewRecorder()
	server.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings GET: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, token) {
		t.Fatalf("plaintext token must not be echoed:\n%s", body)
	}
	var parsed struct {
		ProxyTokenMask string `json:"proxyTokenMask"`
		ProxyTokenSet  bool   `json:"proxyTokenSet"`
		Settings       struct {
			ProxyToken string `json:"proxyToken"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.ProxyTokenSet || parsed.Settings.ProxyToken != "" {
		t.Fatalf("unexpected settings payload: %s", body)
	}
	if !strings.HasPrefix(parsed.ProxyTokenMask, "sk-zc") || !strings.HasSuffix(parsed.ProxyTokenMask, token[len(token)-4:]) {
		t.Fatalf("mask should keep a readable head and tail: %q", parsed.ProxyTokenMask)
	}

	rec = httptest.NewRecorder()
	server.handleRevealToken(rec, httptest.NewRequest(http.MethodGet, "/api/settings/token", nil))
	var revealed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &revealed); err != nil {
		t.Fatal(err)
	}
	if revealed.Token != token {
		t.Fatalf("reveal endpoint should return the real token, got %q", revealed.Token)
	}
}
