package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestOpenAIRequestConversion(t *testing.T) {
	body := map[string]any{
		"model": "GLM-5.3",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "北京天气"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "weather", "arguments": `{"city":"北京"}`},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
		},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
		"max_tokens":  256,
		"temperature": 0.3,
		"stop":        "END",
	}
	converted, err := openAIRequestToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	if converted["system"] != "你是助手" {
		t.Fatalf("system not lifted out: %#v", converted["system"])
	}
	if converted["max_tokens"] != 256 {
		t.Fatalf("max_tokens not mapped: %#v", converted["max_tokens"])
	}
	if seq, ok := converted["stop_sequences"].([]any); !ok || len(seq) != 1 || seq[0] != "END" {
		t.Fatalf("stop not mapped to stop_sequences: %#v", converted["stop_sequences"])
	}
	messages := converted["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected user/assistant/user(tool_result), got %d: %#v", len(messages), messages)
	}
	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant tool_call not converted: %#v", blocks)
	}
	if blocks[0].(map[string]any)["name"] != "weather" {
		t.Fatalf("tool name lost: %#v", blocks[0])
	}
	toolResult := messages[2].(map[string]any)
	resultBlocks := toolResult["content"].([]any)
	if resultBlocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("tool message not converted to tool_result: %#v", resultBlocks)
	}
	tools := converted["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "weather" {
		t.Fatalf("tools not converted: %#v", tools)
	}
	if _, ok := tools[0].(map[string]any)["input_schema"]; !ok {
		t.Fatalf("input_schema missing: %#v", tools[0])
	}
}

func TestOpenAIRequestDefaultsMaxTokens(t *testing.T) {
	converted, err := openAIRequestToAnthropic(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if converted["max_tokens"] != 4096 {
		t.Fatalf("Anthropic requires max_tokens, expected default 4096, got %#v", converted["max_tokens"])
	}
}

func TestAnthropicToOpenAIResponse(t *testing.T) {
	raw := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[
			{"type":"text","text":"先查一下"},
			{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"北京"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":11,"output_tokens":7}
	}`)
	out, usage, err := anthropicToOpenAIResponse(raw, "GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage lost: %#v", usage)
	}
	var parsed struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Object != "chat.completion" {
		t.Fatalf("unexpected object: %q", parsed.Object)
	}
	if parsed.Choices[0].Message.Content != "先查一下" {
		t.Fatalf("text lost: %#v", parsed.Choices[0].Message)
	}
	if parsed.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("stop_reason not mapped: %q", parsed.Choices[0].FinishReason)
	}
	if len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_use not converted: %#v", parsed.Choices[0].Message)
	}
	call := parsed.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "weather" || !strings.Contains(call.Function.Arguments, "北京") {
		t.Fatalf("tool call payload wrong: %#v", call)
	}
	if parsed.Usage.TotalTokens != 18 {
		t.Fatalf("total_tokens should be input+output: %#v", parsed.Usage)
	}
}

func TestOpenAIStreamTranslation(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newOpenAIStream(rec, "GLM-5.3")
	lines := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`,
		`data: {"type":"content_block_start","content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"好"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}
	produced := 0
	for _, line := range lines {
		if stream.feed(line) {
			produced++
		}
	}
	stream.done()

	if produced != 2 {
		t.Fatalf("expected 2 content deltas, got %d", produced)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, `"content":"好"`) {
		t.Fatalf("text deltas missing:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream should end with [DONE]:\n%s", body)
	}
	if stream.usage.InputTokens != 9 || stream.usage.OutputTokens != 4 {
		t.Fatalf("stream usage not captured: %#v", stream.usage)
	}
}

// 端到端：/v1/chat/completions 打到会返回 Anthropic JSON 的假上游。
func TestChatCompletionsEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-openai-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = os.RemoveAll(dir)
	})
	store.accounts = nil
	store.settings.ProxyToken = ""
	acc := &Account{
		ID: "oa-1", Label: "oa-1", Provider: "zai", Mode: "start-plan",
		User: UserInfo{ID: "oa-user"}, Enabled: true, CreatedAt: 1,
		ZcodeJWT: "fake-token", Models: map[string]*UsageTotals{},
		Quota: Quota{Granted: 1000, Remaining: 1000},
	}
	if err := store.AddAccount(acc); err != nil {
		t.Fatal(err)
	}

	var seen map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_9","type":"message","role":"assistant","content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("ZCODE_UPSTREAM", upstream.URL+"/v1/messages")

	server := NewServer(store, mustUpstream(t), "http://test")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	server.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if seen["system"] == nil {
		// system 可以为空，但 messages 必须是 Anthropic 结构
		if _, ok := seen["messages"].([]any); !ok {
			t.Fatalf("upstream did not receive anthropic messages: %#v", seen)
		}
	}
	if _, ok := seen["max_tokens"]; !ok {
		t.Fatalf("max_tokens missing in upstream request: %#v", seen)
	}
	var parsed struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	if parsed.Object != "chat.completion" || parsed.Choices[0].Message.Content != "你好" {
		t.Fatalf("unexpected OpenAI response: %s", rec.Body.String())
	}
}
