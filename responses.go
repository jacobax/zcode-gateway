package main

// OpenAI Responses API（/v1/responses）兼容层。
//
// 为什么单独做一个端点而不是让 chat.completions 兼任：Codex CLI 这一类客户端
// 只认 Responses 形状——请求用 input/instructions/max_output_tokens，响应用
// output[] 数组和 output_text，流式事件是 response.* 而不是 chat.completion.chunk。
// 用 chat.completions 硬顶会直接 400。
//
// 转换方向只有一个：Responses ⇄ Anthropic。上游只有 Anthropic 接口，
// 这里不引入第三条中间表示。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// ---------- 请求：Responses → Anthropic ----------

func responsesRequestToAnthropic(body map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var system []string
	if instructions, ok := body["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		system = append(system, instructions)
	}
	messages, err := responsesInputToMessages(body["input"])
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, errors.New("request body requires input")
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	out["messages"] = messages

	if tools, ok := responsesTools(body["tools"]); ok {
		out["tools"] = tools
	}
	if choice, ok := body["tool_choice"].(string); ok {
		switch choice {
		case "auto":
			// Anthropic 默认就是 auto，不用显式传。
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			out["tool_choice"] = map[string]any{"type": "none"}
		}
	}

	maxTokens := intField(body, "max_output_tokens")
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
	out["stream"], _ = body["stream"].(bool)
	return out, nil
}

// responsesInputToMessages 把 input（字符串或 item 数组）翻成 Anthropic messages。
//
// item 数组里混着三种东西：普通对话消息、function_call（模型发起的工具调用）、
// function_call_output（工具结果）。后两者在 Anthropic 里是 content block，
// 不是独立消息，所以要合并进相邻消息，避免出现空轮次。
func responsesInputToMessages(input any) ([]any, error) {
	switch value := input.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, nil
		}
		return []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": value}},
		}}, nil

	case []any:
		messages := make([]any, 0, len(value))
		for _, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := item["type"].(string)
			switch kind {
			case "function_call":
				messages = appendRoleBlocks(messages, "assistant", []any{map[string]any{
					"type":  "tool_use",
					"id":    firstString(item["call_id"], item["id"]),
					"name":  firstString(item["name"]),
					"input": parseJSONObject(firstString(item["arguments"])),
				}})
			case "function_call_output":
				messages = appendRoleBlocks(messages, "user", []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": firstString(item["call_id"]),
					"content":     responsesToolOutput(item["output"]),
				}})
			default:
				role, _ := item["role"].(string)
				blocks := responsesContentBlocks(item["content"])
				if len(blocks) == 0 {
					continue
				}
				if role == "assistant" {
					messages = appendRoleBlocks(messages, "assistant", blocks)
				} else {
					// user / system / developer 一律按 user 处理：
					// Anthropic 的 system 是顶层字段，混在 messages 里会报错。
					messages = appendRoleBlocks(messages, "user", blocks)
				}
			}
		}
		return messages, nil
	}
	return nil, errors.New("input must be a string or an array")
}

// appendRoleBlocks 把 blocks 合并进上一条同角色消息，没有就新开一条。
func appendRoleBlocks(messages []any, role string, blocks []any) []any {
	if len(blocks) == 0 {
		return messages
	}
	if n := len(messages); n > 0 {
		if last, ok := messages[n-1].(map[string]any); ok && last["role"] == role {
			if existing, ok := last["content"].([]any); ok {
				last["content"] = append(existing, blocks...)
				return messages
			}
		}
	}
	return append(messages, map[string]any{"role": role, "content": blocks})
}

// responsesContentBlocks 把 Responses 的 content 翻成 Anthropic content block。
func responsesContentBlocks(content any) []any {
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
			switch kind, _ := part["type"].(string); kind {
			case "input_text", "output_text", "text", "summary_text":
				if text := firstString(part["text"]); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "input_image":
				if block, ok := imageBlock(firstString(part["image_url"], part["url"])); ok {
					blocks = append(blocks, block)
				}
			}
		}
		return blocks
	}
	return nil
}

