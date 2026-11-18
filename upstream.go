package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	fhttp "github.com/bogdanfinn/fhttp"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

type Upstream struct {
	client   tls_client.HttpClient
	proxyURL string
	// 领取/活动调用用的独立客户端：这类请求量大、同 IP 连续打容易吃 3012，
	// 单独走一个能换出口的代理（通常是本地中继 → 住宅代理池）。
	claimClient   tls_client.HttpClient
	claimProxyURL string
	// egress 是模型流量可选的出口：指向本地中继时，等价于把住宅代理池当网关出口。
	egressClient   tls_client.HttpClient
	egressProxyURL string
}

func normalizeProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5 or socks5h)", scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy URL has no host")
	}
	u.Scheme = scheme
	return u.String(), nil
}

func proxyDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func newTLSClient(proxyURL string) (tls_client.HttpClient, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithTimeoutSeconds(300),
	}
	if proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(proxyURL))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
}

func NewUpstream() (*Upstream, error) {
	proxy := envOr("ZCODE_PROXY_URL", envOr("ZCODE_PROXY", ""))
	proxyURL, err := normalizeProxyURL(proxy)
	if err != nil {
		return nil, err
	}
	client, err := newTLSClient(proxyURL)
	if err != nil {
		return nil, err
	}
	u := &Upstream{client: client, proxyURL: proxyURL, claimClient: client}
	if raw := envOr("ZCODE_PROXY_URL_CLAIM", ""); raw != "" {
		claimURL, err := normalizeProxyURL(raw)
		if err != nil {
			return nil, err
		}
		claimClient, err := newTLSClient(claimURL)
		if err != nil {
			return nil, err
		}
		u.claimClient = claimClient
		u.claimProxyURL = claimURL
	}
	// 网关出口：默认复用领取中继（ZCODE_PROXY_URL_CLAIM），也可用 ZCODE_PROXY_URL_EGRESS 单独指定。
	if raw := envOr("ZCODE_PROXY_URL_EGRESS", envOr("ZCODE_PROXY_URL_CLAIM", "")); raw != "" {
		egressURL, err := normalizeProxyURL(raw)
		if err != nil {
			return nil, err
		}
		egressClient, err := newTLSClient(egressURL)
		if err != nil {
			return nil, err
		}
		u.egressClient = egressClient
		u.egressProxyURL = egressURL
	}
	return u, nil
}

func (u *Upstream) ProxyURL() string {
	return u.proxyURL
}

// ClaimProxyURL 是领取/活动调用实际使用的代理（空 = 与主流量相同）。
func (u *Upstream) ClaimProxyURL() string {
	return u.claimProxyURL
}

// HasClaimProxy 表示领取是否配了独立出口（否则 Claim() 和直连是同一个客户端）。
func (u *Upstream) HasClaimProxy() bool {
	return u.claimProxyURL != ""
}

// HasEgress 表示是否配置了可选的模型出口（本地中继或独立代理）。
func (u *Upstream) HasEgress() bool {
	return u.egressClient != nil
}

// EgressProxyURL 是模型出口代理地址（空 = 未配置）。
func (u *Upstream) EgressProxyURL() string {
	return u.egressProxyURL
}

// claimDoers 按面板设置排出领取/活动调用的出口顺序：
//
//	direct          仅直连
//	direct_fallback 直连优先，传输层失败再走代理
//	proxy           仅代理
//	proxy_fallback  代理优先，传输层失败再直连
func (s *Server) claimDoers() []HTTPDoer {
	direct := HTTPDoer(s.upstream)
	if !s.upstream.HasClaimProxy() {
		return []HTTPDoer{direct}
	}
	proxy := s.upstream.Claim()
	switch s.store.Settings().ClaimEgress {
	case "direct":
		return []HTTPDoer{direct}
	case "direct_fallback":
		return []HTTPDoer{direct, proxy}
	case "proxy_fallback":
		return []HTTPDoer{proxy, direct}
	default: // "proxy" 或未设置
		return []HTTPDoer{proxy}
	}
}

