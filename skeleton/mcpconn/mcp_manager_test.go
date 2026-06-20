package mcpconn

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Msg string `json:"msg"`
}

// startInMemoryServer 起一个内存 MCP server（带一个 echo 工具），
// 返回供 client 连接的 transport。用 in-memory transport 避免起真实子进程。
func startInMemoryServer(t *testing.T, ctx context.Context) mcp.Transport {
	t.Helper()
	clientT, serverT := mcp.NewInMemoryTransports()

	srv := mcp.NewServer(&mcp.Implementation{Name: "demo-server", Version: "1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "原样返回 msg"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: in.Msg}},
			}, nil, nil
		})

	// server 必须先于 client 连接。
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	return clientT
}

func TestManagerListAndCallTool(t *testing.T) {
	ctx := context.Background()
	clientT := startInMemoryServer(t, ctx)

	m := NewManager("awy_service", "L3")
	defer m.Close()

	// 用 in-memory transport 走与 Start 相同的登记路径。
	if err := m.connect(ctx, MCPServerSpec{Name: "demo", Type: "stdio"}, clientT); err != nil {
		t.Fatalf("connect: %v", err)
	}

	tools, err := m.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d: %+v", len(tools), tools)
	}
	if tools[0].Name != "demo.echo" || tools[0].BareName != "echo" || tools[0].Server != "demo" {
		t.Fatalf("tool = %+v", tools[0])
	}

	out, err := m.CallTool(ctx, "demo.echo", map[string]any{"msg": "hello mcp"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "hello mcp" {
		t.Fatalf("out = %q", out)
	}
}

func TestManagerCallToolErrors(t *testing.T) {
	ctx := context.Background()
	m := NewManager("awy_service", "L3")
	defer m.Close()

	if _, err := m.CallTool(ctx, "noNamespace", nil); err == nil {
		t.Fatal("缺命名空间应报错")
	}
	if _, err := m.CallTool(ctx, "unknown.tool", nil); err == nil {
		t.Fatal("未连接 server 应报错")
	}
}

func TestManagerStopRemovesServer(t *testing.T) {
	ctx := context.Background()
	clientT := startInMemoryServer(t, ctx)
	m := NewManager("awy_service", "L3")
	defer m.Close()

	if err := m.connect(ctx, MCPServerSpec{Name: "demo", Type: "stdio"}, clientT); err != nil {
		t.Fatal(err)
	}
	m.Stop("demo")
	if _, err := m.CallTool(ctx, "demo.echo", map[string]any{"msg": "x"}); err == nil {
		t.Fatal("Stop 后调用应报错")
	}
}