// responsesToolOutput 取工具结果文本。output 可能是字符串，也可能是 content 数组。
func responsesToolOutput(output any) string {
	if text := firstString(output); text != "" {
		return text
	}
	var b strings.Builder
	if list, ok := output.([]any); ok {
		for _, raw := range list {
			if part, ok := raw.(map[string]any); ok {
				b.WriteString(firstString(part["text"], part["content"]))
			}
		}
	}
	return b.String()
}

// responsesTools 把 Responses 的扁平工具定义翻成 Anthropic 的 input_schema 形状。
//
//	Responses: {"type":"function","name":"f","description":"..","parameters":{..}}
//	Anthropic: {"name":"f","description":"..","input_schema":{..}}
func responsesTools(raw any) ([]any, bool) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	tools := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := firstString(tool["name"])
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if desc := firstString(tool["description"]); desc != "" {
			entry["description"] = desc
		}
		if params, ok := tool["parameters"].(map[string]any); ok {
			entry["input_schema"] = params
		} else {
			entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, entry)
	}
	if len(tools) == 0 {
		return nil, false
	}
	return tools, true
}

func parseJSONObject(raw string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return map[string]any{}
	}
	return out
}

// ---------- 响应：Anthropic → Responses ----------

func anthropicToResponsesResponse(data []byte, model string) ([]byte, Usage, error) {
	var parsed struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, Usage{}, err
	}
	usage := Usage{InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens}
	output := make([]any, 0, len(parsed.Content))
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			output = append(output, map[string]any{
				"type": "message", "id": "msg_" + randomHex(12), "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": block.Text, "annotations": []any{}}},
			})
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_" + randomHex(12), "call_id": block.ID,
				"name": block.Name, "arguments": args, "status": "completed",
			})
		}
	}
	resp := map[string]any{
		"id": "resp_" + randomHex(16), "object": "response", "created_at": time.Now().Unix(),
		"model": model, "status": "completed", "output": output,
		"usage": map[string]any{
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
			"total_tokens": usage.InputTokens + usage.OutputTokens,
		},
	}
	out, err := json.Marshal(resp)
	return out, usage, err
}

// ---------- 流式：Anthropic SSE → Responses SSE ----------

type responsesStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	id      string
	created int64
	started bool

	nextIndex int
	output    []any

	textOpen  bool
	textIndex int
	textID    string
	textBuf   strings.Builder

	// GLM 这类模型先发 thinking block 再发 text block。思考内容在 Responses
	// 里对应 reasoning item——丢掉它会让「只思考不输出」的回合变成空 output，
	// 客户端以为模型没回话。
	reasoningOpen  bool
	reasoningIndex int
	reasoningID    string
	reasoningBuf   strings.Builder

	toolOpen   bool
	toolIndex  int
	toolID     string
	toolCallID string
	toolName   string
	toolArgs   strings.Builder

	usage    Usage
	finished bool
}

func newResponsesStream(w http.ResponseWriter, model string) *responsesStream {
	flusher, _ := w.(http.Flusher)
	return &responsesStream{
		w:       w,
		flusher: flusher,
		model:   model,
		id:      "resp_" + randomHex(16),
		created: time.Now().Unix(),
	}
}

func (s *responsesStream) event(kind string, payload map[string]any) {
	payload["type"] = kind
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, data)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *responsesStream) responseObject(status string) map[string]any {
	return map[string]any{
		"id": s.id, "object": "response", "created_at": s.created,
		"model": s.model, "status": status,
		"output": append([]any(nil), s.output...),
		"usage": map[string]any{
			"input_tokens":  s.usage.InputTokens,
			"output_tokens": s.usage.OutputTokens,
			"total_tokens":  s.usage.InputTokens + s.usage.OutputTokens,
		},
	}
}

func (s *responsesStream) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	s.event("response.created", map[string]any{"response": s.responseObject("in_progress")})
}

func (s *responsesStream) allocIndex() int {
	index := s.nextIndex
	s.nextIndex++
	return index
}

