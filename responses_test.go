package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Responses 请求要能翻成 Anthropic 形状：instructions → system，
// input 数组里的 function_call / function_call_output 要合并进相邻消息。
func TestResponsesRequestToAnthropic(t *testing.T) {
	body := map[string]any{
		"model":             "gpt-5",
		"instructions":      "be brief",
		"max_output_tokens": json.Number("512"),
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "list files"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "ls", "arguments": `{"path":"/"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "a.txt"},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "ls", "description": "list", "parameters": map[string]any{"type": "object"}},
		},
	}
	out, err := responsesRequestToAnthropic(body)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if out["system"] != "be brief" {
		t.Fatalf("system = %v", out["system"])
	}
	if out["max_tokens"] != 512 {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	messages, _ := out["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant tool_use, user tool_result), got %d: %#v", len(messages), messages)
	}
	if messages[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("function_call should land on an assistant message: %#v", messages[1])
	}
	blocks := messages[1].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("want tool_use block, got %#v", blocks[0])
	}
	toolInput := blocks[0].(map[string]any)["input"].(map[string]any)
	if toolInput["path"] != "/" {
		t.Fatalf("tool input not parsed: %#v", toolInput)
	}
	if messages[2].(map[string]any)["role"] != "user" {
		t.Fatalf("function_call_output should land on a user message: %#v", messages[2])
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	if _, ok := tools[0].(map[string]any)["input_schema"]; !ok {
		t.Fatalf("tool should carry input_schema: %#v", tools[0])
	}
}

// 字符串 input 也要能用。
func TestResponsesStringInput(t *testing.T) {
	out, err := responsesRequestToAnthropic(map[string]any{"input": "hello there"})
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	messages, _ := out["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(messages))
	}
	if out["max_tokens"] != 4096 {
		t.Fatalf("default max_tokens = %v, want 4096", out["max_tokens"])
	}
}

// Anthropic 响应要翻成 Responses 形状：text → output_text，tool_use → function_call。
func TestAnthropicToResponsesResponse(t *testing.T) {
	data := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","stop_reason":"tool_use",
		"content":[
			{"type":"text","text":"let me look"},
			{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"/"}}
		],
		"usage":{"input_tokens":11,"output_tokens":7}
	}`)
	out, usage, err := anthropicToResponsesResponse(data, "glm")
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %#v", usage)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			CallID  string `json:"call_id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("bad json: %v (%s)", err, out)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("envelope wrong: %#v", resp)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("want 2 output items, got %d", len(resp.Output))
	}
	if resp.Output[0].Type != "message" || resp.Output[0].Content[0].Text != "let me look" {
		t.Fatalf("text item wrong: %#v", resp.Output[0])
	}
	if resp.Output[1].Type != "function_call" || resp.Output[1].Name != "ls" || resp.Output[1].CallID != "toolu_1" {
		t.Fatalf("tool item wrong: %#v", resp.Output[1])
	}
}

// 流式：Anthropic SSE 要翻成 response.* 事件序列。
func TestResponsesStreamEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newResponsesStream(rec, "glm")
	lines := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
	}
	for _, line := range lines {
		stream.feed(line + "\n")
	}
	stream.done()

	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"text":"Hello"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in stream output:\n%s", want, out)
		}
	}
	if stream.usage.InputTokens != 5 || stream.usage.OutputTokens != 2 {
		t.Fatalf("usage = %#v", stream.usage)
	}
}

// 工具调用的流式分片要累积到 function_call_arguments.done。
func TestResponsesStreamToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newResponsesStream(rec, "glm")
	for _, line := range []string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"ls"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"th\":\"/\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	} {
		stream.feed(line + "\n")
	}
	stream.done()

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.function_call_arguments.done") {
		t.Fatalf("missing arguments.done:\n%s", out)
	}
	if !strings.Contains(out, `{\"path\":\"/\"}`) {
		t.Fatalf("arguments not accumulated:\n%s", out)
	}
}

// 先思考后输出的模型：thinking block 要变成 reasoning item，text 照常输出。
// 丢掉 thinking 会让「只思考不输出」的回合变成空 output，客户端以为模型没回话。
func TestResponsesStreamThinking(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newResponsesStream(rec, "glm")
	for _, line := range []string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"pong"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	} {
		stream.feed(line + "\n")
	}
	stream.done()

	out := rec.Body.String()
	for _, want := range []string{`"type":"reasoning"`, `"text":"let me think"`, `"text":"pong"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in stream output:\n%s", want, out)
		}
	}
	if len(stream.output) != 2 {
		t.Fatalf("want 2 output items (reasoning + message), got %d", len(stream.output))
	}
}

// 端到端：/v1/responses 打到会返回 Anthropic JSON 的假上游（非流式），
// 验证请求翻译、响应改写和 usage 透传。
func TestResponsesEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-responses-")
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
		ID: "rs-1", Label: "rs-1", Provider: "zai", Mode: "start-plan",
		User: UserInfo{ID: "rs-user"}, Enabled: true, CreatedAt: 1,
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
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"GLM-5.3","input":"hi","max_output_tokens":64}`))
	req.Header.Set("Content-Type", "application/json")
	server.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := seen["messages"].([]any); !ok {
		t.Fatalf("upstream did not receive anthropic messages: %#v", seen)
	}
	var parsed struct {
		Object string `json:"object"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	if parsed.Object != "response" {
		t.Fatalf("object: %q body=%s", parsed.Object, rec.Body.String())
	}
	if len(parsed.Output) != 1 || parsed.Output[0].Type != "message" || len(parsed.Output[0].Content) != 1 || parsed.Output[0].Content[0].Text != "你好" {
		t.Fatalf("output: %#v body=%s", parsed.Output, rec.Body.String())
	}
	if parsed.Usage.InputTokens != 3 || parsed.Usage.OutputTokens != 2 {
		t.Fatalf("usage: %#v", parsed.Usage)
	}
}

// 非流式也要把 thinking 带出来：GLM 先发 thinking 再发 text，只思考不出字时
// output 不能是空数组（流式那边发的是 reasoning item，这里保持同一形状）。
func TestAnthropicToResponsesThinking(t *testing.T) {
	data := []byte(`{"id":"msg_1","content":[{"type":"thinking","thinking":"先想一下"},{"type":"text","text":"好了"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	out, usage, err := anthropicToResponsesResponse(data, "GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 1 || usage.OutputTokens != 2 {
		t.Fatalf("usage: %#v", usage)
	}
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Output) != 2 {
		t.Fatalf("thinking + text should yield 2 items, got %d: %s", len(parsed.Output), out)
	}
	if parsed.Output[0].Type != "reasoning" || len(parsed.Output[0].Summary) != 1 || parsed.Output[0].Summary[0].Text != "先想一下" {
		t.Fatalf("reasoning item: %#v", parsed.Output[0])
	}
	if parsed.Output[1].Type != "message" || len(parsed.Output[1].Content) != 1 || parsed.Output[1].Content[0].Text != "好了" {
		t.Fatalf("message item: %#v", parsed.Output[1])
	}

	onlyThinking, _, err := anthropicToResponsesResponse([]byte(`{"content":[{"type":"thinking","thinking":"只有思考"}]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	var parsedOnly struct {
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	_ = json.Unmarshal(onlyThinking, &parsedOnly)
	if len(parsedOnly.Output) != 1 || parsedOnly.Output[0].Type != "reasoning" {
		t.Fatalf("thinking-only response should still carry a reasoning item: %s", onlyThinking)
	}
}
