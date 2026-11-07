package main

import "testing"

// 夹具取自 2026-11-03 实测的 /api/v1/client/configs 响应（已裁剪）。
func sampleClientConfigs() map[string]any {
	return map[string]any{
		"code": float64(0),
		"data": map[string]any{
			"builtinModels": []any{
				map[string]any{
					"modelId": "GLM-5.3", "name": "GLM-5.3",
					"contextWindow": float64(1000000), "maxCompletionTokens": float64(128000),
					"priority": float64(99),
					"reasoning": map[string]any{
						"defaultLevel": "max",
						"levels": map[string]any{
							"low": map[string]any{"anthropic": map[string]any{"set": []any{
								map[string]any{"path": []any{"output_config", "effort"}, "value": "low"},
							}}},
							"high": map[string]any{"anthropic": map[string]any{"set": []any{
								map[string]any{"path": []any{"output_config", "effort"}, "value": "high"},
							}}},
							"max": map[string]any{"anthropic": map[string]any{"set": []any{
								map[string]any{"path": []any{"output_config", "effort"}, "value": "max"},
							}}},
						},
					},
				},
				map[string]any{
					"modelId": "GLM-5.3-Flash", "name": "GLM-5.3-Flash",
					"contextWindow": float64(1000000), "maxCompletionTokens": float64(128000),
					"priority": float64(100),
				},
			},
			"builtinProviders": []any{
				map[string]any{"id": "builtin:bigmodel", "defaultModel": "GLM-5.3"},
			},
		},
	}
}

func TestParseModelCatalog(t *testing.T) {
	models, def := parseModelCatalog(sampleClientConfigs())
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if def != "GLM-5.3" {
		t.Fatalf("default model = %q", def)
	}
	m := models["glm-5.3"]
	if m == nil {
		t.Fatal("GLM-5.3 missing (keys should be lowercased)")
	}
	if m.ContextWindow != 1000000 || m.MaxOutput != 128000 || m.Priority != 99 {
		t.Fatalf("fields wrong: %#v", m)
	}
	for _, level := range []string{"low", "high", "max"} {
		sets := m.Reasoning[level]
		if len(sets) != 1 || len(sets[0].Path) != 2 || sets[0].Value != level {
			t.Fatalf("reasoning %q wrong: %#v", level, sets)
		}
	}
	// 没有 reasoning 的模型不该 panic。
	if flash := models["glm-5.3-flash"]; flash == nil || len(flash.Reasoning) != 0 {
		t.Fatalf("flash model wrong: %#v", flash)
	}
}

func TestParseModelCatalogEmpty(t *testing.T) {
	for _, parsed := range []map[string]any{
		nil,
		{},
		{"data": map[string]any{}},
		{"data": map[string]any{"builtinModels": []any{}}},
		{"data": map[string]any{"builtinModels": []any{map[string]any{"name": "no-id"}}}},
	} {
		models, def := parseModelCatalog(parsed)
		if len(models) != 0 || def != "" {
			t.Fatalf("want empty catalog, got %#v def=%q", models, def)
		}
	}
}

// 路径写入：中间节点不存在时自动创建；遇到非 map 就放弃这条，不写坏客户端结构。
func TestApplyCatalogSets(t *testing.T) {
	payload := map[string]any{}
	if !applyCatalogSets(payload, []catalogSet{{Path: []string{"output_config", "effort"}, Value: "low"}}) {
		t.Fatal("should report change")
	}
	output, ok := payload["output_config"].(map[string]any)
	if !ok || output["effort"] != "low" {
		t.Fatalf("path not written: %#v", payload)
	}

	// 中间节点是字符串：放弃，不覆盖。
	payload2 := map[string]any{"output_config": "client-set-string"}
	if applyCatalogSets(payload2, []catalogSet{{Path: []string{"output_config", "effort"}, Value: "low"}}) {
		t.Fatal("should not write through a non-map node")
	}
	if payload2["output_config"] != "client-set-string" {
		t.Fatalf("clobbered client value: %#v", payload2)
	}
}

