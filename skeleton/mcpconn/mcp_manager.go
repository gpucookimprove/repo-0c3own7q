package mcpconn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 工具名命名空间：对外用 "<server>.<tool>"，与轴2 的 NameMapper 对接
// （adapter 再把点转成 provider 安全名）。
const nsSep = "."

// MCPTool 是聚合后、provider 无关的工具条目。
type MCPTool struct {
	Server      string // 来源 server 名
	Name        string // "<server>.<tool>"
	BareName    string // server 内的原始工具名
	Description string //
	InputSchema any    // 原样 JSON Schema（来自 SDK 的 Tool.InputSchema）
}

// serverProc 持有一个已连接 server 的 session。SDK 负责进程/连接/JSON-RPC。
type serverProc struct {
	spec    MCPServerSpec
	session *mcp.ClientSession
}

// Manager 是对官方 SDK client 的薄封装：起停 server、聚合 tools、路由 tools/call。
// 替代原设计里手写的 MCPManager + MCPClient + mcp_protocol。
type Manager struct {
	mu      sync.RWMutex
	client  *mcp.Client
	servers map[string]*serverProc
}

// NewManager 创建一个 Manager。name/version 是 awy_service 自己的 client 标识。
func NewManager(name, version string) *Manager {
	return &Manager{
		client:  mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, nil),
		servers: make(map[string]*serverProc),
	}
}

// transportFor 按 spec 类型构造 SDK transport。
func transportFor(spec MCPServerSpec) (mcp.Transport, error) {
	switch spec.Type {
	case "http":
		return &mcp.StreamableClientTransport{Endpoint: spec.URL}, nil
	case "", "stdio":
		cmd := exec.Command(spec.Command, spec.Args...)
		if len(spec.Env) > 0 {
			cmd.Env = mergeEnv(spec.Env)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	default:
		return nil, fmt.Errorf("不支持的 transport 类型: %q", spec.Type)
	}
}

// Start 连接一个 server 并初始化 session（幂等：已存在则先 Stop）。
func (m *Manager) Start(ctx context.Context, spec MCPServerSpec) error {
	tr, err := transportFor(spec)
	if err != nil {
		return err
	}
	return m.connect(ctx, spec, tr)
}

// connect 用给定 transport 连接并登记 session（Start 与测试共用此接缝，
// 测试里传 in-memory transport 即可不起真实子进程）。
func (m *Manager) connect(ctx context.Context, spec MCPServerSpec, tr mcp.Transport) error {
	m.Stop(spec.Name)
	sess, err := m.client.Connect(ctx, tr, nil) // SDK：起进程 + initialize + 版本协商
	if err != nil {
		return fmt.Errorf("连接 MCP server %q 失败: %w", spec.Name, err)
	}
	m.mu.Lock()
	m.servers[spec.Name] = &serverProc{spec: spec, session: sess}
	m.mu.Unlock()
	return nil
}

// Stop 关闭一个 server 的 session（SDK 负责回收子进程）。
func (m *Manager) Stop(name string) {
	m.mu.Lock()
	proc := m.servers[name]
	delete(m.servers, name)
	m.mu.Unlock()
	if proc != nil && proc.session != nil {
		_ = proc.session.Close()
	}
}

// Restart 重连一个已知 server（健康检查失败时用）。
func (m *Manager) Restart(ctx context.Context, name string) error {
	m.mu.RLock()
	proc := m.servers[name]
	m.mu.RUnlock()
	if proc == nil {
		return fmt.Errorf("未知 server: %q", name)
	}
	return m.Start(ctx, proc.spec)
}

// Sync 把当前连接集对齐到 desired：新增的 Start、移除的 Stop、保留的不动。
// 由 ConfigSource 的 fsnotify 回调驱动，实现 ~/.claude.json 热更新。
func (m *Manager) Sync(ctx context.Context, desired []MCPServerSpec) error {
	want := make(map[string]MCPServerSpec, len(desired))
	for _, s := range desired {
		want[s.Name] = s
	}

	m.mu.RLock()
	current := make(map[string]MCPServerSpec, len(m.servers))
	for name, proc := range m.servers {
		current[name] = proc.spec
	}
	m.mu.RUnlock()

	for name := range current {
		if _, ok := want[name]; !ok {
			m.Stop(name)
		}
	}
	var errs []string
	for name, spec := range want {
		cur, ok := current[name]
		if ok && dedupKey(cur) == dedupKey(spec) {
			continue // 未变化
		}
		if err := m.Start(ctx, spec); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sync 部分失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ListTools 跨所有 server 聚合工具，名字加 "<server>." 命名空间。
func (m *Manager) ListTools(ctx context.Context) ([]MCPTool, error) {
	m.mu.RLock()
	procs := make([]*serverProc, 0, len(m.servers))
	for _, p := range m.servers {
		procs = append(procs, p)
	}
	m.mu.RUnlock()

	var out []MCPTool
	for _, p := range procs {
		res, err := p.session.ListTools(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("列举 %q 工具失败: %w", p.spec.Name, err)
		}
		for _, t := range res.Tools {
			out = append(out, MCPTool{
				Server:      p.spec.Name,
				Name:        p.spec.Name + nsSep + t.Name,
				BareName:    t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return out, nil
}

// CallTool 按 "<server>.<tool>" 路由到对应 session，返回拼接后的文本结果。
// 工具内部错误（IsError）作为文本返回，便于 LLM 自我纠正（与 SDK 约定一致）。
func (m *Manager) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	server, bare, ok := strings.Cut(name, nsSep)
	if !ok {
		return "", fmt.Errorf("工具名缺少 server 命名空间: %q", name)
	}
	m.mu.RLock()
	proc := m.servers[server]
	m.mu.RUnlock()
	if proc == nil {
		return "", fmt.Errorf("未连接的 server: %q", server)
	}

	res, err := proc.session.CallTool(ctx, &mcp.CallToolParams{Name: bare, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("调用 %q 失败: %w", name, err)
	}
	return textOf(res), nil
}

// textOf 把 CallToolResult 的 content blocks 拼成纯文本。
func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Close 关闭所有 session。
func (m *Manager) Close() {
	m.mu.Lock()
	procs := m.servers
	m.servers = make(map[string]*serverProc)
	m.mu.Unlock()
	for _, p := range procs {
		if p.session != nil {
			_ = p.session.Close()
		}
	}
}

// mergeEnv 把 spec.Env 叠加到当前进程环境。
func mergeEnv(extra map[string]string) []string {
	base := os.Environ()
	for k, v := range extra {
		base = append(base, k+"="+v)
	}
	return base
}
