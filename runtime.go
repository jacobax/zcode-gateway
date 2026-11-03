package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

var appVersion = envOr("ZCODE_APP_VERSION", "3.16.3")

// claimAppVersion 是活动/领取这一族接口上报的客户端版本。
//
// 为什么和 appVersion 分开：上游的活动是按客户端版本下发的。实测
// （2026-11-03，同一个账号、同一套头，只改版本号）——
//
//	3.15.0 / 3.16.3 / 3.16.4 / 3.17.0 → marketing/touch 回 deliveries:[]，
//	                                     领取回 1004「暂不满足领取条件」
//	3.17.1 / 3.18.0                    → 能看到横幅，领取直接 200
//
// 但模型请求（/v1/messages 等）用 3.17.1 会被上游判 3012，所以两边不能
// 共用同一个版本号：模型保持 appVersion，活动和领取用这个。
var claimAppVersion = envOr("ZCODE_CLAIM_APP_VERSION", "3.17.1")

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
