package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// appVersion 是模拟的 ZCode 桌面客户端版本，用于模型请求（/v1/messages 等）。
//
// 它和活动/领取上报的版本（claimAppVersion()，见 claimversion.go）必须分开：
// 实测（2026-11-03，同一账号同一套头只改版本号）模型接口报 3.17.x 会被上游判
// 3012，而活动接口报 3.16.3 则看不到横幅、领取回 1004。两边门槛相反，不能合并。
var appVersion = envOr("ZCODE_APP_VERSION", "3.16.3")

// buildVersion 是网关自己的版本，发布时用 -X main.buildVersion=<tag> 注入。
// 注意它和 appVersion 不是一回事：后者是模拟的 ZCode 客户端版本。
var buildVersion = "dev"

func envSeconds(key string, fallback int) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Duration(fallback) * time.Second
}

// envSecondsAllowZero 与 envSeconds 相同，但允许 0：有些预算用 0 表示「关闭」。
func envSecondsAllowZero(key string, fallback int) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Duration(fallback) * time.Second
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// upstreamBase 是额度/活动/OAuth 这一族接口的根地址，默认 zcode.z.ai；
// 自建镜像时用 ZCODE_UPSTREAM_BASE 覆盖。
//
// 注意它和 ZCODE_UPSTREAM 不是一回事：后者覆盖的是模型转发端点的整条 URL
// （端到端测试也靠它指向 httptest），只影响 /v1/messages 那条路。
func upstreamBase() string {
	if v := strings.TrimSpace(os.Getenv("ZCODE_UPSTREAM_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://zcode.z.ai"
}
