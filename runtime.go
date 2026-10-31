package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

var appVersion = envOr("ZCODE_APP_VERSION", "3.16.3")

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
