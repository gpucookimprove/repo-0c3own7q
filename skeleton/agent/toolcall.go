// Package agent contains a landable skeleton for the multi-provider tool-calling
// layer described in agent-skill-mcp-l3-design.md (section 七).
//
// 两条轴是解耦的：
//   - 轴1 MCP server（工具来源）：统一成一份 []Tool，provider 无关。
//   - 轴2 LLM provider：每个协议族一个 ToolCallingAdapter。
//
// 本文件定义共享类型 + ToolCallingAdapter 接口 + 工具名命名空间转换。
// 各 provider 的编解码在 adapter_*.go，JSON Schema 清洗在 schema.go。
package agent

import (
	"context"
	"regexp"
	"strings"
	"sync"
)

// Tool 是 provider 无关的工具定义（直接来自 MCP tools/list）。
// Name 用 "<server>.<tool>" 命名空间，例如 "codegraph.search_definition"。
type Tool struct {
	Name        string         // "<server>.<tool>"，可能含点
	Description string         //
	InputSchema map[string]any // JSON Schema（MCP 原样）
}

// ToolCall 是从任意 provider 解析出来的、归一化后的一次工具调用。
type ToolCall struct {
	ID   string         // provider 给的调用 id（回填结果时要带回）
	Name string         // 已 denamespace 回 "<server>.<tool>"
	Args map[string]any // 解析后的参数
}

// ChatMessage 是 awy_service 内部统一的消息表示。各 adapter 负责把它
// 编码成本家 provider 的 wire 格式（OpenAI role:tool / Anthropic content block …）。
type ChatMessage struct {
	Role       string     // "system" | "user" | "assistant" | "tool"
	Content    string     //
	ToolCalls  []ToolCall // assistant 轮里模型请求的工具调用
	ToolCallID string     // role=="tool" 时，对应的 ToolCall.ID
}

// StreamDelta 是流式回调里推给前端 SSE 的增量。
type StreamDelta struct {
	Text string // 文本增量
}

// StreamResult 是一轮 LLM 调用的归一化结果。
type StreamResult struct {
	Text         string     // 本轮累计文本
	ToolCalls    []ToolCall // 本轮模型请求的工具调用（可能多个）
	FinishReason string     // "stop" | "tool_calls" | ...
}

// ToolCallingAdapter 把统一的工具目录 + 消息历史适配到某个 provider 协议族。
// 实现：OpenAIAdapter（≈所有国产）/ AnthropicAdapter / GeminiAdapter。
type ToolCallingAdapter interface {
	// Family 返回协议族名，用于日志/路由（"openai" | "anthropic" | "gemini"）。
	Family() string

	// SupportsTools：provider 是否支持原生 function-calling。
	// 返回 false 时上层走 prompt 式降级（见 fallback.go）。
	SupportsTools() bool

	// EncodeTools 把统一工具目录转成本家 tools spec（已做命名清洗 + schema 降级）。
	// 返回值直接塞进该 provider 的请求体。
	EncodeTools(tools []Tool) (any, error)

	// EncodeMessages 把统一历史转成本家 messages 数组（含 tool_call / tool_result 形态）。
	EncodeMessages(msgs []ChatMessage) (any, error)

	// ParseStream 读 provider 的流式响应，边推 onDelta 边累计，
	// 返回归一化结果（含已 denamespace 的 ToolCall）。
	// transport 实现因 provider 而异，这里给出可复用的解析骨架（见各 adapter）。
	ParseStream(ctx context.Context, raw RawStream, onDelta func(StreamDelta)) (StreamResult, error)
}

// RawStream 抽象底层 HTTP/SSE 流，让 adapter 的 ParseStream 可单测。
// 真实实现包一层 *http.Response.Body 的 SSE reader；测试里用内存实现。
type RawStream interface {
	// Next 返回下一条 SSE 事件的 data 行（不含 "data: " 前缀）；io.EOF 结束。
	Next() ([]byte, error)
}

// ---------------------------------------------------------------------------
// 工具名命名空间转换
//
// MCP 用 "<server>.<tool>"，但 OpenAI/Gemini 的工具名必须匹配
// ^[a-zA-Z0-9_-]+$（不允许点）。所以对外用安全名 "<server>__<tool>"，
// 并维护一张安全名 → 原名 的映射，解析 tool_call 时再还原。
// ---------------------------------------------------------------------------

const nsSep = "__"

var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// NameMapper 在「MCP 原名」与「provider 安全名」之间双向映射，并发安全。
type NameMapper struct {
	mu        sync.RWMutex
	safeToRaw map[string]string
}

// NewNameMapper 创建空映射表。
func NewNameMapper() *NameMapper {
	return &NameMapper{safeToRaw: make(map[string]string)}
}

// ToSafe 把 "codegraph.search_definition" → "codegraph__search_definition"，
// 并记录反向映射。任何非法字符都会被替换成 '_'。
func (m *NameMapper) ToSafe(raw string) string {
	safe := strings.ReplaceAll(raw, ".", nsSep)
	if !safeNameRe.MatchString(safe) {
		var b strings.Builder
		for _, r := range safe {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		safe = b.String()
	}
	m.mu.Lock()
	m.safeToRaw[safe] = raw
	m.mu.Unlock()
	return safe
}

// ToRaw 把 provider 返回的安全名还原为 MCP 原名。
// 没有记录时尽力还原（首个 "__" 还原成 "."），保证不丢调用。
func (m *NameMapper) ToRaw(safe string) string {
	m.mu.RLock()
	raw, ok := m.safeToRaw[safe]
	m.mu.RUnlock()
	if ok {
		return raw
	}
	return strings.Replace(safe, nsSep, ".", 1)
}
