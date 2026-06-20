package mcpconn

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseClaudeConfigNormalizeAndDedup(t *testing.T) {
	data := []byte(`{
      "numFiles": 3,
      "mcpServers": {
        "codegraph": {"command": "codegraph", "args": ["serve", "--mcp"]},
        "node_repl": {"command": "node_repl.exe", "env": {"FOO": "bar"}},
        "codegraph_dup": {"command": "codegraph", "args": ["serve", "--mcp"]},
        "remote": {"type": "http", "url": "https://example.com/mcp"}
      }
    }`)
	specs, err := parseClaudeConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	// codegraph_dup 与 codegraph 同 command+args → 去重，剩 3 个。
	if len(specs) != 3 {
		t.Fatalf("specs = %d, want 3: %+v", len(specs), specs)
	}
	byName := map[string]MCPServerSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if byName["node_repl"].Type != "stdio" || byName["node_repl"].Env["FOO"] != "bar" {
		t.Fatalf("node_repl = %+v", byName["node_repl"])
	}
	if byName["remote"].Type != "http" || byName["remote"].URL != "https://example.com/mcp" {
		t.Fatalf("remote = %+v", byName["remote"])
	}
	if _, ok := byName["codegraph_dup"]; ok {
		t.Fatalf("dup not removed")
	}
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	specs, err := LoadFromClaudeJSON(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %v", specs)
	}
}

func TestConfigSourceWatchFiresOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := NewConfigSource(path)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan []MCPServerSpec, 1)
	done := make(chan struct{})
	go func() { _ = cs.Watch(done, func(s []MCPServerSpec) { got <- s }) }()
	defer close(done)

	time.Sleep(200 * time.Millisecond) // 让 watcher 先 Add 目录
	newCfg := `{"mcpServers":{"codegraph":{"command":"codegraph","args":["serve","--mcp"]}}}`
	if err := os.WriteFile(path, []byte(newCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case specs := <-got:
		if len(specs) != 1 || specs[0].Name != "codegraph" {
			t.Fatalf("specs = %+v", specs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 5s 内回调")
	}
}
