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
