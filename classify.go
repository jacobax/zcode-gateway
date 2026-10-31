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
//   - Wait：账号本身健康，原地等一会儿再试同一个号（429 频控）
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
)

// 上游业务码（HTTP 200 或 4xx 都可能带）。
const (
	codeQuotaExhausted = 1005 // 额度/名额用完，属正常业务态
	codeNoResourcePack = 1113 // Key 没有资源包，永久态
	codeSessionExpired = 3004 // OAuth 会话过期
	codeCaptchaReject  = 3007 // 验证码被拒
	codeRiskControl    = 3012 // 风控：账号或网络环境异常
)

func classifyUpstream(status int, retryAfterHeader string, body []byte) upstreamVerdict {
	var parsed struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	msg := firstNonEmpty(parsed.Msg, parsed.Error.Message)
	reason := firstNonEmpty(msg, fmt.Sprintf("上游 HTTP %d", status))
	text := strings.ToLower(msg)

	switch {
	case parsed.Code == codeCaptchaReject:
		// 码被拒：下一次请求会重新求解，账号和模型都不该背锅。
		return upstreamVerdict{Retry: true, Reason: firstNonEmpty(msg, "验证码被拒")}

	case parsed.Code == codeNoResourcePack:
		// Key 没有资源包：等多久都没用，直接换号且不冷却。
		return upstreamVerdict{Retry: true, ModelScoped: true, RateLimited: true, Reason: firstNonEmpty(msg, "无可用资源包")}

	case parsed.Code == codeSessionExpired:
		return upstreamVerdict{Retry: true, Disable: true, Cooldown: authCooldown, Reason: firstNonEmpty(msg, "OAuth 会话过期")}

	case status == 429:
		// 频控：账号是健康的，按 Retry-After 原地等一下再打同一个号。
		return upstreamVerdict{Retry: true, Wait: parseRetryAfter(retryAfterHeader), RateLimited: true, Reason: firstNonEmpty(msg, "上游限流 429")}

	case status == 401 || status == 403:
		return upstreamVerdict{Retry: true, Disable: true, Cooldown: authCooldown, Reason: firstNonEmpty(msg, "认证失败")}

	case parsed.Code == codeRiskControl || status == 405:
		// 风控不原地重试：对已被标记的账号持续施压只会加剧。
		return upstreamVerdict{Retry: true, Risk: true, Reason: firstNonEmpty(msg, "上游风控 3012")}

	case status >= 500:
		return upstreamVerdict{Retry: true, Cooldown: serverErrorCooldown, Reason: firstNonEmpty(msg, fmt.Sprintf("上游 HTTP %d", status))}

	case isContentPolicyReject(text):
		// 提示词被判违规：多半是 system 指纹误报，换号即可，别记账号的账。
		return upstreamVerdict{Retry: true, Reason: firstNonEmpty(msg, "内容策略拦截")}
	}

	return upstreamVerdict{Retry: false, Reason: reason}
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
