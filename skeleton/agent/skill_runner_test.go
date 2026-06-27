package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLoadAllSkills_PopulatesScripts(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "---\nname: Demo\n---\n正文")
	scriptsDir := filepath.Join(root, "demo", "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"search.py":  "python",
		"run.cjs":    "node",
		"build.sh":   "shell",
		"notes.txt":  "", // 未识别后缀，应被忽略
	} {
		if err := os.WriteFile(filepath.Join(scriptsDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = want
	}

	skills, err := LoadAllSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("want 1 skill, got %d", len(skills))
	}
	scripts := skills[0].Scripts
	if len(scripts) != 3 {
		t.Fatalf("want 3 recognized scripts, got %d: %+v", len(scripts), scripts)
	}
	// 排序后：build(shell), run(node), search(python)
	byName := map[string]string{}
	for _, s := range scripts {
		byName[s.Name] = s.Lang
	}
	if byName["search"] != "python" || byName["run"] != "node" || byName["build"] != "shell" {
		t.Fatalf("lang detection wrong: %+v", byName)
	}
}

func TestAsMCPToolsAndSkillTools(t *testing.T) {
	skills := []Skill{
		{ID: "demo", Name: "Demo", Description: "演示", Scripts: []SkillScript{
			{Name: "search", Path: "/x/search.py", Lang: "python"},
		}},
		{ID: "other", Name: "Other", Scripts: []SkillScript{
			{Name: "go", Path: "/x/go.sh", Lang: "shell"},
		}},
	}
	tools := SkillTools(skills)
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "skill.demo.search" {
		t.Fatalf("tool name = %q", tools[0].Name)
	}
	// schema 应是 object，含 args/stdin。
	schema := tools[0].InputSchema
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["args"]; !ok {
		t.Fatalf("missing args prop: %+v", schema)
	}
	if _, ok := props["stdin"]; !ok {
		t.Fatalf("missing stdin prop: %+v", schema)
	}
}

func TestSkillRunnerHandlesAndCombinedDispatcher(t *testing.T) {
	skills := []Skill{{ID: "demo", Scripts: []SkillScript{{Name: "search", Lang: "python"}}}}
	sr := NewSkillRunner(skills, func(ctx context.Context, sc SkillScript, args []string) (*exec.Cmd, error) {
		return nil, nil // 不会走到（路由测试）
	}, 0)

	if !sr.Handles("skill.demo.search") {
		t.Fatal("should handle registered skill tool")
	}
	if sr.Handles("codegraph.search_definition") {
		t.Fatal("should not handle MCP tool")
	}

	var mcpCalled string
	mcp := func(ctx context.Context, name string, args map[string]any) (string, error) {
		mcpCalled = name
		return "mcp-result", nil
	}
	disp := CombinedDispatcher(sr, mcp)
	out, err := disp(context.Background(), "codegraph.search_definition", nil)
	if err != nil || out != "mcp-result" || mcpCalled != "codegraph.search_definition" {
		t.Fatalf("MCP routing failed: out=%q err=%v called=%q", out, err, mcpCalled)
	}
}

func TestSkillRunner_Dispatch_ExecutesScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash 不可用，跳过脚本执行测试")
	}
	skills := []Skill{{ID: "demo", Scripts: []SkillScript{{Name: "echo", Path: "ignored", Lang: "shell"}}}}
	// 注入 builder：用 bash -c 回显 $* 与 stdin，绕开真实脚本文件 + 解释器差异。
	build := func(ctx context.Context, sc SkillScript, args []string) (*exec.Cmd, error) {
		script := `printf 'args=[%s] stdin=[' "$*"; cat; printf ']'`
		full := append([]string{"-c", script, "bash"}, args...)
		return exec.CommandContext(ctx, bash, full...), nil
	}
	sr := NewSkillRunner(skills, build, 0)

	out, err := sr.Dispatch(context.Background(), "skill.demo.echo", map[string]any{
		"args":  []any{"hello", "world"},
		"stdin": "STDIN_DATA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "args=[hello world] stdin=[STDIN_DATA]" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSkillRunner_Dispatch_ErrorAsText(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash 不可用，跳过脚本执行测试")
	}
	skills := []Skill{{ID: "demo", Scripts: []SkillScript{{Name: "fail", Lang: "shell"}}}}
	build := func(ctx context.Context, sc SkillScript, args []string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, bash, "-c", "echo boom 1>&2; exit 3"), nil
	}
	sr := NewSkillRunner(skills, build, 0)
	out, err := sr.Dispatch(context.Background(), "skill.demo.fail", nil)
	if err != nil {
		t.Fatalf("nonzero exit should be returned as text, not error: %v", err)
	}
	if out == "" || out[:13] != "[skill error]" {
		t.Fatalf("want [skill error] prefix, got %q", out)
	}
}

func TestSkillRunner_Dispatch_UnknownTool(t *testing.T) {
	sr := NewSkillRunner(nil, nil, 0)
	if _, err := sr.Dispatch(context.Background(), "skill.x.y", nil); err == nil {
		t.Fatal("unknown tool should error")
	}
}
