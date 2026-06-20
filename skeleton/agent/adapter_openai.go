package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// OpenAIAdapter 覆盖 OpenAI / Codex 及绝大多数走 OpenAI 兼容端点的国产模型
// （火山方舟 / DeepSeek / 千问 / 豆包 / Kimi / 智谱）。
//
// 工具调用流式协议：assistant 消息里的 tool_calls 以 delta 形式分片到达，
// 每片带 index，需要按 index 累积 name + 拼接 arguments(JSON 字符串片段)。
type OpenAIAdapter struct {
	names  *NameMapper
	strict bool // strict 模式做 additionalProperties:false + 全 required
}

func NewOpenAIAdapter(names *NameMapper, strict bool) *OpenAIAdapter {
	if names == nil {
		names = NewNameMapper()
	}
	return &OpenAIAdapter{names: names, strict: strict}
}

func (a *OpenAIAdapter) Family() string      { return "openai" }
func (a *OpenAIAdapter) SupportsTools() bool { return true }

// EncodeTools → [{type:"function", function:{name, description, parameters}}]
func (a *OpenAIAdapter) EncodeTools(tools []Tool) (any, error) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := t.InputSchema
		if a.strict {
			params = SanitizeForOpenAI(t.InputSchema)
		} else {
			params = ensureObject(cloneSchema(t.InputSchema).(map[string]any))
		}
		fn := map[string]any{
			"name":        a.names.ToSafe(t.Name), // 点 → "__"
			"description": t.Description,
			"parameters":  params,
		}
		if a.strict {
			fn["strict"] = true
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, nil
}

// EncodeMessages → OpenAI messages（assistant.tool_calls + role:"tool"）
func (a *OpenAIAdapter) EncodeMessages(msgs []ChatMessage) (any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      m.Content,
			})
		case "assistant":
			am := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.ToolCalls) > 0 {
				tcs := make([]map[string]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					args, _ := json.Marshal(tc.Args)
					tcs = append(tcs, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      a.names.ToSafe(tc.Name),
							"arguments": string(args),
						},
					})
				}
				am["tool_calls"] = tcs
				am["content"] = nil
			}
			out = append(out, am)
		default:
			out = append(out, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	return out, nil
}

// ---- 流式解析 ----

type oaToolCallAcc struct {
	id   string
	name string
	args []byte
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ParseStream 累积 content + 按 index 拼接 tool_calls，最后归一化（denamespace）。
func (a *OpenAIAdapter) ParseStream(ctx context.Context, raw RawStream, onDelta func(StreamDelta)) (StreamResult, error) {
	var res StreamResult
	accs := map[int]*oaToolCallAcc{}
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
		if len(line) == 0 || string(line) == "[DONE]" {
			continue
		}
		var chunk oaStreamChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue // 容忍非数据行（注释/心跳）
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" {
			res.Text += ch.Delta.Content
			if onDelta != nil {
				onDelta(StreamDelta{Text: ch.Delta.Content})
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc, ok := accs[tc.Index]
			if !ok {
				acc = &oaToolCallAcc{}
				accs[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args = append(acc.args, tc.Function.Arguments...)
		}
		if ch.FinishReason != "" {
			res.FinishReason = ch.FinishReason
		}
	}

	for _, idx := range order {
		acc := accs[idx]
		var args map[string]any
		if len(acc.args) > 0 {
			if err := json.Unmarshal(acc.args, &args); err != nil {
				return res, errors.New("openai: 工具参数 JSON 解析失败: " + acc.name)
			}
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:   acc.id,
			Name: a.names.ToRaw(acc.name), // 安全名 → MCP 原名
			Args: args,
		})
	}
	return res, nil
}
