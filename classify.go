package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// upstreamVerdict 是上游一次失败的可执行结论。
//
// 关键是把三件事分开表达，而不是压成一个 cooldown 数字：
//   - Retry：要不要换号重试
//   - Wait：账号本身健康，原地等一会儿再试同一个号（429 频控 / 模型并发）
//   - Cooldown / Disable：这次失败对账号造成了多大伤害
//
// 混在一起就会把健康产能踢出池：429 是频控不是故障，冷却它等于自己减员。
type upstreamVerdict struct {
	Retry       bool
	Wait        time.Duration
	Cooldown    time.Duration
	Disable     bool
	Risk        bool // 3012 类风控：冷却时长按 strike 指数退避，由调用方算
	ModelScoped bool // 只影响这个模型，不动账号
	RateLimited bool
	Reason      string
}

const (
	authCooldown        = 30 * time.Minute
	serverErrorCooldown = 5 * time.Minute
	riskCooldownBase    = 15 * time.Minute
	riskCooldownMax     = 24 * time.Hour
	defaultRetryAfter   = 5 * time.Second
	maxRetryAfter       = 120 * time.Second
	modelBusyRetryAfter = time.Second
)

// 上游业务码（HTTP 200 或 4xx 都可能带）。
const (
	codeQuotaExhausted = 1005 // 额度/名额用完，属正常业务态
	codeNoResourcePack = 1113 // Key 没有资源包，永久态
	codeSessionExpired = 3004 // OAuth 会话过期
	codeCaptchaReject  = 3007 // 验证码被拒
	codeModelBusy      = 3009 // 模型并发准入（dengyie 口径）
	codeModelAdmit     = 3010 // 模型并发准入（zcode2api-plus 口径）
	codeRiskControl    = 3012 // 风控：账号或网络环境异常
)

// quotaExhaustedCodes 是「这个号在这份套餐上没额度了」的码族。
//
// 处理方式统一：换号 + 模型级熔断，不冷却账号——额度用尽是正常业务态，
// 冷却它只会把还有额度的号也一起误伤。13xx 全族来自 zcode2api-plus 的实测，
// 我们原来只认 1113，其余全部落到默认分支透传给客户端。
var quotaExhaustedCodes = map[int]bool{
	1005: true, // 额度/名额用完
	1113: true, // Key 没有资源包
	1308: true, // 用量上限
	1309: true, // 套餐过期
	1310: true, // 周/月上限
	1311: true, // 套餐不含此模型
	1313: true, // 公平使用限制
	1316: true, // 5 小时上限
	1317: true, // 7 天上限
	1318: true, // 子账户上限
	1319: true, // 企业消费上限
	1320: true,
	1321: true,
}

// transientRateLimitCodes 是瞬时限流：账号健康，按短退避换号。
var transientRateLimitCodes = map[int]bool{
	1302: true,
	1305: true,
}

// captchaChallengeHeaders 是阿里云验证码挑战的响应头。
//
// 上游在 400/403 上带这些头时，语义是「重新解一次验证码」——账号本身没问题。
// 只看 body 的 3007 会漏掉这类挑战，然后落进「401/403 → Disable」把好号停用
// 30 分钟（dengyie 的注释专门警告过这个坑）。
var captchaChallengeHeaders = []string{
	"x-aliyun-captcha-verify-param",
	"x-aliyun-captcha-verify-region",
	"x-aliyun-captcha-challenge",
}

// upstreamEnvelope 是上游响应里的业务码与文案。
type upstreamEnvelope struct {
	Code int
	Msg  string
}

