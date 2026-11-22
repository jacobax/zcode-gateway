package main

import (
	"testing"
	"time"
)

// 2026-11-22 实证：6 个坏请求把 42 个号全打进风控冷却（全池 503）。
// 两个护栏各自钉一个测试，防以后被"简化"掉。

func TestIsClientShapeReject(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"官方文案·英文", `{"error":{"message":"This client is not supported. Please use the official ZCode client.","code":"3012"}}`, true},
		{"官方文案·中文", `{"code":3012,"msg":"不支持的客户端，请使用官方 ZCode"}`, true},
		{"行为风控（账号级）", `{"error":{"message":"request has been blocked due to unusual activity.","code":"3012"}}`, false},
		{"中文账号级风控", `{"code":3012,"msg":"账号或者网络环境存在异常"}`, false},
		{"空 body", ``, false},
		{"正常响应", `{"content":[{"type":"text","text":"hi"}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isClientShapeReject([]byte(c.body)); got != c.want {
				t.Fatalf("isClientShapeReject(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestRiskCooldownEscalatesAndCaps(t *testing.T) {
	if got := riskCooldownFor(0); got != riskCooldownBase {
		t.Fatalf("strikes=0 -> %s, want base %s", got, riskCooldownBase)
	}
	if got := riskCooldownFor(1); got != riskCooldownBase {
		t.Fatalf("strikes=1 -> %s, want base %s", got, riskCooldownBase)
	}
	if got := riskCooldownFor(2); got != riskCooldownBase*2 {
		t.Fatalf("strikes=2 -> %s, want %s", got, riskCooldownBase*2)
	}
	if got := riskCooldownFor(99); got != riskCooldownMax {
		t.Fatalf("strikes=99 -> %s, want cap %s", got, riskCooldownMax)
	}
	if riskCooldownMax > 24*time.Hour {
		t.Fatalf("cap %s exceeds 24h", riskCooldownMax)
	}
}

func TestEnvIntFallsBackOnBadValues(t *testing.T) {
	t.Setenv("ZCODE_TEST_INT", "")
	if got := envInt("ZCODE_TEST_INT", 3); got != 3 {
		t.Fatalf("empty -> %d, want fallback 3", got)
	}
	t.Setenv("ZCODE_TEST_INT", "abc")
	if got := envInt("ZCODE_TEST_INT", 3); got != 3 {
		t.Fatalf("non-numeric -> %d, want fallback 3", got)
	}
	t.Setenv("ZCODE_TEST_INT", "0")
	if got := envInt("ZCODE_TEST_INT", 3); got != 3 {
		t.Fatalf("zero -> %d, want fallback 3 (0 会让风控轮转停摆)", got)
	}
	t.Setenv("ZCODE_TEST_INT", "5")
	if got := envInt("ZCODE_TEST_INT", 3); got != 5 {
		t.Fatalf("5 -> %d, want 5", got)
	}
}
