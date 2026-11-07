package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 3.17.2 提示词是从官方客户端抄来的字节级常量，解析失败会让开关变成空操作，
// 这里把它钉住。
func TestZcode317PromptsParse(t *testing.T) {
	p := zcode317Prompts
	if p == nil {
		t.Fatal("zcode317_prompt.json 没解析出来")
	}
	if p.CliPrefix != "You are ZCode, an interactive agent" {
		t.Fatalf("cliPrefix = %q", p.CliPrefix)
	}
	if len(p.StableSections) != 2 {
		t.Fatalf("stableSections = %d, want 2", len(p.StableSections))
	}
	for name, value := range map[string]string{
		"dynamicBehavior":   p.DynamicBehavior,
		"sessionGuidance":   p.SessionGuidance,
		"contextManagement": p.ContextManagement,
		"poweredBy":         p.PoweredBy,
		"environment":       p.Environment.Heading,
		"currentDateLine":   p.Context.CurrentDateLine,
	} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("%s 为空", name)
		}
	}
}

func TestSystemShape317Switch(t *testing.T) {
	t.Setenv("ZCODE_SYSTEM_SHAPE", "")
	if systemShape317() {
		t.Fatal("317 形态必须默认关闭")
	}
	t.Setenv("ZCODE_SYSTEM_SHAPE", "317")
	if !systemShape317() {
		t.Fatal("ZCODE_SYSTEM_SHAPE=317 应该打开")
	}
}

// dressSystem317 要复刻官方形态：恰好三块 system、日期 reminder 前置、
// cache_control 只留三块 system + 最后一条消息末块、调用方 system 并入第三块。
func TestDressSystem317(t *testing.T) {
	acc := &Account{
		User:      UserInfo{ID: "user-1"},
		DeviceMid: "mid-1",
		Profile:   &DeviceProfile{Platform: "linux", Arch: "x64", OSVersion: "6.1.0-13-amd64", DeviceMid: "mid-1"},
	}
	body := map[string]any{
		"model": "GLM-5.3-Flash",
		"system": []any{
			map[string]any{"type": "text", "text": "客户端自己的规则", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "第一轮", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "回答"},
			}},
			map[string]any{"role": "user", "content": "第二轮"},
		},
		"tools": []any{map[string]any{
			"name": "Skill", "cache_control": map[string]any{"type": "ephemeral"},
			"input_schema": map[string]any{"type": "object"},
		}},
	}
	dressSystem317(body, acc, "sess-1")

	system, _ := body["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("system 必须是 3 块，得到 %d", len(system))
	}
	for i, item := range system {
		block := item.(map[string]any)
		if block["cache_control"] == nil {
			t.Fatalf("第 %d 块缺少 cache_control", i)
		}
	}
	if got := system[0].(map[string]any)["text"]; got != zcode317Prompts.CliPrefix {
		t.Fatalf("首块不是 cliPrefix: %q", got)
	}
	third := system[2].(map[string]any)["text"].(string)
	for _, want := range []string{
		zcode317Prompts.DynamicBehavior,
		zcode317Prompts.SessionGuidance, // tools 里有 Skill 才追加
		"# Environment",
		"GLM-5.3-Flash", // poweredBy 里的 {model}
		zcode317Prompts.ContextManagement,
		"客户端自己的规则", // 调用方 system 并入块尾
	} {
		if !strings.Contains(third, want) {
			t.Fatalf("第三块缺少 %q", want)
		}
	}
	if strings.Contains(third, "{model}") || strings.Contains(third, "{provider}") {
		t.Fatalf("占位符没替换: %q", third)
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("应该多出一条日期 reminder，得到 %d 条", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("reminder 应该是 user 轮: %#v", first)
	}
	reminder := first["content"].([]any)[0].(map[string]any)["text"].(string)
	today := time.Now().Format("2006-01-02")
	if !strings.Contains(reminder, today) || !strings.HasPrefix(reminder, "<system-reminder>") {
		t.Fatalf("日期 reminder 不对: %q", reminder)
	}

	// cache_control 只应留在最后一条非 system 消息的末块。
	last := messages[len(messages)-1].(map[string]any)
	blocks := last["content"].([]any)
	if blocks[len(blocks)-1].(map[string]any)["cache_control"] == nil {
		t.Fatal("最后一条消息的末块应带 cache_control")
	}
	firstUserBlocks := messages[1].(map[string]any)["content"].([]any)
	if firstUserBlocks[0].(map[string]any)["cache_control"] != nil {
		t.Fatal("中间消息的 cache_control 应被清掉")
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["cache_control"] != nil {
		t.Fatal("tools 上的 cache_control 应被清掉")
	}

	// metadata.user_id 是官方 blob。
	var blob map[string]string
	if err := json.Unmarshal([]byte(body["metadata"].(map[string]any)["user_id"].(string)), &blob); err != nil {
		t.Fatalf("metadata.user_id 不是 JSON blob: %v", err)
	}
	if blob["account_uuid"] != "user-1" || blob["session_id"] != "sess-1" || blob["device_id"] == "" {
		t.Fatalf("blob 字段不对: %#v", blob)
	}
}

// 没有 Skill 工具时不追加技能指引段（对齐官方 ContextBuilder 的条件）。
func TestDressSystem317NoSkillTool(t *testing.T) {
	acc := &Account{User: UserInfo{ID: "u"}, Profile: &DeviceProfile{Platform: "linux"}}
	body := map[string]any{
		"model":    "GLM-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	dressSystem317(body, acc, "s")
	third := body["system"].([]any)[2].(map[string]any)["text"].(string)
	if strings.Contains(third, zcode317Prompts.SessionGuidance) {
		t.Fatal("没有 Skill 工具时不该带技能指引段")
	}
}
