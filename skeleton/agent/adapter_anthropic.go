package agent

import (
	"context"
	"encoding/json"
	"io"
)

// AnthropicAdapter 适配 Claude。
//
// 与 OpenAI 的关键差异：
//   - tools: [{name, description, input_schema}]（不是 function 包一层）。
//   - 模型请求工具用 assistant 消息里的 tool_use content block。
//   - 工具结果用 user 消息里的 tool_result content block（不是独立 role:"tool"）。
//   - 流式是 content_block_start/delta/stop 事件；工具参数在 input_json_delta 里分片。
type AnthropicAdapter struct {
	names *NameMapper
}

func NewAnthropicAdapter(names *NameMapper) *AnthropicAdapter {
	if names == nil {
		names = NewNameMapper()
	}
	return &AnthropicAdapter{names: names}
}

func (a *AnthropicAdapter) Family() string      { return "anthropic" }
func (a *AnthropicAdapter) SupportsTools() bool { return true }

// EncodeTools → [{name, description, input_schema}]
// Claude 工具名同样建议走安全名（保持跨 provider 一致 + 可还原）。
func (a *AnthropicAdapter) EncodeTools(tools []Tool) (any, error) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":         a.names.ToSafe(t.Name),
			"description":  t.Description,
			"input_schema": SanitizeForAnthropic(t.InputSchema),
		})
	}
	return out, nil
}

// EncodeMessages → Anthropic messages：
//   - assistant 的 ToolCalls → tool_use blocks
//   - role=="tool" → user 消息里的 tool_result block
func (a *AnthropicAdapter) EncodeMessages(msgs []ChatMessage) (any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
		case "assistant":
			blocks := []map[string]any{}
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  a.names.ToSafe(tc.Name),
					"input": tc.Args,
				})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			out = append(out, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	return out, nil
}

// ---- 流式解析（content_block_* 事件）----

type anthEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"` // "text" | "tool_use"
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"` // "text_delta" | "input_json_delta"
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}

type anthBlockAcc struct {
	isTool bool
	id     string
	name   string
	args   []byte
}

func (a *AnthropicAdapter) ParseStream(ctx context.Context, raw RawStream, onDelta func(StreamDelta)) (StreamResult, error) {
	var res StreamResult
	blocks := map[int]*anthBlockAcc{}
	var order []int

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		line, err := raw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		if len(line) == 0 {
			continue
		}
		var ev anthEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			acc := &anthBlockAcc{}
			if ev.ContentBlock.Type == "tool_use" {
				acc.isTool = true
				acc.id = ev.ContentBlock.ID
				acc.name = ev.ContentBlock.Name
			}
			blocks[ev.Index] = acc
			order = append(order, ev.Index)
		case "content_block_delta":
			acc := blocks[ev.Index]
			if acc == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				res.Text += ev.Delta.Text
				if onDelta != nil {
					onDelta(StreamDelta{Text: ev.Delta.Text})
				}
			case "input_json_delta":
				acc.args = append(acc.args, ev.Delta.PartialJSON...)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				res.FinishReason = ev.Delta.StopReason
			}
		case "message_stop":
			// 流结束
		}
	}

	for _, idx := range order {
		acc := blocks[idx]
		if acc == nil || !acc.isTool {
			continue
		}
		var args map[string]any
		if len(acc.args) > 0 {
			_ = json.Unmarshal(acc.args, &args)
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:   acc.id,
			Name: a.names.ToRaw(acc.name),
			Args: args,
		})
	}
	if res.FinishReason == "" && len(res.ToolCalls) > 0 {
		res.FinishReason = "tool_use"
	}
	return res, nil
}
