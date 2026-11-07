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
	if env.Code == 0 || env.Code == 200 {
		return upstreamVerdict{}, false
	}
	verdict, ok := businessCodeVerdict(env.Code, firstNonEmpty(env.Msg, "上游业务错误"))
	if !ok {
		return upstreamVerdict{}, false
	}
	return verdict, true
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
