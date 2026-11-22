package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// 2026-11-22 第二次实证：直连 aws1 被阿里云 WAF 挡回 HTML 405，body 里没有
// 业务码，于是 Reason 落成 fallback 文案「上游风控 3012」，20 个号被记 strike。
// 判定必须把「没进业务层的拦截页」和「业务层 3012」分开。
const aliyunWAF405 = `<!doctypehtml><html lang="zh-cn"><meta charset="utf-8">` +
	`<meta name="data-spm" content="a3c0e"><title>405</title>` +
	`<style>a,body{margin:0}</style><body><h2>405</h2></body></html>`

func TestIsUpstreamWAFBlock(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"阿里云 WAF 405 页面", aliyunWAF405, true},
		{"带空格的 doctype", "<!DOCTYPE html><html><title>405</title></html>", true},
		{"业务 3012 JSON", `{"code":3012,"msg":"This client is not supported. Please use the official ZCode client."}`, false},
		{"业务账号风控 JSON", `{"code":3012,"msg":"账号或者网络环境存在异常"}`, false},
		{"正常响应", `{"content":[{"type":"text","text":"hi"}]}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamWAFBlock([]byte(c.body)); got != c.want {
				t.Fatalf("isUpstreamWAFBlock(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// WAF 405 走 InfraBlock：要 Retry（换出口/换号还有救），但绝不能 Risk
// （Risk 会 BumpRiskStrikes + MarkExhausted，把 WAF 抖动摊成账号冷却）。
func TestClassifyUpstream405SplitsWAFFromRisk(t *testing.T) {
	waf := classifyUpstream(405, nil, []byte(aliyunWAF405))
	if !waf.Retry {
		t.Fatal("WAF 405 应该 Retry")
	}
	if !waf.InfraBlock {
		t.Fatal("WAF 405 应该判 InfraBlock")
	}
	if waf.Risk {
		t.Fatal("WAF 405 不能判 Risk：那会给账号记 strike")
	}

	biz := classifyUpstream(405, nil, []byte(`{"code":3012,"msg":"账号或者网络环境存在异常"}`))
	if !biz.Risk {
		t.Fatal("业务 3012 应该判 Risk")
	}
	if biz.InfraBlock {
		t.Fatal("业务 3012 不是 InfraBlock")
	}
}

// rotateEgress 必须节流：中继的 /rotate 会断开正在传输的隧道。
// 2026-11-22 实证：上游每次 405 都触发轮换，一小时 214 次，单请求耗时被拉到
// 47~54 秒并产生成片 502。
func TestRotateEgressThrottled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("ZCODE_RELAY_ADMIN", srv.URL)
	t.Setenv("ZCODE_EGRESS_ROTATE_MIN_SEC", "30")

	s := &Server{}
	for i := 0; i < 5; i++ {
		s.rotateEgress(context.Background())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("30s 窗口内 5 次调用只应打出 1 次轮换，实际 %d", got)
	}
}

// 拨回时间戳后应允许再次轮换；设 0 表示完全不节流（仅排障用）。
func TestRotateEgressWindowExpires(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("ZCODE_RELAY_ADMIN", srv.URL)
	t.Setenv("ZCODE_EGRESS_ROTATE_MIN_SEC", "30")

	s := &Server{}
	s.rotateEgress(context.Background())
	s.lastEgressRotate.Store(time.Now().Add(-31 * time.Second).Unix())
	s.rotateEgress(context.Background())
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("窗口过期后应再轮换一次（共 2 次），实际 %d", got)
	}

	t.Setenv("ZCODE_EGRESS_ROTATE_MIN_SEC", "0")
	s2 := &Server{}
	s2.rotateEgress(context.Background())
	s2.rotateEgress(context.Background())
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Fatalf("MIN_SEC=0 时不应节流（累计 4 次），实际 %d", got)
	}
}
