package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 在 tmp 下写一个 <id>/SKILL.md。
func writeSkill(t *testing.T, root, id, content string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllSkills_ParsesFrontmatterAndBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "brainstorming", `---
name: Brainstorming
description: 在写代码前帮你做需求分析
---
请先澄清需求，再给出方案。`)
	// 没有 frontmatter 也应能加载，name 回退到目录名。
	writeSkill(t, root, "plain", "直接干活的提示语")
	// 没有 SKILL.md 的目录应被跳过。
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	skills, err := LoadAllSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(skills), skills)
	}
	// 按 ID 排序：brainstorming, plain
	bs := skills[0]
	if bs.ID != "brainstorming" || bs.Name != "Brainstorming" {
		t.Fatalf("frontmatter not parsed: %+v", bs)
	}
	if bs.Description != "在写代码前帮你做需求分析" {
		t.Fatalf("description = %q", bs.Description)
	}
	if bs.Body != "请先澄清需求，再给出方案。" {
		t.Fatalf("body = %q", bs.Body)
	}
	pl := skills[1]
	if pl.ID != "plain" || pl.Name != "plain" {
		t.Fatalf("name fallback failed: %+v", pl)
	}
	if pl.Body != "直接干活的提示语" {
		t.Fatalf("plain body = %q", pl.Body)
	}
}

func TestLoadAllSkills_MissingDirReturnsEmpty(t *testing.T) {
	skills, err := LoadAllSkills(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("want empty, got %d", len(skills))
	}
}

func TestFilterEnabled(t *testing.T) {
	all := []Skill{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := FilterEnabled(all, []string{"c", "x", "a"})
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "a" {
		t.Fatalf("filter order/unknown handling wrong: %+v", got)
	}
}

func TestBuildSystemPromptWithSkills(t *testing.T) {
	enabled := []Skill{
		{ID: "bs", Name: "Brainstorming", Description: "需求分析", Body: "先澄清需求"},
		{ID: "empty", Name: "Empty", Body: "   "}, // 空正文应被跳过
	}
	sys := BuildSystemPromptWithSkills("", enabled)
	if !strings.Contains(sys, "Brainstorming") || !strings.Contains(sys, "先澄清需求") {
		t.Fatalf("skill body not injected: %q", sys)
	}
	if strings.Contains(sys, "Empty") {
		t.Fatalf("empty-body skill should be skipped: %q", sys)
	}

	// 没有可用 skill 且无 mode → 空串。
	if got := BuildSystemPromptWithSkills("", nil); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	// 有 mode 但无 skill → 仍返回 mode。
	if got := BuildSystemPromptWithSkills("代码模式", nil); got != "代码模式" {
		t.Fatalf("mode-only prompt = %q", got)
	}
}

// TestRunReActLoop_InjectsSkillSystem 验证启用的 skill 正文进入了发往 LLM 的请求。
func TestRunReActLoop_InjectsSkillSystem(t *testing.T) {
	skills := []Skill{{ID: "bs", Name: "Brainstorming", Body: "先澄清需求"}}
	history := []ChatMessage{{Role: "user", Content: "帮我做个功能"}}

	var sentMessages []ChatMessage
	transport := func(ctx context.Context, encMsgs any, encTools any) (RawStream, error) {
		// OpenAIAdapter.EncodeMessages 产出 []map[string]any，逐条还原 role/content 校验。
		msgs, _ := encMsgs.([]map[string]any)
		sentMessages = sentMessages[:0]
		for _, m := range msgs {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			sentMessages = append(sentMessages, ChatMessage{Role: role, Content: content})
		}
		// 直接回一个无工具调用的终态文本。
		return &memStream{lines: lines(
			`{"choices":[{"delta":{"content":"好的"},"finish_reason":"stop"}]}`,
		)}, nil
	}
	dispatch := func(ctx context.Context, name string, args map[string]any) (string, error) {
		return "", nil
	}

	_, err := RunReActLoop(context.Background(), "openai", nil, skills, history, 3, transport, dispatch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sentMessages) < 2 || sentMessages[0].Role != "system" {
		t.Fatalf("first message should be injected system, got %+v", sentMessages)
	}
	if !strings.Contains(sentMessages[0].Content, "先澄清需求") {
		t.Fatalf("skill body not in system message: %q", sentMessages[0].Content)
	}
}
