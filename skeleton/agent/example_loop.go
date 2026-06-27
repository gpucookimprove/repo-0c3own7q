package agent

import (
	"context"
	"fmt"
)

// 本文件演示如何把 adapter 接进设计文档 5.5 的多轮 ReAct 循环。
// 这里只展示「provider 无关编排」与「provider 适配」的接缝，
// 真实的 HTTP 发送 / SSE 读取由调用方实现 RawStream 与 sendFn。

// LLMTransport 由调用方实现：把已编码的 messages + tools 发给当前 provider，
// 返回一个 RawStream 供 adapter.ParseStream 消费。
type LLMTransport func(ctx context.Context, encodedMessages any, encodedTools any) (RawStream, error)

// MCPDispatcher 由调用方实现（即设计文档的 MCPRouter.Dispatch）：
// 按 "<server>.<tool>" 路由到对应 MCP server 执行，返回文本结果。
type MCPDispatcher func(ctx context.Context, name string, args map[string]any) (string, error)

// RunReActLoop 跑 provider 无关的多轮工具调用循环。
//
//	provider : 当前 CC Switch 选中的 provider（决定用哪个 adapter）
//	tools    : 已启用的、归一化的 MCP 工具目录（provider 无关）
//	skills   : 已启用的 skill（L1：正文拼进 system prompt 注入对话；可为 nil）
//	history  : 初始消息（system + user）
//	maxRounds: 防死循环上限
func RunReActLoop(
	ctx context.Context,
	provider string,
	tools []Tool,
	skills []Skill,
	history []ChatMessage,
	maxRounds int,
	transport LLMTransport,
	dispatch MCPDispatcher,
	onDelta func(StreamDelta),
) (StreamResult, error) {
	// L1：把启用的 skill 正文注入为首条 system 消息（在工具/fallback 之前）。
	if sys := BuildSystemPromptWithSkills("", skills); sys != "" {
		history = append([]ChatMessage{{Role: "system", Content: sys}}, history...)
	}

	names := NewNameMapper()
	adapter := AdapterFor(provider, names)

	var encodedTools any
	if adapter.SupportsTools() {
		t, err := adapter.EncodeTools(tools)
		if err != nil {
			return StreamResult{}, err
		}
		encodedTools = t
	} else if prompt := BuildToolPrompt(tools); prompt != "" {
		// 不支持原生 FC：把工具目录塞进 system，走 prompt 式降级。
		history = append([]ChatMessage{{Role: "system", Content: prompt}}, history...)
	}

	var last StreamResult
	for round := 0; round < maxRounds; round++ {
		encMsgs, err := adapter.EncodeMessages(history)
		if err != nil {
			return last, err
		}
		stream, err := transport(ctx, encMsgs, encodedTools)
		if err != nil {
			return last, err
		}
		res, err := adapter.ParseStream(ctx, stream, onDelta)
		if err != nil {
			return last, err
		}
		last = res

		// 不支持原生 FC 时，尝试从文本里解析 prompt 式工具调用。
		if !adapter.SupportsTools() && len(res.ToolCalls) == 0 {
			if tc, ok := ParsePromptToolCall(res.Text); ok {
				res.ToolCalls = []ToolCall{tc}
			}
		}

		if len(res.ToolCalls) == 0 {
			return res, nil // 终态：纯文本回答
		}

		// 执行每个工具调用，把 assistant(tool_calls) + tool(result) 拼回历史。
		for _, tc := range res.ToolCalls {
			result, derr := dispatch(ctx, tc.Name, tc.Args)
			if derr != nil {
				result = fmt.Sprintf("[tool error] %v", derr)
			}
			history = append(history,
				ChatMessage{Role: "assistant", ToolCalls: []ToolCall{tc}},
				ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: result},
			)
		}
	}
	return last, fmt.Errorf("超出最大工具调用轮次(%d)", maxRounds)
}
