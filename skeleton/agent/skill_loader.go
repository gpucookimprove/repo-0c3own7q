package agent

// skill_loader.go 实现 L3 设计文档 4.6 节的 skill 加载（L1 + L2）：
//
//	扫描 ~/.cc-switch/skills/<id>/SKILL.md → 解析 frontmatter + 正文
//	→ BuildSystemPromptWithSkills 把启用的 skill 正文拼成一段 system prompt（L1）。
//	并扫描 <id>/scripts/* → Skill.Scripts，供 skill_runner.go 包成工具（L2）。
//
// 不引入 YAML 依赖：frontmatter 用首尾 "---" 包裹的简单 key: value 自解析，
// 只取 name / description 两个字段，其余忽略。

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill 是一个 CC Switch SKILL 的归一化表示。
type Skill struct {
	ID          string        // 目录名，作为前端 toggle 的稳定标识
	Name        string        // frontmatter.name，缺省回退到 ID
	Description string        // frontmatter.description
	Directory   string        // skill 目录绝对路径
	Body        string        // 去掉 frontmatter 后的 SKILL.md 正文（已 trim）
	Scripts     []SkillScript // scripts/ 下识别出的可执行脚本（L2）
}

// SkillScript 是 skill 目录 scripts/ 下的一个可执行脚本。
type SkillScript struct {
	Name string // 文件名去后缀，例如 "search"（用于工具名）
	Path string // 绝对路径
	Lang string // "python" | "node" | "shell" | "cmd"；空表示未识别（不包成工具）
}

// langForExt 按扩展名识别脚本解释器族；未知返回 ""。
func langForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".py":
		return "python"
	case ".js", ".cjs", ".mjs":
		return "node"
	case ".sh", ".bash":
		return "shell"
	case ".cmd", ".bat":
		return "cmd"
	default:
		return ""
	}
}

// loadScripts 扫描 <skillDir>/scripts/ 下可识别的脚本，按文件名排序。
func loadScripts(skillDir string) []SkillScript {
	dir := filepath.Join(skillDir, "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SkillScript
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		lang := langForExt(ext)
		if lang == "" {
			continue
		}
		out = append(out, SkillScript{
			Name: strings.TrimSuffix(e.Name(), ext),
			Path: filepath.Join(dir, e.Name()),
			Lang: lang,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DefaultSkillsDir 返回 ~/.cc-switch/skills 的绝对路径。
func DefaultSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cc-switch", "skills"), nil
}

// LoadAllSkills 扫描 skills 根目录下每个子目录的 SKILL.md，解析成 []Skill。
// 目录不存在时返回空切片而非报错（未配置 skill 是正常的）。
// 结果按 ID 排序，保证稳定可测。没有 SKILL.md 的子目录被跳过。
func LoadAllSkills(skillsDir string) ([]Skill, error) {
	entries, err := os.ReadDir(skillsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	out := make([]Skill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(skillsDir, e.Name())
		mdPath := filepath.Join(dir, "SKILL.md")
		data, rerr := os.ReadFile(mdPath)
		if rerr != nil {
			continue // 没有 SKILL.md 的目录不是 skill
		}
		sk := parseSkill(e.Name(), dir, string(data))
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// parseSkill 把 SKILL.md 内容解析成 Skill：拆 frontmatter（name/description）+ 正文。
func parseSkill(id, dir, content string) Skill {
	fm, body := splitFrontmatter(content)
	name := fm["name"]
	if name == "" {
		name = id
	}
	return Skill{
		ID:          id,
		Name:        name,
		Description: fm["description"],
		Directory:   dir,
		Body:        strings.TrimSpace(body),
		Scripts:     loadScripts(dir),
	}
}

// splitFrontmatter 解析首尾 "---" 包裹的 YAML 风格 frontmatter（仅 key: value）。
// 没有 frontmatter 时，返回空 map + 原文。
func splitFrontmatter(content string) (map[string]string, string) {
	fm := map[string]string{}
	s := strings.TrimLeft(content, "\ufeff") // 去 BOM
	if !strings.HasPrefix(s, "---") {
		return fm, content
	}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// 第一行必须是 "---"
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return fm, content
	}
	var bodyLines []string
	closed := false
	for sc.Scan() {
		line := sc.Text()
		if !closed && strings.TrimSpace(line) == "---" {
			closed = true
			continue
		}
		if closed {
			bodyLines = append(bodyLines, line)
			continue
		}
		if k, v, ok := parseKV(line); ok {
			fm[k] = v
		}
	}
	if !closed {
		// frontmatter 没闭合：当作没有 frontmatter，原文返回。
		return map[string]string{}, content
	}
	return fm, strings.Join(bodyLines, "\n")
}

// parseKV 解析 "key: value"，去引号；非法行返回 ok=false。
func parseKV(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	v = strings.Trim(v, `"'`)
	if k == "" {
		return "", "", false
	}
	return k, v, true
}

// FilterEnabled 按 id 列表从全集里挑出启用的 skill，保持 ids 的顺序。
// 未知 id 被忽略。前端 SkillsPanel 勾选后把 id 列表带到后端，用这个过滤。
func FilterEnabled(all []Skill, ids []string) []Skill {
	idx := make(map[string]Skill, len(all))
	for _, s := range all {
		idx[s.ID] = s
	}
	out := make([]Skill, 0, len(ids))
	for _, id := range ids {
		if s, ok := idx[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// BuildSystemPromptWithSkills 把启用的 skill 正文拼成一段 system prompt。
// mode 为可选的对话模式说明（为空则省略）。没有启用的 skill 时返回 ""，
// 调用方据此决定是否注入 system 消息。
//
// 对应设计文档 5.5 的 buildSystemPromptWithSkills(req.Mode, req.EnabledSkills)。
func BuildSystemPromptWithSkills(mode string, enabled []Skill) string {
	var b strings.Builder
	if m := strings.TrimSpace(mode); m != "" {
		b.WriteString(m)
		b.WriteString("\n\n")
	}
	hasSkill := false
	for _, s := range enabled {
		if strings.TrimSpace(s.Body) == "" {
			continue
		}
		if !hasSkill {
			b.WriteString("你可以使用以下技能（Skills）。请在合适时遵循其中的指引：\n")
			hasSkill = true
		}
		title := s.Name
		if title == "" {
			title = s.ID
		}
		b.WriteString("\n## Skill: ")
		b.WriteString(title)
		if d := strings.TrimSpace(s.Description); d != "" {
			b.WriteString(" — ")
			b.WriteString(d)
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(s.Body))
		b.WriteString("\n")
	}
	if !hasSkill && strings.TrimSpace(mode) == "" {
		return ""
	}
	return strings.TrimSpace(b.String())
}
