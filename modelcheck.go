package main

// 模型白名单：在转发前拒绝客户端请求了、但上游肯定不认的模型名。
//
// 为什么要有这个：上游对不认识的模型回 400 / 1005，白白花掉一次上游请求，
// 还会在账号上留一条失败记录、影响额度判断。提前拒绝的代价是零。
//
// 注意它和 ZCODE_MODEL 的关系：设了 ZCODE_MODEL 时所有请求都会被改写成那个
// 模型，白名单校验的是「客户端请求的名字」，那种配置下基本不会触发。

import (
	"os"
	"strings"
)

// modelAllowlist 解析 ZCODE_MODEL_ALLOWLIST（逗号分隔，大小写不敏感）。
// 返回 nil 表示不限制。
func modelAllowlist() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("ZCODE_MODEL_ALLOWLIST"))
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if name := strings.ToLower(strings.TrimSpace(part)); name != "" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// modelAllowed 判断客户端请求的模型是否放行。白名单为空时一律放行。
func modelAllowed(model string) bool {
	allow := modelAllowlist()
	if allow == nil {
		return true
	}
	return allow[strings.ToLower(strings.TrimSpace(model))]
}