// claimRequest 依次尝试候选出口。只在传输层失败时回退——一旦拿到响应就返回，
// 免得把一次领取提交两次。
func (s *Server) claimRequest(ctx context.Context, method, target string, headers map[string]string, body []byte) (*fhttp.Response, error) {
	doers := s.claimDoers()
	var lastErr error
	for i, doer := range doers {
		resp, err := doRequest(ctx, doer, method, target, headers, body)
		if err == nil {
			if i > 0 {
				log.Printf("领取出口回退成功（第 %d 个候选）", i+1)
			}
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// claimDoer 让领取/活动这类调用走独立出口，主流量（模型请求、额度刷新）保持直连。
type claimDoer struct{ c tls_client.HttpClient }

func (d claimDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.c.Do(req)
}

// Claim 返回领取/活动调用应使用的 HTTPDoer。
func (u *Upstream) Claim() HTTPDoer {
	return claimDoer{c: u.claimClient}
}

func (u *Upstream) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return u.client.Do(req)
}

func (s *Server) ensureProjectToken(ctx context.Context, acc *Account) (string, error) {
	if acc.Project == nil {
		return "", errors.New("account has no project key")
	}
	if acc.ProjectToken != "" && acc.ProjectTokenExp > time.Now().Unix()+120 {
		return acc.ProjectToken, nil
	}
	token, expires, err := s.mintProjectToken(ctx, acc)
	if err != nil {
		return "", err
	}
	s.store.UpdateAccount(acc.ID, func(a *Account) {
		a.ProjectToken = token
		a.ProjectTokenExp = expires
	})
	acc.ProjectToken = token
	acc.ProjectTokenExp = expires
	return token, nil
}

// passthroughBetas 是实测上游接受、且缺了会掉能力的 anthropic-beta 能力位。
// 上游对不认识的 beta 会直接 400，所以默认只放行白名单里的；排查时用
// ZCODE_PASSTHROUGH_BETA=1 整串透传。
var passthroughBetas = map[string]bool{
	"mid-conversation-system": true,
}

// filterAnthropicBeta 只留下白名单里的 beta（输入是逗号分隔的一串）。
func filterAnthropicBeta(value string) string {
	if value == "" {
		return ""
	}
	keep := make([]string, 0, 2)
	for _, part := range strings.Split(value, ",") {
		beta := strings.TrimSpace(part)
		if beta != "" && passthroughBetas[beta] {
			keep = append(keep, beta)
		}
	}
	return strings.Join(keep, ",")
}

func (s *Server) Forward(ctx context.Context, acc *Account, body map[string]any, clientHeaders map[string]string, sessionKey string) (*fhttp.Response, []byte, error) {
	return s.forward(ctx, acc, body, clientHeaders, false, sessionKey)
}

// ForwardViaEgress 走可选出口（本地中继 / 独立代理），用于直连被风控时的兜底重试。
func (s *Server) ForwardViaEgress(ctx context.Context, acc *Account, body map[string]any, clientHeaders map[string]string, sessionKey string) (*fhttp.Response, []byte, error) {
	return s.forward(ctx, acc, body, clientHeaders, true, sessionKey)
}

func (s *Server) forward(ctx context.Context, acc *Account, body map[string]any, clientHeaders map[string]string, viaEgress bool, sessionKey string) (*fhttp.Response, []byte, error) {
	payload := make(map[string]any, len(body))
	for k, v := range body {
		payload[k] = v
	}
	// 会话标识要稳定且两处同值：X-Session-Id 和 metadata 里的 session_id
	// 是同一个值，上游才认得出「同一段对话」，前缀缓存才有机会命中。
	sessionID := upstreamSessionID(sessionKey)
	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["user_id"] = upstreamUserID(acc, sessionID)
	payload["metadata"] = meta
	if override := strings.TrimSpace(os.Getenv("ZCODE_MODEL")); override != "" {
		payload["model"] = override
	} else if _, ok := payload["model"]; !ok || payload["model"] == "" {
		payload["model"] = "GLM-5.3-Flash"
	}
	sanitizeForUpstream(payload)
	// 模型清单：拉得到就钳 max_tokens、把 thinking_level 翻成上游认的路径。
	s.ensureModelCatalog(acc)
	applyModelCatalog(payload)
	if systemShape317() {
		// 3.17.2 形态自己重建 system/messages/metadata，和 fingerprint 路径互斥：
		// 上游要求版本头与提示词成套，混着来必吃 3012。
		dressSystem317(payload, acc, sessionID)
	} else {
		if s.store.Settings().Fingerprint && os.Getenv("ZCODE_SYSTEM_FINGERPRINT") != "0" {
			mode := s.store.Settings().PromptMode
			if mode == "replace" {
				payload["system"] = cloneOfficialSystem()
			} else if mode == "append" || acc.Mode == "start-plan" {
				payload["system"] = withSystemFingerprint(payload["system"])
			}
		}
		payload["system"] = mergeClientSystem(payload["system"])
		// 三块形态开关：只在 fingerprint 路径生效（没有 head/prefix 时补第三块
		// 没有意义，反而会把官方 harness 文案塞给不用 fingerprint 的客户端）。
		if systemShapeThreeBlocks() && hasFingerprint(systemBlocks(payload["system"])) {
			payload["system"] = withThreeSystemBlocks(systemBlocks(payload["system"]))
		}
	}
	// 前缀缓存路由键：默认关闭，见 promptCacheKeyFor 的说明。
	if os.Getenv("ZCODE_PROMPT_CACHE_KEY") == "1" {
		if _, exists := payload["prompt_cache_key"]; !exists {
			if key := promptCacheKeyFor(acc, sessionKey); key != "" {
				payload["prompt_cache_key"] = key
			}
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	token := acc.ZcodeJWT
	if acc.Mode == "coding-plan" {
		token, err = s.ensureProjectToken(ctx, acc)
		if err != nil {
			return nil, nil, err
		}
	}
	if token == "" {
		return nil, nil, errors.New("account has no usable token")
	}

	endpoint := "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"
	if acc.Mode == "coding-plan" {
		logical := logicalEndpointZai
		if acc.Provider == "bigmodel" {
			logical = logicalEndpointBigmodel
			endpoint = "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
		} else {
			endpoint = "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
		}
		// 上游通过 agent/configs 下发「客户端 base URL → 实际端点」映射，
		// 命中就用它。上面的硬编码只是映射还没拉回来时的兜底。
		s.ensureEndpointMap(ctx, acc)
		if mapped := resolveEndpoint(logical); mapped != "" {
			endpoint = mapped
		}
	}
	if override := strings.TrimSpace(os.Getenv("ZCODE_UPSTREAM")); override != "" {
		endpoint = override
	}
	stream, _ := body["stream"].(bool)
	headers := identityHeaders(*s.store.EnsureProfile(acc))
	headers["Content-Type"] = "application/json"
	headers["Anthropic-Version"] = "2023-06-01"
	headers["Authorization"] = "Bearer " + token
	headers["X-Api-Key"] = token
	headers["X-Request-Id"] = randomUUID()
	headers["X-ZCode-Session-Type"] = "main"
	headers["X-ZCode-Trace-Id"] = randomUUID()
	headers["X-Query-Id"] = uuidV7()
	headers["X-ZCode-Query-Source"] = "main_turn"
	headers["X-Session-Id"] = sessionID
	if stream {
		headers["Accept"] = "text/event-stream"
	} else {
		headers["Accept"] = "application/json"
	}
	// anthropic-beta：默认只透传白名单里的能力位，其余丢弃（上游对不认识的
	// beta 会 400）。ZCODE_PASSTHROUGH_BETA=1 时整串透传，用于排查。
	beta := filterAnthropicBeta(clientHeaders["anthropic-beta"])
	if os.Getenv("ZCODE_PASSTHROUGH_BETA") == "1" {
		beta = strings.TrimSpace(clientHeaders["anthropic-beta"])
	}
	if beta != "" {
		headers["Anthropic-Beta"] = beta
	}
	if systemShape317() {
		applyHeaders317(headers, acc)
	}
	if acc.Mode == "coding-plan" && acc.ZcodeJWT != "" {
		headers["X-Zcode-Authorization"] = "Bearer " + acc.ZcodeJWT
	}
	if param := strings.TrimSpace(os.Getenv("ZCODE_CAPTCHA_PARAM")); param != "" {
		headers["X-Aliyun-Captcha-Verify-Param"] = param
		headers["X-Aliyun-Captcha-Verify-Region"] = envOr("ZCODE_CAPTCHA_REGION", "cn")
	}

	req, err := fhttp.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	doer := s.upstream.Do
	if viaEgress && s.upstream.egressClient != nil {
		doer = s.upstream.egressClient.Do
	}
	resp, err := doer(req)
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func cloneOfficialSystem() []any {
	out := make([]any, 0, len(officialSystemBlocks))
	for _, block := range officialSystemBlocks {
		copyMap, _ := block.(map[string]any)
		clone := make(map[string]any, len(copyMap))
		for k, v := range copyMap {
			clone[k] = v
		}
		out = append(out, clone)
	}
	return out
}

// systemShapeThreeBlocks 是「system 恰好 3 块」形态的开关（ZCODE_SYSTEM_SHAPE=3）。
//
// 背景：三家独立复刻都指出上游 harness 的 system 骨架恰好 3 块，客户端 system
// 并入末块。我们的 fingerprint 路径在客户端带 system 时本来就是 3 块，只有
// 「客户端没带 system」时是 2 块——实测这个形态 start-plan 也能跑通（2026-11-07），
// 所以默认关闭，等真的看到 3012/405 再开。
func systemShapeThreeBlocks() bool {
	return strings.TrimSpace(os.Getenv("ZCODE_SYSTEM_SHAPE")) == "3"
}

// officialHarnessTail 取官方 system 第 3 块里与机器无关的部分。
// "# Environment" 之后是工作目录/平台/OS 版本，抄过来会告诉模型一个假环境，
// 所以从那里截断。
func officialHarnessTail() string {
	if len(officialSystemBlocks) < 3 {
		return ""
	}
	text := blockText(officialSystemBlocks[2])
	if idx := strings.Index(text, "\n# Environment"); idx > 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

// withThreeSystemBlocks 在只有 [head, prefix] 时补上第三块，凑成官方骨架。
// 已经有 3 块（客户端 system 已并入末块）就原样返回。
func withThreeSystemBlocks(blocks []any) []any {
	if len(blocks) >= 3 {
		return blocks
	}
	tail := officialHarnessTail()
	if tail == "" {
		return blocks
	}
	out := make([]any, 0, len(blocks)+1)
	out = append(out, blocks...)
	return append(out, map[string]any{"type": "text", "text": tail})
}

// mergeClientSystem folds multiple client system blocks into one block.
// ZCode's upstream anti-abuse layer rejects requests that carry more than one
// client-authored system block in some combinations, while the concatenated
// text is accepted. Concatenation preserves every instruction; only the
// per-block cache hints are dropped.
func mergeClientSystem(system any) []any {
	blocks := systemBlocks(system)
	if os.Getenv("ZCODE_MERGE_SYSTEM") == "0" || len(blocks) == 0 {
		return blocks
	}
	start := 0
	if hasFingerprint(blocks) {
		start = 2
	}
	if len(blocks) <= start+1 {
		return blocks
	}
	var merged strings.Builder
	for _, block := range blocks[start:] {
		text := blockText(block)
		if text == "" {
			continue
		}
		if merged.Len() > 0 {
			merged.WriteString("\n\n")
		}
		merged.WriteString(text)
	}
	out := make([]any, 0, start+1)
	out = append(out, blocks[:start]...)
	out = append(out, map[string]any{"type": "text", "text": merged.String()})
	return out
}

func sanitizeForUpstream(payload map[string]any) {
	delete(payload, "context_management")
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		delete(thinking, "display")
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]any); ok {
				delete(toolMap, "strict")
			}
		}
	}
}

func withSystemFingerprint(system any) []any {
	blocks := systemBlocks(system)
	if hasFingerprint(blocks) {
		return blocks
	}
	out := []any{
		map[string]any{"type": "text", "text": SystemHead},
		map[string]any{"type": "text", "text": SystemPrefix},
	}
	out = append(out, blocks...)
	return out
}

func systemBlocks(system any) []any {
	switch v := system.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		return v
	case []map[string]any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	default:
		return nil
	}
}

func blockText(block any) string {
	m, ok := block.(map[string]any)
	if !ok {
		return ""
	}
	text, _ := m["text"].(string)
	return text
}

func hasFingerprint(blocks []any) bool {
	if len(blocks) < 2 {
		return false
	}
	if strings.TrimSpace(blockText(blocks[0])) != SystemHead {
		return false
	}
	second := blockText(blocks[1])
	return strings.HasPrefix(second, SystemPrefix[:min(64, len(SystemPrefix))])
}

func parseUsageFromJSON(data []byte) Usage {
	var parsed struct {
		Content []struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheReadTokens  int `json:"cache_read_input_tokens"`
			CacheWriteTokens int `json:"cache_creation_input_tokens"`
			ReasoningTokens  int `json:"reasoning_tokens"`
			OutputDetails    struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &parsed) != nil {
		return Usage{}
	}
	u := Usage{
		InputTokens:      parsed.Usage.InputTokens,
		OutputTokens:     parsed.Usage.OutputTokens,
		CacheReadTokens:  parsed.Usage.CacheReadTokens,
		CacheWriteTokens: parsed.Usage.CacheWriteTokens,
		ReasoningTokens:  parsed.Usage.ReasoningTokens,
	}
	if u.ReasoningTokens == 0 {
		u.ReasoningTokens = parsed.Usage.OutputDetails.ReasoningTokens
	}
	if u.ReasoningTokens == 0 {
		chars := 0
		for _, block := range parsed.Content {
			if block.Type == "thinking" {
				chars += len(block.Thinking)
			}
		}
		if chars > 0 {
			u.ReasoningTokens = chars / 4
		}
	}
	return u
}

type sseUsageTracker struct {
	usage         Usage
	thinkingChars int
	sawUsage      bool
}

func (t *sseUsageTracker) feed(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return false
	}
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return false
	}
	switch event["type"] {
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			if usage, ok := message["usage"].(map[string]any); ok {
				t.applyUsageMap(usage)
			}
		}
	case "message_delta":
		if usage, ok := event["usage"].(map[string]any); ok {
			t.applyUsageMap(usage)
		}
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			if delta["type"] == "thinking_delta" {
				if text, ok := delta["thinking"].(string); ok {
					t.thinkingChars += len(text)
				}
			}
			switch delta["type"] {
			case "text_delta", "thinking_delta", "input_json_delta":
				return true
			}
		}
	}
	return false
}

func (t *sseUsageTracker) applyUsageMap(usage map[string]any) {
	t.sawUsage = true
	if v, ok := number(usage["input_tokens"]); ok {
		t.usage.InputTokens = v
	}
	if v, ok := number(usage["output_tokens"]); ok {
		t.usage.OutputTokens = v
	}
	if v, ok := number(usage["cache_read_input_tokens"]); ok {
		t.usage.CacheReadTokens = v
	}
	if v, ok := number(usage["cache_creation_input_tokens"]); ok {
		t.usage.CacheWriteTokens = v
	}
	if v, ok := number(usage["reasoning_tokens"]); ok {
		t.usage.ReasoningTokens = v
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if v, ok := number(details["reasoning_tokens"]); ok {
			t.usage.ReasoningTokens = v
		}
	}
}

func (t *sseUsageTracker) finish() Usage {
	if t.usage.ReasoningTokens == 0 && t.thinkingChars > 0 {
		t.usage.ReasoningTokens = t.thinkingChars / 4
	}
	return t.usage
}

func number(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
