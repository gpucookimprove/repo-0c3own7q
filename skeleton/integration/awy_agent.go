// Package integration 给出把 skeleton 的两条轴接进 awy_service 的可编译示例。
//
// 它演示设计文档 5.5 节 ChatStreamWithTools 的完整装配：
//
//	发现层/连接层（轴1）          ──┐
//	  mcpconn.ConfigSource          │  ListTools → []agent.Tool ─┐
//	  mcpconn.Manager  ────────────┘                            │
//	                                                            ├─► RunReActLoop
//	skill（L1+L2）               ──┐                            │      （轴2 适配器）
//	  agent.LoadAllSkills          │  SkillTools → []agent.Tool ─┘
//	  agent.BuildSystemPromptWithSkills（L1：注入 system）        │
//	  agent.SkillRunner（L2：执行脚本）─► CombinedDispatcher ─────┘
//
// 搬进 awy_service 时：把本文件的逻辑放进 internal/agent/service.go 的
// ChatStreamWithTools，Transport 换成真实 HTTP/SSE，ToolSource 用 *mcpconn.Manager。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gpucookimprove/awy-agent-skeleton/agent"
	"github.com/gpucookimprove/awy-agent-skeleton/mcpconn"
)

// ToolSource 抽象 MCP 工具来源（*mcpconn.Manager 即满足此接口）。
// 抽出接口便于单测注入假实现，也让 ChatStreamWithTools 不直接耦合连接层。
type ToolSource interface {
	ListTools(ctx context.Context) ([]mcpconn.MCPTool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// 编译期断言：官方 SDK 薄封装满足 ToolSource。
var _ ToolSource = (*mcpconn.Manager)(nil)

// ChatRequest 对应设计文档 4.7 的 AgentChatReq（截取与本示例相关的字段）。
type ChatRequest struct {
	Provider string   // CC Switch 当前 provider（决定 adapter）
	Message  string   // 用户输入
	Mode     string   // 可选：对话模式说明，拼进 system
	Skills   []string // 前端勾选启用的 skill id（SkillsPanel）
	MCPTools []string // 可选：仅启用这些 "<server>.<tool>"；空=全部
}

// AgentService 是把各层组装起来的最小 agent runtime 示例。
type AgentService struct {
	Tools     ToolSource         // 轴1：MCP 工具来源
	Transport agent.LLMTransport // 轴2：真实 HTTP/SSE 发送（awy 里实现）
	SkillsDir string             // ~/.cc-switch/skills

	// 以下可选，便于定制/测试。
	MaxRounds    int                  // 防死循环；<=0 用 8
	SkillTimeout time.Duration        // 单脚本超时；<=0 用 30s
	SkillBuilder agent.CommandBuilder // skill 脚本命令构造；nil 用默认解释器
}

// ChatStreamWithTools 跑一次「带工具的多轮对话」。onDelta 收文本增量推 SSE。
func (s *AgentService) ChatStreamWithTools(
	ctx context.Context,
	req ChatRequest,
	onDelta func(agent.StreamDelta),
) (agent.StreamResult, error) {
	// 1) 加载并筛出启用的 skill（L1+L2 共用）。
	allSkills, err := agent.LoadAllSkills(s.SkillsDir)
	if err != nil {
		return agent.StreamResult{}, fmt.Errorf("加载 skills 失败: %w", err)
	}
	enabled := agent.FilterEnabled(allSkills, req.Skills)

	// 2) 汇总工具目录：MCP 工具 + skill 脚本工具（L2）。
	tools, err := s.collectTools(ctx, req.MCPTools, enabled)
	if err != nil {
		return agent.StreamResult{}, err
	}

	// 3) 组合调度：skill 工具走 SkillRunner，其余走 MCP。
	runner := agent.NewSkillRunner(enabled, s.SkillBuilder, s.SkillTimeout)
	dispatch := agent.CombinedDispatcher(runner, func(c context.Context, name string, args map[string]any) (string, error) {
		return s.Tools.CallTool(c, name, args)
	})

	// 4) 首条 system：mode + 启用 skill 正文（L1）。这里统一注入，故下方
	//    RunReActLoop 的 skills 传 nil，避免重复注入。
	history := []agent.ChatMessage{{Role: "user", Content: req.Message}}
	if sys := agent.BuildSystemPromptWithSkills(req.Mode, enabled); sys != "" {
		history = append([]agent.ChatMessage{{Role: "system", Content: sys}}, history...)
	}

	maxRounds := s.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 8
	}

	// 5) 跑 provider 无关的多轮 ReAct 循环（轴2 适配器在循环内部按 provider 选）。
	return agent.RunReActLoop(ctx, req.Provider, tools, nil, history, maxRounds, s.Transport, dispatch, onDelta)
}

// collectTools 把 MCP 工具（可按启用集过滤）转成 agent.Tool，再并上 skill 脚本工具。
func (s *AgentService) collectTools(ctx context.Context, enabledMCP []string, skills []agent.Skill) ([]agent.Tool, error) {
	var tools []agent.Tool
	if s.Tools != nil {
		mcpTools, err := s.Tools.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("列举 MCP 工具失败: %w", err)
		}
		allow := toSet(enabledMCP)
		for _, mt := range mcpTools {
			if len(allow) > 0 && !allow[mt.Name] {
				continue
			}
			t, err := toAgentTool(mt)
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
	}
	tools = append(tools, agent.SkillTools(skills)...)
	return tools, nil
}

// toAgentTool 把连接层的 MCPTool 转成 provider 无关的 agent.Tool。
// SDK 的 InputSchema 是结构化类型，这里 JSON round-trip 成 map[string]any
// （adapter 的 schema 清洗按 map 操作）。
func toAgentTool(mt mcpconn.MCPTool) (agent.Tool, error) {
	var schema map[string]any
	if mt.InputSchema != nil {
		b, err := json.Marshal(mt.InputSchema)
		if err != nil {
			return agent.Tool{}, fmt.Errorf("序列化 %q schema 失败: %w", mt.Name, err)
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			return agent.Tool{}, fmt.Errorf("反序列化 %q schema 失败: %w", mt.Name, err)
		}
	}
	return agent.Tool{Name: mt.Name, Description: mt.Description, InputSchema: schema}, nil
}

func toSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
