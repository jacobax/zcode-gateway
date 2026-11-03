package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// OpenAI Chat Completions ↔ Anthropic Messages 的双向翻译。
//
// 上游只会讲 Anthropic，所以这里做两件事：
//   - 入站：chat.completions 请求体 → anthropic messages 请求体
//   - 出站：anthropic 响应 / SSE → chat.completion / chat.completion.chunk
//
// 只翻译两边语义对得上的部分：文本、图片（data URI）、工具调用、stop、
// 采样参数与 token 上限。thinking 之类 Anthropic 专有的字段直接透传。

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "api_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": status},
	})
}

func writeOpenAIUpstreamError(w http.ResponseWriter, status int, body []byte) {
	message := strings.TrimSpace(string(body))
	var parsed struct {
		Msg   string `json:"msg"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		message = firstNonEmpty(parsed.Msg, parsed.Error.Message, message)
	}
	writeOpenAIError(w, status, message)
}

// openAIRequestToAnthropic 把 chat.completions 请求翻成 Anthropic 请求体。
func openAIRequestToAnthropic(body map[string]any) (map[string]any, error) {
	rawMessages, _ := body["messages"].([]any)
	if len(rawMessages) == 0 {
		return nil, errors.New("request body requires messages[]")
	}
	out := map[string]any{}
	var system []string
	messages := make([]any, 0, len(rawMessages))

	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			if text := openAIText(msg["content"]); text != "" {
				system = append(system, text)
			}
		case "assistant":
			blocks := openAIContentBlocks(msg["content"])
			for _, call := range openAIToolCalls(msg["tool_calls"]) {
				blocks = append(blocks, call)
			}
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
		case "tool", "function":
			// OpenAI 的 tool 结果单独成条，Anthropic 要包成 user + tool_result。
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": firstString(msg["tool_call_id"], msg["name"]),
				"content":     openAIText(msg["content"]),
			}
			// 连续多个 tool 结果合并进同一条 user 消息，避免出现空轮次。
			if n := len(messages); n > 0 {
				if last, ok := messages[n-1].(map[string]any); ok && last["role"] == "user" {
					if blocks, ok := last["content"].([]any); ok && len(blocks) > 0 {
						if first, ok := blocks[0].(map[string]any); ok && first["type"] == "tool_result" {
							last["content"] = append(blocks, block)
							continue
						}
					}
				}
			}
			messages = append(messages, map[string]any{"role": "user", "content": []any{block}})
		default: // user 以及未知角色都按 user 处理
			blocks := openAIContentBlocks(msg["content"])
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, map[string]any{"role": "user", "content": blocks})
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("request body requires at least one user message")
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	out["messages"] = messages

	if tools, ok := openAITools(body["tools"]); ok {
		out["tools"] = tools
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		if name := nestedString(choice, "function", "name"); name != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": name}
		} else if choice["type"] == "any" || choice["type"] == "required" {
			out["tool_choice"] = map[string]any{"type": "any"}
		}
	}

	maxTokens := intField(body, "max_completion_tokens")
	if maxTokens == 0 {
		maxTokens = intField(body, "max_tokens")
	}
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic 必填，给个保守默认值
	}
	out["max_tokens"] = maxTokens
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}
	if stop, ok := body["stop"]; ok {
		switch value := stop.(type) {
		case string:
			if value != "" {
				out["stop_sequences"] = []any{value}
			}
		case []any:
			if len(value) > 0 {
				out["stop_sequences"] = value
			}
		}
	}
	out["stream"], _ = body["stream"].(bool)
	return out, nil
}

// openAIText 取文本内容，兼容 string 与 [{type:text,text:...}] 两种写法。
func openAIText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// openAIContentBlocks 把 OpenAI 的 content 转成 Anthropic 的 content blocks。
func openAIContentBlocks(content any) []any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": value}}
	case []any:
		blocks := make([]any, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				if text, ok := part["text"].(string); ok && text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				url := nestedString(part, "image_url", "url")
				if block, ok := imageBlock(url); ok {
					blocks = append(blocks, block)
				}
			}
		}
		return blocks
	}
	return nil
}

// imageBlock 只接受 data URI：上游拿不到公网图片，也不该由网关去抓。
func imageBlock(url string) (map[string]any, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, false
	}
	meta, payload, ok := strings.Cut(strings.TrimPrefix(url, prefix), ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return nil, false
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		return nil, false
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return nil, false
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": mediaType, "data": payload},
	}, true
}

func openAIToolCalls(raw any) []any {
	calls, _ := raw.([]any)
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		input := map[string]any{}
		if args, ok := fn["arguments"].(string); ok && strings.TrimSpace(args) != "" {
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				input = map[string]any{}
			}
		}
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    firstString(call["id"], "toolu_"+randomHex(12)),
			"name":  name,
			"input": input,
		})
	}
	return out
}

func openAITools(raw any) ([]any, bool) {
	list, _ := raw.([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			entry["description"] = desc
		}
		if schema, ok := fn["parameters"]; ok {
			entry["input_schema"] = schema
		} else {
			entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// anthropicToOpenAIResponse 把非流式 Anthropic 响应翻成 chat.completion。
func anthropicToOpenAIResponse(data []byte, model string) ([]byte, Usage, error) {
	var payload struct {
		ID         string `json:"id"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, Usage{}, err
	}
	var text strings.Builder
	toolCalls := make([]any, 0, 2)
	for _, block := range payload.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := strings.TrimSpace(string(block.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": block.ID, "type": "function",
				"function": map[string]any{"name": block.Name, "arguments": args},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	usage := Usage{
		InputTokens:  payload.Usage.InputTokens,
		OutputTokens: payload.Usage.OutputTokens,
	}
	out := map[string]any{
		"id":      firstNonEmpty(payload.ID, "chatcmpl-"+randomHex(12)),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": openAIFinishReason(payload.StopReason, len(toolCalls) > 0),
		}},
		"usage": map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		},
	}
	encoded, err := json.Marshal(out)
	return encoded, usage, err
}

