package main

// 上游下发的模型清单。
//
// 实测（2026-11-03）GET /api/v1/client/configs 返回：
//
//	{"data":{"builtinModels":[
//	  {"modelId":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
//	   "priority":99,
//	   "reasoning":{"defaultLevel":"max","levels":{
//	     "low":{"anthropic":{"set":[{"path":["output_config","effort"],"value":"low"}]}},
//	     "high":{...},"max":{...}}}}]}}
//
// 两个用途：
//  1. /v1/models 不再硬编码，跟着上游走
//  2. max_tokens 超过模型的 maxCompletionTokens 时钳到上限（上游会回 1210）
//
// 刻意不做的事：**不用 builtinModels 自动生成白名单**。它只有 2 个内置模型，
// 而 providers[].models 里还有 GLM-5.2 / GLM-5-Turbo 等——拿内置清单当白名单
// 会把合法请求误杀。白名单仍然靠 ZCODE_MODEL_ALLOWLIST 手工配。
//
// 严格 fail-open：拉不到就保留旧快照，从没拉到过就用内置兜底。

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	catalogTTL     = time.Hour
	catalogRetry   = 5 * time.Minute
	catalogTimeout = 5 * time.Second
)

// catalogSet 是「把某个值写到请求体的某个路径」，由上游下发。
// 例：low → path ["output_config","effort"], value "low"。
type catalogSet struct {
	Path  []string
	Value string
}

type catalogModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxOutput     int
	Priority      int
	// Reasoning 是 档位 → 要写进请求体的 set 列表。
	Reasoning map[string][]catalogSet
}

var (
	catalogMu         sync.RWMutex
	catalogModels     = map[string]*catalogModel{}
	catalogFetchedAt  time.Time
	catalogNextAt     time.Time
	catalogLastErr    string
	catalogRefreshing bool
)