// parseUpstreamEnvelope 从三种响应形态里提取业务码：
//
//	{"code":1005,"msg":"..."}                     zcode-plan 包装
//	{"error":{"code":1005,"message":"..."}}       Anthropic 风格
//	{"error":{"type":"1000","message":"..."}}     api.z.ai 回退通道（码在 type 里）
//
// type 既可能是数字字符串（"1000"），也可能是符号名（"rate_limit_error"），
// 后者解析不出数字，不参与业务码判断。
func parseUpstreamEnvelope(body []byte) upstreamEnvelope {
	var parsed struct {
		Code  json.RawMessage `json:"code"`
		Msg   string          `json:"msg"`
		Error struct {
			Code    json.RawMessage `json:"code"`
			Type    json.RawMessage `json:"type"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return upstreamEnvelope{}
	}
	return upstreamEnvelope{
		Code: firstInt(parsed.Code, parsed.Error.Code, parsed.Error.Type),
		Msg:  firstNonEmpty(parsed.Msg, parsed.Error.Message),
	}
}

// headerGetter 让 classifyUpstream 同时接受 net/http.Header 和 fhttp.Header
// ——上游响应头是 fhttp 的类型，测试里构造的是 net/http 的，两者不能直接互转。
type headerGetter interface {
	Get(string) string
}

// firstInt 返回第一个能解析成整数的值（数字或数字字符串）。
func firstInt(values ...json.RawMessage) int {
	for _, raw := range values {
		if len(raw) == 0 {
			continue
		}
		var n int
		if json.Unmarshal(raw, &n) == nil {
			return n
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				return n
			}
		}
	}
	return 0
}

// classifyUpstream 把一次非 200 响应翻成可执行结论。
//
// 顺序很重要：验证码挑战要排在「401/403 → Disable」前面，业务码要排在
// HTTP 状态前面——上游会用 200/400 承载同一套业务码。
func classifyUpstream(status int, headers headerGetter, body []byte) upstreamVerdict {
	env := parseUpstreamEnvelope(body)
	reason := firstNonEmpty(env.Msg, fmt.Sprintf("上游 HTTP %d", status))
	text := strings.ToLower(env.Msg)

	// 验证码挑战优先：它可能以 400/403 出现，body 里不一定有 3007。
	if verdict, ok := captchaChallengeVerdict(status, headers, env, text); ok {
		return verdict
	}

	if env.Code != 0 && env.Code != 200 {
		if verdict, ok := businessCodeVerdict(env.Code, reason); ok {
			// 瞬时限流码在 429 上以 Retry-After 为准——服务端给了具体秒数，
			// 比默认 5 秒更准。
			if status == 429 && transientRateLimitCodes[env.Code] {
				verdict.Wait = parseRetryAfter(headers.Get("Retry-After"))
			}
			return verdict
		}
	}

	if status == 429 {
		// 频控：账号是健康的，按 Retry-After 原地等一下再打同一个号。
		return upstreamVerdict{
			Retry: true, Wait: parseRetryAfter(headers.Get("Retry-After")),
			RateLimited: true, Reason: firstNonEmpty(env.Msg, "上游限流 429"),
		}
	}

	switch {
	case status == 401 || status == 403:
		return upstreamVerdict{Retry: true, Disable: true, Cooldown: authCooldown, Reason: firstNonEmpty(env.Msg, "认证失败")}

	case status == 405:
		// 风控不原地重试：对已被标记的账号持续施压只会加剧。
		return upstreamVerdict{Retry: true, Risk: true, Reason: firstNonEmpty(env.Msg, "上游风控 3012")}

	case status >= 500:
		return upstreamVerdict{Retry: true, Cooldown: serverErrorCooldown, Reason: reason}

	case isContentPolicyReject(text):
		// 提示词被判违规：多半是 system 指纹误报，换号即可，别记账号的账。
		return upstreamVerdict{Retry: true, Reason: firstNonEmpty(env.Msg, "内容策略拦截")}
	}

	return upstreamVerdict{Retry: false, Reason: reason}
}

// businessCodeVerdict 把业务码翻成结论。ok=false 表示这个码没有已知语义，
// 调用方继续按 HTTP 状态判断，别把未知码当成错误吞掉。
func businessCodeVerdict(code int, reason string) (upstreamVerdict, bool) {
	switch {
	case code == codeSessionExpired:
		return upstreamVerdict{Retry: true, Disable: true, Cooldown: authCooldown, Reason: reason}, true

	case code == codeRiskControl:
		return upstreamVerdict{Retry: true, Risk: true, Reason: reason}, true

	case code == codeCaptchaReject:
		// 码被拒：下一次请求会重新求解，账号和模型都不该背锅。
		return upstreamVerdict{Retry: true, Reason: reason}, true

	case quotaExhaustedCodes[code]:
		return upstreamVerdict{Retry: true, ModelScoped: true, RateLimited: true, Reason: reason}, true

	case transientRateLimitCodes[code]:
		return upstreamVerdict{Retry: true, Wait: defaultRetryAfter, RateLimited: true, Reason: reason}, true

	case code == codeModelBusy || code == codeModelAdmit:
		// 模型级并发准入：账号没坏，等一秒重来（原版客户端也是 1s/2s 退避）。
		return upstreamVerdict{Retry: true, ModelScoped: true, Wait: modelBusyRetryAfter, Reason: reason}, true
	}
	return upstreamVerdict{}, false
}

// captchaChallengeVerdict 识别「要求重新验证」的响应。
//
// 三条独立证据：响应头、body 里的 3007、以及只对 400/403 生效的文案匹配。
// 文案限定状态码是为了避开 5xx 里偶然出现的 "verify" 字样。
func captchaChallengeVerdict(status int, headers headerGetter, env upstreamEnvelope, text string) (upstreamVerdict, bool) {
	if env.Code == codeCaptchaReject {
		return upstreamVerdict{Retry: true, Reason: firstNonEmpty(env.Msg, "验证码被拒")}, true
	}
	if status != 400 && status != 403 {
		return upstreamVerdict{}, false
	}
	if headers != nil {
		for _, name := range captchaChallengeHeaders {
			if headers.Get(name) != "" {
				return upstreamVerdict{Retry: true, Reason: "上游要求重新验证（验证码挑战）"}, true
			}
		}
	}
	for _, needle := range []string{"captcha", "f001", "verify token", "verify failed"} {
		if strings.Contains(text, needle) {
			return upstreamVerdict{Retry: true, Reason: firstNonEmpty(env.Msg, "验证码挑战")}, true
		}
	}
	return upstreamVerdict{}, false
}

func isContentPolicyReject(text string) bool {
	for _, needle := range []string{
		"unapproved channel",
		"illegal api invocation",
		"blocked by security policy",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// parseRetryAfter 读 Retry-After（秒数或 HTTP 日期），夹在 [0, 120s]。
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultRetryAfter
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0
		}
		wait := time.Duration(secs) * time.Second
		if wait > maxRetryAfter {
			return maxRetryAfter
		}
		return wait
	}
	if when, err := http.ParseTime(raw); err == nil {
		wait := time.Until(when)
		if wait < 0 {
			return 0
		}
		if wait > maxRetryAfter {
			return maxRetryAfter
		}
		return wait
	}
	return defaultRetryAfter
}

// riskCooldownFor 把连续风控次数翻成冷却时长：15 分钟起，逐次翻倍，封顶 24 小时。
func riskCooldownFor(strikes int) time.Duration {
	if strikes < 1 {
		strikes = 1
	}
	cooldown := riskCooldownBase
	for i := 1; i < strikes; i++ {
		cooldown *= 2
		if cooldown >= riskCooldownMax {
			return riskCooldownMax
		}
	}
	return cooldown
}
