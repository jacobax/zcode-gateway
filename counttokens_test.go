package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 估算规则要可预测：英文 4 字符/token、CJK 1 字符/token、其他非 ASCII 2 字符/token。
func TestEstimateTokens(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"abcd":     1,
		"abcdefgh": 2,
		"你好世界":     4,
		"Привет":   3, // 6 个西里尔字符 → 3
	}
	for text, want := range cases {
		if got := estimateTokens(text); got != want {
			t.Fatalf("estimateTokens(%q) = %d, want %d", text, got, want)
		}
	}

	// 递归收集：messages 里的文本都要算进去。role/type 这类结构字段也会被
	// 算上——刻意偏保守，多算几个 token 只会让客户端早点压缩上下文。
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "abcd"},
			}},
		},
	}
	// ascii: user(4) + assistant(9) + text(4) + abcd(4) = 21 → 6；wide: 你好 = 2
	if got := estimateTokens(body); got != 8 {
		t.Fatalf("nested body = %d, want 8", got)
	}
}

func TestCountTokensEndpoint(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.accounts = nil
	store.settings.ProxyToken = "tok"
	server := NewServer(store, mustUpstream(t), "http://test")

	post := func(token string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("x-api-key", token)
		}
		server.handleCountTokens(rec, req)
		return rec
	}

	rec := post("tok", `{"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	// "user"(4 ascii → 1) + "你好"(2 wide → 2)
	if out.InputTokens != 3 {
		t.Fatalf("input_tokens = %d, want 3 (%s)", out.InputTokens, rec.Body.String())
	}

	if rec := post("", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token should be 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	server.handleCountTokens(rec, httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET should be 405, got %d", rec.Code)
	}

	if rec := post("tok", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json should be 400, got %d", rec.Code)
	}
}