// applyModelCatalog：max_tokens 钳制 + thinking_level 翻译 + 客户端已带 output_config 时不覆盖。
func TestApplyModelCatalog(t *testing.T) {
	setCatalogForTest(map[string]*catalogModel{
		"glm-5.3": {
			ID: "GLM-5.3", MaxOutput: 128000,
			Reasoning: map[string][]catalogSet{
				"low": {{Path: []string{"output_config", "effort"}, Value: "low"}},
			},
		},
	})
	defer resetCatalogForTest()

	// 1) 超限钳制
	payload := map[string]any{"model": "GLM-5.3", "max_tokens": 200000}
	applyModelCatalog(payload)
	if payload["max_tokens"] != 128000 {
		t.Fatalf("max_tokens not clamped: %v", payload["max_tokens"])
	}

	// 2) 未超限不动
	payload = map[string]any{"model": "GLM-5.3", "max_tokens": 4096}
	applyModelCatalog(payload)
	if payload["max_tokens"] != 4096 {
		t.Fatalf("max_tokens should stay: %v", payload["max_tokens"])
	}

	// 3) thinking_level 翻译并删除原字段
	payload = map[string]any{"model": "GLM-5.3", "thinking_level": "low"}
	applyModelCatalog(payload)
	if _, exists := payload["thinking_level"]; exists {
		t.Fatal("thinking_level should be removed after translation")
	}
	if out, ok := payload["output_config"].(map[string]any); !ok || out["effort"] != "low" {
		t.Fatalf("thinking_level not translated: %#v", payload)
	}

	// 4) 客户端自己带 output_config 时不覆盖
	payload = map[string]any{
		"model": "GLM-5.3", "thinking_level": "low",
		"output_config": map[string]any{"effort": "max"},
	}
	applyModelCatalog(payload)
	if out := payload["output_config"].(map[string]any); out["effort"] != "max" {
		t.Fatalf("client output_config must win: %#v", out)
	}

	// 5) 未知模型不钳制
	payload = map[string]any{"model": "unknown-model", "max_tokens": 999999}
	applyModelCatalog(payload)
	if payload["max_tokens"] != 999999 {
		t.Fatalf("unknown model should not be clamped: %v", payload["max_tokens"])
	}
}

// catalogReasoningSets 在没有上游映射时回落到硬编码的 output_config.effort
// ——这是实测生效的路径，不能因为拉不到清单就丢掉思考档位。
func TestCatalogReasoningSetsFallback(t *testing.T) {
	resetCatalogForTest()
	sets := catalogReasoningSets("whatever", "high")
	if len(sets) != 1 || sets[0].Value != "high" || len(sets[0].Path) != 2 {
		t.Fatalf("fallback wrong: %#v", sets)
	}
	if sets[0].Path[0] != "output_config" || sets[0].Path[1] != "effort" {
		t.Fatalf("fallback path wrong: %#v", sets[0].Path)
	}
}

func setCatalogForTest(models map[string]*catalogModel) {
	catalogMu.Lock()
	catalogModels = models
	catalogMu.Unlock()
}

func resetCatalogForTest() {
	catalogMu.Lock()
	catalogModels = map[string]*catalogModel{}
	catalogMu.Unlock()
}

// reasoning_effort / reasoning.effort 要收敛成上游认的 low/high/max，
// 并经由 applyModelCatalog 落成 output_config.effort。
func TestReasoningEffortMapping(t *testing.T) {
	cases := map[string]string{
		"minimal": "low", "low": "low", "LOW": "low",
		"medium": "high", "high": "high", "max": "max",
		"none": "", "": "", "turbo": "",
	}
	for input, want := range cases {
		if got := normalizeReasoningLevel(input); got != want {
			t.Fatalf("normalizeReasoningLevel(%q) = %q, want %q", input, got, want)
		}
	}

	chat, err := openAIRequestToAnthropic(map[string]any{
		"model":            "GLM-5.3",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if chat["thinking_level"] != "high" {
		t.Fatalf("chat reasoning_effort not mapped: %#v", chat)
	}
	if chat["model"] != "GLM-5.3" {
		t.Fatalf("chat model not propagated upstream: %#v", chat)
	}

	resp, err := responsesRequestToAnthropic(map[string]any{
		"model":     "GLM-5.3",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "minimal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp["thinking_level"] != "low" {
		t.Fatalf("responses reasoning.effort not mapped: %#v", resp)
	}

	// effort=none 不设档位，走模型默认。
	none, err := responsesRequestToAnthropic(map[string]any{
		"input":     "hi",
		"reasoning": map[string]any{"effort": "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := none["thinking_level"]; exists {
		t.Fatalf("effort=none should not set a level: %#v", none)
	}

	// 转换出来的档位要能被 applyModelCatalog 落成上游字段。
	resetCatalogForTest()
	applyModelCatalog(chat)
	if out, ok := chat["output_config"].(map[string]any); !ok || out["effort"] != "high" {
		t.Fatalf("level not translated to output_config: %#v", chat)
	}
}
