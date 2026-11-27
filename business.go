package main

// 上游会用 HTTP 200 承载业务失败。典型的是没套餐的号回
// {"code":1005,"msg":"exceed quota limit"}，以及额度耗尽后的 13xx 族。
//
// 之前 handlers.go 只按 status 判断，200 就直接渲染 + 记成功 + ResetRiskStrikes。
// 三个后果：
//  1. 错误 JSON 原样塞给客户端（OpenAI SDK 会当成空回复）
//  2. 日志里全是成功，排障时看不到真实失败率
//  3. ResetRiskStrikes 把 3012 的 strike 计数洗掉 → 指数退避永远升不上去
//
// 这里只做一件事：peek 响应体开头，判断有没有业务形状。有形状的一律放行，
// 避免把正常响应误判成错误（dengyie 的教训：裸子串匹配比漏判更糟）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

const businessPeekLimit = 4096

// replayBody 让 peek 过的响应体能被后续读取方完整读到。
type replayBody struct {
	io.Reader
	inner io.ReadCloser
}

func (r *replayBody) Close() error { return r.inner.Close() }

// peekBusinessError 判断 200 响应里是不是塞了业务错误码。
// 返回 ok=true 时 body 已还原，调用方按失败处理即可。
func peekBusinessError(resp *fhttp.Response) (upstreamVerdict, []byte, bool) {
	if resp.Body == nil {
		return upstreamVerdict{}, nil, false
	}
	// SSE 一定是正常流：业务错误不会用 event-stream 回。
	if strings.Contains(strings.ToLower(resp.Header.Get("content-type")), "text/event-stream") {
		return upstreamVerdict{}, nil, false
	}
	head := make([]byte, businessPeekLimit)
	n, _ := io.ReadFull(resp.Body, head)
	head = head[:n]
	resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), inner: resp.Body}
	if n == 0 {
		return upstreamVerdict{}, nil, false
	}
	verdict, ok := businessErrorVerdict(head)
	return verdict, head, ok
}

// businessErrorVerdict 在已 peek 的内容里找业务错误。
//
// 判定口径：合法响应一定带业务形状（messages 有 content，OpenAI 有 choices，
// billing 有 data）。所以「没有业务形状 + code 是非 0/200 的已知码」才算错误。
// JSON 解析失败（多半是被 4KB 截断）一律当正常，宁可漏判不误判。
func businessErrorVerdict(body []byte) (upstreamVerdict, bool) {
	var probe map[string]any
	if json.Unmarshal(body, &probe) != nil {
		return upstreamVerdict{}, false
	}
	for _, key := range []string{"choices", "data", "content", "candidates"} {
		if _, ok := probe[key]; ok {
			return upstreamVerdict{}, false
		}
	}
	if kind, ok := probe["type"].(string); ok && kind != "error" {
		// Anthropic message 的 type 是 "message"；"error" 留给下面按码判断。
		return upstreamVerdict{}, false
	}
	env := parseUpstreamEnvelope(body)
	// 2026-11-27 上游新增一种「空包」：客户端版本不匹配时回
	// 200 + {"code":0,"msg":"","logid":"..."}，没有任何业务形状、也没有报错。
	// 当成成功会把空响应当正文交给客户端——流式表现就是
	// 「Anthropic stream ended without a stop reason」（magpie plugins#1639）。
	// 这里判成上游异常：换号重试，全池耗尽时由 businessErrorStatus 回 502，
	// 让失败可见，而不是假装成功。
	if emptyEnvelope(probe) {
		return upstreamVerdict{Retry: true, Reason: "上游空包（本次未服务）"}, true
	}
	if env.Code == 0 || env.Code == 200 {
		return upstreamVerdict{}, false
	}
	verdict, ok := businessCodeVerdict(env.Code, firstNonEmpty(env.Msg, "上游业务错误"))
	if !ok {
		return upstreamVerdict{}, false
	}
	return verdict, true
}

// emptyEnvelope 判断 200 响应是不是「什么也没服务」的空包。
//
// 形状（实测 2026-11-27）：{"code":0,"msg":"","logid":"20261127164646..."}，
// 通常 60~70 字节，没有 content / choices / data / type 等任何业务字段。
// 只认「键集合都是 code/msg/logid/request_id/error 且 code 为 0」，避免把将来
// 新增的正常字段误判成空包。
func emptyEnvelope(probe map[string]any) bool {
	if len(probe) == 0 {
		return false
	}
	allowed := map[string]bool{"code": true, "msg": true, "message": true, "logid": true, "request_id": true, "error": true}
	for key := range probe {
		if !allowed[key] {
			return false
		}
	}
	// msg/message 必须为空——`{"code":0,"msg":"ok"}` 是正常业务回执，不能误判。
	for _, key := range []string{"msg", "message"} {
		if v, ok := probe[key]; ok {
			if s, _ := v.(string); strings.TrimSpace(s) != "" {
				return false
			}
		}
	}
	if code, ok := probe["code"]; ok {
		switch v := code.(type) {
		case float64:
			return v == 0
		case string:
			return v == "0" || v == ""
		}
	}
	return false
}

// businessErrorStatus 把「200 + 业务错误码」翻成该给的 HTTP 状态。
//
// 为什么必须翻：池子里所有号都试过还是业务失败时，如果按 200 原样回给客户端，
// OpenAI SDK 会把错误体当成空回复（不抛异常），客户端只看到「模型不说话」，
// 排障时也看不出是额度问题。返回 0 表示不是业务错误，调用方保持原状态。
func businessErrorStatus(body []byte) int {
	verdict, ok := businessErrorVerdict(body)
	if !ok {
		return 0
	}
	if verdict.RateLimited || verdict.Wait > 0 {
		// 额度耗尽、瞬时限流、模型并发准入：都是「等一会儿再来」。
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}