func (s *responsesStream) openReasoning() {
	if s.reasoningOpen {
		return
	}
	s.closeText()
	s.closeTool()
	s.reasoningOpen = true
	s.reasoningID = "rs_" + randomHex(12)
	s.reasoningIndex = s.allocIndex()
	s.reasoningBuf.Reset()
	s.event("response.output_item.added", map[string]any{
		"output_index": s.reasoningIndex,
		"item":         map[string]any{"type": "reasoning", "id": s.reasoningID, "summary": []any{}},
	})
}

func (s *responsesStream) closeReasoning() {
	if !s.reasoningOpen {
		return
	}
	s.reasoningOpen = false
	text := s.reasoningBuf.String()
	item := map[string]any{
		"type": "reasoning", "id": s.reasoningID,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	s.event("response.output_item.done", map[string]any{"output_index": s.reasoningIndex, "item": item})
	s.output = append(s.output, item)
	s.reasoningBuf.Reset()
}

func (s *responsesStream) openText() {
	if s.textOpen {
		return
	}
	s.closeTool()
	s.closeReasoning()
	s.textOpen = true
	s.textID = "msg_" + randomHex(12)
	s.textIndex = s.allocIndex()
	s.textBuf.Reset()
	s.event("response.output_item.added", map[string]any{
		"output_index": s.textIndex,
		"item": map[string]any{
			"type": "message", "id": s.textID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	s.event("response.content_part.added", map[string]any{
		"item_id": s.textID, "output_index": s.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (s *responsesStream) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	text := s.textBuf.String()
	s.event("response.output_text.done", map[string]any{
		"item_id": s.textID, "output_index": s.textIndex, "content_index": 0, "text": text,
	})
	s.event("response.content_part.done", map[string]any{
		"item_id": s.textID, "output_index": s.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := map[string]any{
		"type": "message", "id": s.textID, "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	s.event("response.output_item.done", map[string]any{"output_index": s.textIndex, "item": item})
	s.output = append(s.output, item)
	s.textBuf.Reset()
}

func (s *responsesStream) openTool(id, name string) {
	if s.toolOpen {
		s.closeTool()
	}
	s.closeText()
	s.closeReasoning()
	s.toolOpen = true
	s.toolID = "fc_" + randomHex(12)
	s.toolCallID = id
	s.toolName = name
	s.toolIndex = s.allocIndex()
	s.toolArgs.Reset()
	s.event("response.output_item.added", map[string]any{
		"output_index": s.toolIndex,
		"item": map[string]any{
			"type": "function_call", "id": s.toolID, "call_id": s.toolCallID,
			"name": s.toolName, "arguments": "", "status": "in_progress",
		},
	})
}

func (s *responsesStream) closeTool() {
	if !s.toolOpen {
		return
	}
	s.toolOpen = false
	args := s.toolArgs.String()
	if args == "" {
		args = "{}"
	}
	s.event("response.function_call_arguments.done", map[string]any{
		"item_id": s.toolID, "output_index": s.toolIndex, "arguments": args,
	})
	item := map[string]any{
		"type": "function_call", "id": s.toolID, "call_id": s.toolCallID,
		"name": s.toolName, "arguments": args, "status": "completed",
	}
	s.event("response.output_item.done", map[string]any{"output_index": s.toolIndex, "item": item})
	s.output = append(s.output, item)
}

// feed 处理一行 Anthropic SSE，返回是否产生了内容 token（用于首 token 计时）。
func (s *responsesStream) feed(line string) bool {
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
			Thinking   string `json:"thinking"`
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
	if json.Unmarshal([]byte(payload), &event) != nil {
		return false
	}
	s.ensureStarted()
	produced := false
	switch event.Type {
	case "message_start":
		if event.Message.Usage.InputTokens > 0 {
			s.usage.InputTokens = event.Message.Usage.InputTokens
		}
	case "content_block_start":
		switch event.ContentBlock.Type {
		case "tool_use":
			s.openTool(event.ContentBlock.ID, event.ContentBlock.Name)
		case "thinking", "redacted_thinking":
			s.openReasoning()
		case "text":
			s.openText()
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "thinking_delta":
			if event.Delta.Thinking != "" {
				s.openReasoning()
				s.reasoningBuf.WriteString(event.Delta.Thinking)
				produced = true
			}
		case "text_delta":
			if event.Delta.Text != "" {
				s.openText()
				s.textBuf.WriteString(event.Delta.Text)
				s.event("response.output_text.delta", map[string]any{
					"item_id": s.textID, "output_index": s.textIndex, "content_index": 0,
					"delta": event.Delta.Text,
				})
				produced = true
			}
		case "input_json_delta":
			if fragment, ok := jsonFragment(payload); ok && fragment != "" {
				s.toolArgs.WriteString(fragment)
				s.event("response.function_call_arguments.delta", map[string]any{
					"item_id": s.toolID, "output_index": s.toolIndex, "delta": fragment,
				})
				produced = true
			}
		}
	case "content_block_stop":
		if s.toolOpen {
			s.closeTool()
		}
		if s.reasoningOpen {
			s.closeReasoning()
		}
	case "message_delta":
		if event.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = event.Usage.OutputTokens
		}
		if event.Delta.StopReason != "" {
			s.finished = true
		}
	}
	return produced
}

func (s *responsesStream) done() {
	s.ensureStarted()
	s.closeText()
	s.closeTool()
	s.closeReasoning()
	status := "completed"
	if !s.finished {
		// 上游没给 stop_reason 就断了：如实报 incomplete，别假装成功。
		status = "incomplete"
	}
	s.event("response.completed", map[string]any{"response": s.responseObject(status)})
}

// ---------- 入口 ----------

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeOpenAIError(w, 401, "invalid local proxy token")
		return
	}
	var body map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeOpenAIError(w, 400, "request body is not valid JSON")
		return
	}
	anthropicBody, err := responsesRequestToAnthropic(body)
	if err != nil {
		writeOpenAIError(w, 400, err.Error())
		return
	}
	stream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	if model != "" && !modelAllowed(model) {
		writeOpenAIError(w, 400, "model not allowed: "+model)
		return
	}
	if model == "" {
		model = "GLM-5.3-Flash"
	}
	clientReqBody := ""
	if s.store.Settings().StoreBodies {
		if data, err := json.Marshal(body); err == nil {
			clientReqBody = truncate(string(data), 8192)
		}
	}
	s.relay(w, r, relayOptions{
		body:          anthropicBody,
		model:         model,
		stream:        stream,
		sessionBody:   body,
		clientReqBody: clientReqBody,
		render: func(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, start time.Time) (Usage, streamTiming, int, string) {
			return s.writeUpstreamResponses(ctx, w, resp, stream, model, start)
		},
		writeError:    writeOpenAIError,
		writeUpstream: writeOpenAIUpstreamError,
	})
}

// writeUpstreamResponses 是 relay 的渲染回调：把上游响应改写成 Responses 形状。
func (s *Server) writeUpstreamResponses(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, stream bool, model string, start time.Time) (Usage, streamTiming, int, string) {
	defer resp.Body.Close()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !stream {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		converted, usage, err := anthropicToResponsesResponse(data, model)
		if err != nil {
			// 转换失败就把原始 body 透出去，至少客户端能看到上游说了什么。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(data)
			return parseUsageFromJSON(data), streamTiming{}, len(data), truncate(string(data), 8192)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(converted)
		return usage, streamTiming{}, len(converted), truncate(string(converted), 8192)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	writer := newResponsesStream(w, model)
	total := 0
	var preview strings.Builder
	var timing streamTiming
	var firstTokenAt, lastTokenAt time.Time
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
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
			if writer.feed(line) {
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
	writer.done()
	if flusher != nil {
		flusher.Flush()
	}
	if !firstTokenAt.IsZero() {
		timing.firstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		timing.lastTokenMs = lastTokenAt.Sub(start).Milliseconds()
	}
	return writer.usage, timing, total, truncate(preview.String(), 8192)
}
