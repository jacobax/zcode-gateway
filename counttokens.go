package main

// Anthropic 的 /v1/messages/count_tokens：客户端（Claude Code 这类）在真正发
// 请求前先问一句「这段上下文要多少 token」，用来决定要不要压缩历史。
//
// 上游没有这个端点，我们本地估算。为什么不去问上游：这个接口在客户端的
// 热路径上（每轮都可能调），多一次上游往返既慢又浪费账号额度，而客户端
// 只需要一个稳定、数量级正确的数字。
//
// 估算规则（刻意偏保守，宁可高估：高估只会让客户端早点压缩，低估会让它
// 带着超长上下文撞上游的 1210）：
//   - ASCII：4 字符 ≈ 1 token（英文经验值）
//   - CJK（汉字/平假名/片假名/谚文）：1 字符 ≈ 1 token
//   - 其他非 ASCII（带音标拉丁、西里尔、阿拉伯……）：2 字符 ≈ 1 token
//
// 只做确定性计算：同样输入永远同样输出，客户端可以安全地缓存比较结果。
// JSON 的键名不计入——它们是结构不是内容，量级上可以忽略。

import (
	"encoding/json"
	"io"
	"net/http"
	"unicode"
)

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, 401, "invalid local proxy token")
		return
	}
	var body any
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeError(w, 400, "request body is not valid JSON")
		return
	}
	writeJSON(w, 200, map[string]any{"input_tokens": estimateTokens(body)})
}

// estimateTokens 递归收集请求体里所有字符串，按字符类别折算成 token 数。
func estimateTokens(node any) int {
	ascii, wide, other := 0, 0, 0
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			a, w, o := countRuneClasses(v)
			ascii += a
			wide += w
			other += o
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(node)
	return (ascii+3)/4 + wide + (other+1)/2
}

// countRuneClasses 把字符串里的字符分成三类：ASCII、CJK 宽字符、其他非 ASCII。
func countRuneClasses(text string) (ascii, wide, other int) {
	for _, r := range text {
		switch {
		case r < 0x80:
			ascii++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			wide++
		default:
			other++
		}
	}
	return ascii, wide, other
}