// fallbackCatalog 是拉不到上游清单时用的兜底，和 v1.12.0 之前的硬编码一致。
var fallbackCatalog = []*catalogModel{
	{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash", Priority: 100},
	{ID: "GLM-5.3", Name: "GLM-5.3", Priority: 99},
}

// modelCatalog 返回当前清单（按 priority 降序）。缓存空时返回内置兜底。
func modelCatalog() []*catalogModel {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	if len(catalogModels) == 0 {
		return append([]*catalogModel(nil), fallbackCatalog...)
	}
	out := make([]*catalogModel, 0, len(catalogModels))
	for _, m := range catalogModels {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// catalogModelFor 按名字查模型（大小写不敏感）。查不到返回 nil。
func catalogModelFor(name string) *catalogModel {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	if m, ok := catalogModels[strings.ToLower(strings.TrimSpace(name))]; ok {
		return m
	}
	return nil
}

// catalogMaxOutput 返回该模型的输出上限；未知模型返回 0（不钳制）。
func catalogMaxOutput(name string) int {
	if m := catalogModelFor(name); m != nil {
		return m.MaxOutput
	}
	return 0
}

// catalogReasoningSets 返回某个档位要写进请求体的 set 列表。
// 上游没下发时回落到硬编码的 output_config.effort（实测生效的路径）。
func catalogReasoningSets(model, level string) []catalogSet {
	if m := catalogModelFor(model); m != nil {
		if sets := m.Reasoning[strings.ToLower(level)]; len(sets) > 0 {
			return sets
		}
	}
	return []catalogSet{{Path: []string{"output_config", "effort"}, Value: level}}
}

func catalogStatus() map[string]any {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	out := map[string]any{"models": len(catalogModels)}
	if !catalogFetchedAt.IsZero() {
		out["fetchedAt"] = catalogFetchedAt.Unix()
	}
	if catalogLastErr != "" {
		out["error"] = catalogLastErr
	}
	return out
}

// ensureModelCatalog 在缓存过期时异步刷新一次，立即返回。
func (s *Server) ensureModelCatalog(acc *Account) {
	if acc == nil {
		return
	}
	if time.Now().Before(catalogNextAt) {
		return
	}
	catalogMu.Lock()
	if catalogRefreshing {
		catalogMu.Unlock()
		return
	}
	catalogRefreshing = true
	catalogMu.Unlock()

	go func() {
		defer func() {
			catalogMu.Lock()
			catalogRefreshing = false
			catalogMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
		defer cancel()
		models, _, err := s.fetchModelCatalog(ctx, acc)
		catalogMu.Lock()
		defer catalogMu.Unlock()
		if err != nil {
			catalogLastErr = err.Error()
			catalogNextAt = time.Now().Add(catalogRetry)
			return
		}
		if len(models) > 0 {
			catalogModels = models
			catalogFetchedAt = time.Now()
		}
		catalogLastErr = ""
		catalogNextAt = time.Now().Add(catalogTTL)
	}()
}

func (s *Server) fetchModelCatalog(ctx context.Context, acc *Account) (map[string]*catalogModel, string, error) {
	parsed, _, err := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/client/configs", nil, nil, nil)
	if err != nil {
		return nil, "", err
	}
	models, def := parseModelCatalog(parsed)
	return models, def, nil
}

// parseModelCatalog 从 client/configs 的响应里抽模型清单。
func parseModelCatalog(parsed map[string]any) (map[string]*catalogModel, string) {
	data := activityMap(parsed["data"])
	out := map[string]*catalogModel{}
	for _, item := range activitySlice(data, "builtinModels") {
		raw := activityMap(item)
		id := strings.TrimSpace(activityString(raw, "modelId"))
		if id == "" {
			continue
		}
		model := &catalogModel{
			ID:            id,
			Name:          firstNonEmpty(activityString(raw, "name"), id),
			ContextWindow: activityIntDefault(raw, "contextWindow", 0),
			MaxOutput:     activityIntDefault(raw, "maxCompletionTokens", 0),
			Priority:      activityIntDefault(raw, "priority", 0),
			Reasoning:     map[string][]catalogSet{},
		}
		reasoning := activityMap(raw["reasoning"])
		for level, spec := range activityMap(reasoning["levels"]) {
			sets := parseCatalogSets(activityMap(spec))
			if len(sets) > 0 {
				model.Reasoning[strings.ToLower(level)] = sets
			}
		}
		out[strings.ToLower(id)] = model
	}
	def := ""
	if providers := activitySlice(data, "builtinProviders"); len(providers) > 0 {
		def = activityString(activityMap(providers[0]), "defaultModel")
	}
	return out, def
}

// parseCatalogSets 解析 {"anthropic":{"set":[{"path":[...],"value":".."}]}}。
func parseCatalogSets(spec map[string]any) []catalogSet {
	anthropic := activityMap(spec["anthropic"])
	var out []catalogSet
	for _, item := range activitySlice(anthropic, "set") {
		entry := activityMap(item)
		value := activityString(entry, "value")
		if value == "" {
			continue
		}
		var path []string
		for _, part := range activitySlice(entry, "path") {
			if text, ok := part.(string); ok && text != "" {
				path = append(path, text)
			}
		}
		if len(path) == 0 {
			continue
		}
		out = append(out, catalogSet{Path: path, Value: value})
	}
	return out
}

// applyCatalogSets 按路径把值写进 payload，返回是否改动过。
// 只在路径上的中间节点都是 map 时才写，避免把客户端的数组结构写坏。
func applyCatalogSets(payload map[string]any, sets []catalogSet) bool {
	changed := false
	for _, set := range sets {
		node := payload
		ok := true
		for _, key := range set.Path[:len(set.Path)-1] {
			next, exists := node[key]
			if !exists {
				child := map[string]any{}
				node[key] = child
				node = child
				continue
			}
			child, isMap := next.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			node = child
		}
		if !ok {
			continue
		}
		node[set.Path[len(set.Path)-1]] = set.Value
		changed = true
	}
	return changed
}

// applyModelCatalog 用上游下发的模型清单修正请求体：
//
//  1. max_tokens 超过模型的 maxCompletionTokens 时钳到上限。上游对超限的
//     回 1210（max_tokens 非法），钳制比让客户端吃错误好。
//  2. thinking_level（low/high/max）翻译成上游认的路径。默认是
//     output_config.effort——实测 low 把 thinking 从 513 字符压到 12 字符。
//     路径来自 client/configs 的 reasoning.levels，上游改结构时跟着变。
//
// 客户端已经自己带了 output_config 时不覆盖：它比我们更清楚要什么。
func applyModelCatalog(payload map[string]any) {
	model, _ := payload["model"].(string)
	if model == "" {
		return
	}
	if limit := catalogMaxOutput(model); limit > 0 {
		if current, ok := number(payload["max_tokens"]); ok && current > limit {
			payload["max_tokens"] = limit
		}
	}
	level, _ := payload["thinking_level"].(string)
	if level == "" {
		return
	}
	// 上游不认这个字段，翻译完必须删掉。
	delete(payload, "thinking_level")
	if _, exists := payload["output_config"]; exists {
		return
	}
	applyCatalogSets(payload, catalogReasoningSets(model, level))
}
