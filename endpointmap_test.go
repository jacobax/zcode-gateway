package main

import "testing"

// 模型白名单：空 = 不限；设了就按大小写不敏感匹配。
func TestModelAllowlist(t *testing.T) {
	t.Setenv("ZCODE_MODEL_ALLOWLIST", "")
	if !modelAllowed("anything") {
		t.Fatal("empty allowlist should allow everything")
	}

	t.Setenv("ZCODE_MODEL_ALLOWLIST", "GLM-5.3, glm-4.6 ,,")
	for _, name := range []string{"GLM-5.3", "glm-5.3", "GLM-4.6", " glm-4.6 "} {
		if !modelAllowed(name) {
			t.Errorf("%q should be allowed", name)
		}
	}
	for _, name := range []string{"gpt-4", "claude-3"} {
		if modelAllowed(name) {
			t.Errorf("%q should be rejected", name)
		}
	}
}

// 映射解析：只收 https，空字段丢弃，多余字段忽略。
// 夹具是 2026-11-03 从 /api/v1/agent/configs 实测抓到的真实响应。
func TestParseEndpointMapping(t *testing.T) {
	parsed := map[string]any{
		"code": float64(0),
		"data": map[string]any{
			"codingPlanSignature": map[string]any{"enable": true},
			"proxyEndpoint": map[string]any{
				"mapping": []any{
					map[string]any{
						"from":                     "https://open.bigmodel.cn/api/anthropic/v1/messages",
						"to":                       "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages",
						"zcodeAuthorizationHeader": "X-Zcode-Authorization",
					},
					map[string]any{
						"from": "https://api.z.ai/api/anthropic/v1/messages",
						"to":   "http://plaintext.example/v1/messages",
					},
					map[string]any{"from": "https://x/v1/messages"},
					map[string]any{"to": "https://y/v1/messages"},
				},
			},
		},
	}
	got := parseEndpointMapping(parsed)
	if len(got) != 1 {
		t.Fatalf("want 1 accepted mapping, got %d: %#v", len(got), got)
	}
	want := "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
	if got["https://open.bigmodel.cn/api/anthropic/v1/messages"] != want {
		t.Fatalf("mapping wrong: %#v", got)
	}
}

// 拿不到映射时不能 panic，返回空表让调用方回落到硬编码兜底。
func TestParseEndpointMappingEmpty(t *testing.T) {
	for _, parsed := range []map[string]any{
		nil,
		{},
		{"data": map[string]any{}},
		{"data": map[string]any{"proxyEndpoint": map[string]any{}}},
	} {
		if got := parseEndpointMapping(parsed); len(got) != 0 {
			t.Fatalf("want empty mapping, got %#v", got)
		}
	}
}

// resolveEndpoint 没命中时返回空串，调用方回落到自己算的默认端点。
func TestResolveEndpointMiss(t *testing.T) {
	if got := resolveEndpoint("https://unknown/v1/messages"); got != "" {
		t.Fatalf("want empty for unknown endpoint, got %q", got)
	}
	if got := resolveEndpoint(""); got != "" {
		t.Fatalf("want empty for empty endpoint, got %q", got)
	}
}
