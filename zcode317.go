package main

// ZCode 3.17.2 请求形态（灰度开关 ZCODE_SYSTEM_SHAPE=317）。
//
// 为什么需要这一套：上游从 2026-11-03 起对 Start Plan 请求做「版本号 ↔ 提示词
// 形态」的一致性校验——只把 X-ZCode-App-Version 改成 3.17.2、提示词还留 3.16.3
// 会直接吃 405/3012；反过来（3.16.3 版本 + 3.17.2 提示词）也一样。2026-11-07
// 在 aws3 上用 gakiyukr/zcode2api-plus（MIT）实测：整套 3.17.2 形态 + 真实 JWT
// 返回 200，同一进程把版本改回 3.16.3 立刻 3012。
//
// 提示词文本取自 gakiyukr/zcode2api-plus 的 internal/upstream/zcode_system.json
// （MIT License, Copyright (c) 2026 yetone），其注释说明文本来自 ZCode 3.17.2
// 官方客户端源码。这里逐字节照抄，因为上游按 byte-for-byte 校验前两块。
//
// 形态要点（对齐官方 ContextBuilder）：
//  1. system 恰好三块：cliPrefix、两段 stable 合并、动态段（官方文本 + 调用方
//     system 文本并入块尾）；
//  2. 首个 user turn 之前插入当天日期的 <system-reminder>；
//  3. cache_control 只留在 system 三块和最后一条非 system 消息的末块；
//  4. UA 带 ai-sdk 后缀（ZCode/3.17.2 ai-sdk/anthropic/3.0.81）、X-Title 是
//     "Z Code@cli"、X-Platform 只给平台名（不带 -arch）。

import (
	_ "embed"
	"encoding/json"
	"os"
	"strings"
	"time"
)

//go:embed zcode317_prompt.json
var zcode317PromptJSON []byte

type zcode317PromptSet struct {
	CliPrefix         string   `json:"cliPrefix"`
	StableSections    []string `json:"stableSections"`
	DynamicBehavior   string   `json:"dynamicBehavior"`
	SessionGuidance   string   `json:"sessionGuidance"`
	ContextManagement string   `json:"contextManagement"`
	PoweredBy         string   `json:"poweredBy"`
	Environment       struct {
		Heading        string `json:"heading"`
		InvokedLine    string `json:"invokedLine"`
		PlatformLabel  string `json:"platformLabel"`
		OsVersionLabel string `json:"osVersionLabel"`
		UntrustedNote  string `json:"untrustedNote"`
	} `json:"environment"`
	Context struct {
		CurrentDateLine string `json:"currentDateLine"`
	} `json:"context"`
}

var zcode317Prompts *zcode317PromptSet

func init() {
	var parsed zcode317PromptSet
	if err := json.Unmarshal(zcode317PromptJSON, &parsed); err != nil || parsed.CliPrefix == "" {
		// 解析失败时保持 nil：开关打开也不会发出半套提示词（那比不发更糟）。
		return
	}
	zcode317Prompts = &parsed
	// 317 形态自带版本号：没显式指定 ZCODE_APP_VERSION 时一起切到 3.17.2，
	// 否则版本头和提示词会错配（实测两个方向都吃 3012）。
	if systemShape317() && os.Getenv("ZCODE_APP_VERSION") == "" {
		appVersion = "3.17.2"
	}
}

// systemShape317 是 3.17.2 形态的总开关。
func systemShape317() bool {
	return strings.TrimSpace(os.Getenv("ZCODE_SYSTEM_SHAPE")) == "317"
}

