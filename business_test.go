package main

import "testing"

// 上游用 HTTP 200 承载业务失败，必须识别出来——否则会把错误 JSON 渲染给客户端、
// 记成功日志，还会 ResetRiskStrikes 洗掉 3012 的 strike 计数。
func TestBusinessErrorVerdict(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantOK  bool
		wantRet bool
		wantMod bool
	}{
		{
			name: "1005 额度用完", body: `{"code":1005,"msg":"exceed quota limit"}`,
			wantOK: true, wantRet: true, wantMod: true,
		},
		{
			name: "1308 用量上限", body: `{"code":1308,"msg":"usage limit reached"}`,
			wantOK: true, wantRet: true, wantMod: true,
		},
		{
			name: "3004 会话过期要停用", body: `{"code":3004,"msg":"session expired"}`,
			wantOK: true, wantRet: true,
		},
		{
			name: "error.type 形态", body: `{"error":{"type":"1113","message":"no resource pack"}}`,
			wantOK: true, wantRet: true, wantMod: true,
		},
		// 有业务形状的一律放行，哪怕里面也带了 code。
		{name: "Anthropic message", body: `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`},
		{name: "OpenAI choices", body: `{"id":"x","choices":[{"message":{"content":"hi"}}]}`},
		{name: "billing data", body: `{"code":0,"data":{"balances":[]}}`},
		// 未知码不判错：宁可漏判，也别把正常响应吃掉。
		{name: "未知码", body: `{"code":9999,"msg":"who knows"}`},
		{name: "正常 code 0", body: `{"code":0,"msg":"ok"}`},
		// 被 4KB 截断的 JSON 解析不出来，当正常处理。
		{name: "截断 JSON", body: `{"type":"message","content":[{"type":"text","text":"aaa`},
		{name: "空 body", body: ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict, ok := businessErrorVerdict([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (verdict %#v)", ok, c.wantOK, verdict)
			}
			if !ok {
				return
			}
			if verdict.Retry != c.wantRet {
				t.Fatalf("Retry = %v, want %v", verdict.Retry, c.wantRet)
			}
			if verdict.ModelScoped != c.wantMod {
				t.Fatalf("ModelScoped = %v, want %v", verdict.ModelScoped, c.wantMod)
			}
		})
	}
}

// 13xx 全族都要落进「换号 + 模型级」，不能透传给客户端。
func TestQuotaExhaustedCodesCoverage(t *testing.T) {
	for _, code := range []int{1005, 1113, 1308, 1309, 1310, 1311, 1313, 1316, 1317, 1318, 1319, 1320, 1321} {
		verdict, ok := businessCodeVerdict(code, "test")
		if !ok {
			t.Fatalf("code %d not recognised", code)
		}
		if !verdict.Retry || !verdict.ModelScoped || !verdict.RateLimited {
			t.Fatalf("code %d should switch account as model-scoped: %#v", code, verdict)
		}
		if verdict.Disable || verdict.Cooldown != 0 || verdict.Risk {
			t.Fatalf("code %d must not damage the account: %#v", code, verdict)
		}
	}
}

// 池子耗尽后仍按 200 回业务错误，OpenAI SDK 会当成空回复；状态必须映射。
func TestBusinessErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "1005 额度用完", body: `{"code":1005,"msg":"exceed quota limit"}`, want: 429},
		{name: "1308 用量上限", body: `{"code":1308,"msg":"usage limit reached"}`, want: 429},
		{name: "3009 模型并发", body: `{"code":3009,"msg":"model busy"}`, want: 429},
		{name: "3012 风控", body: `{"code":3012,"msg":"account or network abnormal"}`, want: 502},
		{name: "3004 会话过期", body: `{"code":3004,"msg":"session expired"}`, want: 502},
		// 不是业务错误：调用方保持原状态。
		{name: "正常 message", body: `{"type":"message","content":[{"type":"text","text":"hi"}]}`},
		{name: "正常 code 0", body: `{"code":0,"msg":"ok"}`},
		{name: "未知码", body: `{"code":9999,"msg":"who knows"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := businessErrorStatus([]byte(c.body)); got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
}