func openAIFinishReason(stopReason string, hasTools bool) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence", "":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}

// openAIStream 把 Anthropic 的 SSE 逐事件翻成 chat.completion.chunk。
type openAIStream struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	model    string
	id       string
	created  int64
	started  bool
	toolIdx  int
	usage    Usage
	finished bool
}

func newOpenAIStream(w http.ResponseWriter, model string) *openAIStream {
	flusher, _ := w.(http.Flusher)
	return &openAIStream{
		w:       w,
		flusher: flusher,
		model:   model,
		id:      "chatcmpl-" + randomHex(12),
		created: time.Now().Unix(),
	}
}

func (s *openAIStream) write(payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *openAIStream) chunk(delta map[string]any, finish any) {
	if !s.started {
		s.started = true
		delta["role"] = "assistant"
	}
	s.write(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

// feed 处理一行 SSE，返回是否产生了内容 token（用于首 token 计时）。
func (s *openAIStream) feed(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return false
	}
	var event struct {
		Type  string `json:"type"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		// message_start 的 usage 嵌在 message 里，不在事件顶层。
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false
	}
	produced := false
	switch event.Type {
	case "message_start":
		if event.Message.Usage.InputTokens > 0 {
			s.usage.InputTokens = event.Message.Usage.InputTokens
		}
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": s.toolIdx, "id": event.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": event.ContentBlock.Name, "arguments": ""},
			}}}, nil)
			s.toolIdx++
			produced = true
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				s.chunk(map[string]any{"content": event.Delta.Text}, nil)
				produced = true
			}
		case "input_json_delta":
			// 工具参数是分片下发的，直接当 arguments 增量透传。
			if fragment, ok := jsonFragment(payload); ok && fragment != "" {
				s.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": s.toolIdx - 1, "function": map[string]any{"arguments": fragment},
				}}}, nil)
				produced = true
			}
		}
	case "message_delta":
		if event.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = event.Usage.OutputTokens
		}
		if event.Delta.StopReason != "" {
			s.chunk(map[string]any{}, openAIFinishReason(event.Delta.StopReason, s.toolIdx > 0))
			s.finished = true
		}
	}
	return produced
}

// jsonFragment 取 input_json_delta 里的 partial_json 原文。
func jsonFragment(payload string) (string, bool) {
	var event struct {
		Delta struct {
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(payload), &event) != nil {
		return "", false
	}
	return event.Delta.PartialJSON, true
}

func (s *openAIStream) done() {
	if !s.finished {
		s.chunk(map[string]any{}, "stop")
	}
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeUpstreamOpenAI 是 relay 的渲染回调：把上游响应改写成 OpenAI 形状。
func (s *Server) writeUpstreamOpenAI(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, stream bool, model string, start time.Time) (Usage, streamTiming, int, string) {
	defer resp.Body.Close()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !stream {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		converted, usage, err := anthropicToOpenAIResponse(data, model)
		if err != nil {
			writeOpenAIError(w, 502, "上游响应无法解析: "+err.Error())
			return usage, streamTiming{}, len(data), truncate(string(data), 8192)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(converted)
		return usage, streamTiming{}, len(converted), truncate(string(converted), 8192)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	translator := newOpenAIStream(w, model)
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	var timing streamTiming
	var firstTokenAt, lastTokenAt time.Time
	total := 0
	var preview strings.Builder
	for {
		if ctx.Err() != nil {
			break
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += len(line)
			if preview.Len() < 8192 {
				preview.WriteString(line)
			}
			if translator.feed(line) {
				now := time.Now()
				if firstTokenAt.IsZero() {
					firstTokenAt = now
				}
				lastTokenAt = now
			}
		}
		if err != nil {
			break
		}
	}
	translator.done()
	if !firstTokenAt.IsZero() {
		timing.firstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		timing.lastTokenMs = lastTokenAt.Sub(start).Milliseconds()
	}
	return translator.usage, timing, total, truncate(preview.String(), 8192)
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func nestedString(root map[string]any, path ...string) string {
	current := root
	for i, key := range path {
		value, ok := current[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			text, _ := value.(string)
			return text
		}
		next, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

func intField(root map[string]any, key string) int {
	switch value := root[key].(type) {
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	case float64:
		return int(value)
	case int:
		return value
	}
	return 0
}