// dressSystem317 按官方 3.17.2 形态重建 system / messages / metadata。
// 不幂等：同一 body 只能调用一次（会重复追加日期前缀）。
func dressSystem317(payload map[string]any, acc *Account, sessionID string) {
	p := zcode317Prompts
	if p == nil || payload == nil {
		return
	}
	dynamic := []string{p.DynamicBehavior}
	if hasSkillTool(payload["tools"]) {
		dynamic = append(dynamic, p.SessionGuidance)
	}
	dynamic = append(dynamic, environment317(p, acc))
	if model, _ := payload["model"].(string); model != "" {
		line := strings.ReplaceAll(p.PoweredBy, "{provider}", "zai-api")
		dynamic = append(dynamic, strings.ReplaceAll(line, "{model}", model))
	}
	dynamic = append(dynamic, p.ContextManagement)
	dynamic = append(dynamic, clientSystemTexts(payload["system"])...)

	cached := func(text string) map[string]any {
		return map[string]any{
			"type":          "text",
			"text":          text,
			"cache_control": map[string]any{"type": "ephemeral"},
		}
	}
	payload["system"] = []any{
		cached(p.CliPrefix),
		cached(strings.Join(p.StableSections, "\n\n")),
		cached("\n\n" + strings.Join(dynamic, "\n\n")),
	}

	// 官方把当天日期放在首个 user turn，而不是 system 里。
	dateLine := strings.ReplaceAll(p.Context.CurrentDateLine, "{date}", time.Now().Format("2006-01-02"))
	prefix := map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "text",
			"text": "<system-reminder>" + dateLine + "</system-reminder>",
		}},
	}
	messages, _ := payload["messages"].([]any)
	payload["messages"] = append([]any{prefix}, messages...)

	// cache_control 重排：官方只留 system 三块 + 最后一条非 system 消息末块。
	var last map[string]any
	for _, item := range payload["messages"].([]any) {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			continue
		}
		last = msg
		if blocks, ok := msg["content"].([]any); ok {
			for _, b := range blocks {
				if m, ok := b.(map[string]any); ok {
					delete(m, "cache_control")
				}
			}
		}
	}
	if last != nil {
		switch content := last["content"].(type) {
		case string:
			last["content"] = []any{cached(content)}
		case []any:
			if len(content) > 0 {
				if m, ok := content[len(content)-1].(map[string]any); ok {
					m["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, item := range tools {
			if t, ok := item.(map[string]any); ok {
				delete(t, "cache_control")
			}
		}
	}

	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["user_id"] = upstreamUserID(acc, sessionID)
	payload["metadata"] = meta
}

// applyHeaders317 把身份头切成官方 CLI 的取值。
// 实测差异只在 UA / X-Title / X-Platform 三处，其余和 3.16.3 一致。
func applyHeaders317(headers map[string]string, acc *Account) {
	if acc == nil {
		return
	}
	profile := *accountProfile(acc)
	headers["User-Agent"] = "ZCode/" + appVersion + " ai-sdk/anthropic/3.0.81"
	headers["X-Title"] = "Z Code@cli"
	if profile.Platform != "" {
		headers["X-Platform"] = profile.Platform
	}
}

// environment317 渲染官方 environment 段。网关不知道调用方的工作目录，
// 保持官方的「无目录」分支：平台、系统版本、不可信数据提示。
func environment317(p *zcode317PromptSet, acc *Account) string {
	e := p.Environment
	profile := accountProfile(acc)
	osLine := profile.Platform
	if profile.OSVersion != "" {
		osLine = strings.TrimSpace(profile.Platform + " " + profile.OSVersion + " " + profile.Arch)
	}
	lines := []string{
		e.Heading,
		e.InvokedLine,
		" - " + e.PlatformLabel + ": " + profile.Platform,
		" - " + e.OsVersionLabel + ": " + osLine,
		" - " + e.UntrustedNote,
	}
	return strings.Join(lines, "\n")
}

// clientSystemTexts 取调用方原有的 system 文本（字串或 text block 数组）。
func clientSystemTexts(system any) []string {
	switch value := system.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return []string{value}
		}
	case []any:
		var out []string
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := block["type"].(string); kind != "text" {
				continue
			}
			if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// hasSkillTool 判断请求是否带 Skill 工具（官方只在有它时追加技能指引段）。
func hasSkillTool(tools any) bool {
	list, ok := tools.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := tool["name"].(string); name == "Skill" {
			return true
		}
	}
	return false
}

// accountProfile 取账号的设备档案，缺失时给一个空档案（不读宿主机）。
func accountProfile(acc *Account) *DeviceProfile {
	if acc == nil {
		return &DeviceProfile{}
	}
	if acc.Profile != nil {
		return acc.Profile
	}
	return &DeviceProfile{}
}
