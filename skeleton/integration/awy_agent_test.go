package integration

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gpucookimprove/awy-agent-skeleton/agent"
	"github.com/gpucookimprove/awy-agent-skeleton/mcpconn"
)

// fakeToolSource 是 ToolSource 的内存实现，免起真实 MCP server。
type fakeToolSource struct {
	tools  []mcpconn.MCPTool
	called []string
}

func (f *fakeToolSource) ListTools(ctx context.Context) ([]mcpconn.MCPTool, error) {
	return f.tools, nil
}
func (f *fakeToolSource) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	f.called = append(f.called, name)
	return "mcp:" + name, nil
}

// memStream 是 agent.RawStream 的内存实现。
type memStream struct {
	lines [][]byte
	i     int
}

func (m *memStream) Next() ([]byte, error) {
	if m.i >= len(m.lines) {
		return nil, io.EOF
	}
	l := m.lines[m.i]
	m.i++
	return l, nil
}

func line(s string) [][]byte { return [][]byte{[]byte(s)} }

// writeSkillWithScript 在 root 下建一个带 SKILL.md + scripts/echo.sh 的 skill。
func writeSkillWithScript(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: Demo\ndescription: 演示技能\n---\n请遵循演示流程")
	writeFile(t, filepath.Join(dir, "scripts", "echo.sh"), "echo hi")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestChatStream_AssemblesToolsAndSystem 验证：MCP 工具 + skill 工具都进了
// tools，且首条 system 同时含 mode 与启用 skill 的正文（L1）。
func TestChatStream_AssemblesToolsAndSystem(t *testing.T) {
	root := t.TempDir()
	writeSkillWithScript(t, root, "demo")

	fts := &fakeToolSource{tools: []mcpconn.MCPTool{{
		Server: "codegraph", Name: "codegraph.search", BareName: "search",
		Description: "搜代码", InputSchema: map[string]any{"type": "object"},
	}}}

	var gotTools []map[string]any
	var gotMsgs []map[string]any
	transport := func(ctx context.Context, encMsgs any, encTools any) (agent.RawStream, error) {
		gotMsgs, _ = encMsgs.([]map[string]any)
		gotTools, _ = encTools.([]map[string]any)
		return &memStream{lines: line(`{"choices":[{"delta":{"content":"好的"},"finish_reason":"stop"}]}`)}, nil
	}

	svc := &AgentService{Tools: fts, Transport: transport, SkillsDir: root}
	_, err := svc.ChatStreamWithTools(context.Background(), ChatRequest{
		Provider: "openai",
		Message:  "帮我搜代码",
		Mode:     "代码模式",
		Skills:   []string{"demo"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 工具：codegraph.search + skill.demo.echo
	names := toolNames(gotTools)
	if !names["codegraph__search"] {
		t.Fatalf("MCP tool missing: %v", names)
	}
	if !names["skill__demo__echo"] {
		t.Fatalf("skill tool missing: %v", names)
	}

	// system 同时含 mode + skill 正文
	if len(gotMsgs) == 0 || gotMsgs[0]["role"] != "system" {
		t.Fatalf("first message not system: %+v", gotMsgs)
	}
	sys, _ := gotMsgs[0]["content"].(string)
	if !strings.Contains(sys, "代码模式") || !strings.Contains(sys, "请遵循演示流程") {
		t.Fatalf("system missing mode/skill body: %q", sys)
	}
}

// TestChatStream_RoutesSkillToolCall 验证：LLM 发起 skill 工具调用时走 SkillRunner
// 执行脚本，而 MCP 工具调用走 ToolSource。
func TestChatStream_RoutesSkillToolCall(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	root := t.TempDir()
	writeSkillWithScript(t, root, "demo")
	fts := &fakeToolSource{}

	round := 0
	transport := func(ctx context.Context, encMsgs any, encTools any) (agent.RawStream, error) {
		round++
		if round == 1 {
			// 第一轮：请求调用 skill.demo.echo（安全名带 __）。
			return &memStream{lines: line(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"skill__demo__echo","arguments":"{\"args\":[\"hi\"]}"}}]},"finish_reason":"tool_calls"}]}`,
			)}, nil
		}
		// 第二轮：看到 tool 结果后给终态文本。
		return &memStream{lines: line(`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)}, nil
	}

	// 注入 builder：用 bash 回显参数，绕开真实脚本文件差异。
	bash, _ := exec.LookPath("bash")
	builder := func(ctx context.Context, sc agent.SkillScript, args []string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, bash, append([]string{"-c", `printf 'ran %s' "$*"`, "bash"}, args...)...), nil
	}

	svc := &AgentService{Tools: fts, Transport: transport, SkillsDir: root, SkillBuilder: builder}
	_, err := svc.ChatStreamWithTools(context.Background(), ChatRequest{
		Provider: "openai", Message: "跑下 echo", Skills: []string{"demo"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if round < 2 {
		t.Fatalf("expected a second round after tool call, rounds=%d", round)
	}
	if len(fts.called) != 0 {
		t.Fatalf("skill call should NOT hit MCP source, got %v", fts.called)
	}
}

func toolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if n, ok := fn["name"].(string); ok {
			out[n] = true
		}
	}
	return out
}
