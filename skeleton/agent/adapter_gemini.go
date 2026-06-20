package agent

import (
	"context"
	"encoding/json"
	"io"
)

// GeminiAdapter 适配 Gemini CLI。
//
// 关键差异：
//   - 工具用 tools:[{functionDeclarations:[{name, description, parameters}]}]。
//   - 模型请求工具用 candidate.content.parts[].functionCall。
//   - 结果回填用 parts[].functionResponse（按 name 关联，没有 call id）。
//   - schema 子集最窄，必须 SanitizeForGemini。
//
// 注意：Gemini 没有 tool_call id，所以归一化时用 name 作为 ToolCall.ID 占位，
// 回填 functionResponse 时也按 name 关联。
type GeminiAdapter struct {
	names *NameMapper
}

func NewGeminiAdapter(names *NameMapper) *GeminiAdapter {
	if names == nil {
		names = NewNameMapper()
	}
	return &GeminiAdapter{names: names}
}

func (a *GeminiAdapter) Family() string      { return "gemini" }
func (a *GeminiAdapter) SupportsTools() bool { return true }

// EncodeTools → [{functionDeclarations:[...]}]
func (a *GeminiAdapter) EncodeTools(tools []Tool) (any, error) {
	decls := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, map[string]any{
			"name":        a.names.ToSafe(t.Name),
			"description": t.Description,
			"parameters":  SanitizeForGemini(t.InputSchema),
		})
	}
	return []map[string]any{{"functionDeclarations": decls}}, nil
}

// EncodeMessages → Gemini contents（role: user/model，parts 里含 functionCall/functionResponse）
func (a *GeminiAdapter) EncodeMessages(msgs []ChatMessage) (any, error) {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": map[string]any{
						"name":     a.names.ToSafe(m.ToolCallID), // Gemini 按 name 关联
						"response": map[string]any{"content": m.Content},
					},
				}},
			})
		case "assistant":
			parts := []map[string]any{}
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": a.names.ToSafe(tc.Name),
						"args": tc.Args,
					},
				})
			}
			out = append(out, map[string]any{"role": "model", "parts": parts})
		case "system":
			// Gemini 用单独的 systemInstruction 字段，调用方处理；这里并进 user。
			out = append(out, map[string]any{"role": "user", "parts": []map[string]any{{"text": m.Content}}})
		default:
			out = append(out, map[string]any{"role": "user", "parts": []map[string]any{{"text": m.Content}}})
		}
	}
	return out, nil
}

// ---- 流式解析 ----

type gemChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

func (a *GeminiAdapter) ParseStream(ctx context.Context, raw RawStream, onDelta func(StreamDelta)) (StreamResult, error) {
	var res StreamResult
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
		var chunk gemChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		for _, cand := range chunk.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					res.Text += p.Text
					if onDelta != nil {
						onDelta(StreamDelta{Text: p.Text})
					}
				}
				if p.FunctionCall != nil {
					res.ToolCalls = append(res.ToolCalls, ToolCall{
						ID:   p.FunctionCall.Name, // 无 id，用 name 占位
						Name: a.names.ToRaw(p.FunctionCall.Name),
						Args: p.FunctionCall.Args,
					})
				}
			}
			if cand.FinishReason != "" {
				res.FinishReason = cand.FinishReason
			}
		}
	}
	return res, nil
}
