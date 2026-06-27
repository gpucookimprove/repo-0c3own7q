package agent

// skill_runner.go 实现 L3 设计文档 4.6 节的 L2 路径：把 skill 的 scripts/*
// 包成工具，让 LLM 能像调 MCP 工具一样调用。与 L1（system 注入）正交，可叠加。
//
//	AsMCPTools        : 一个 skill → 每个脚本一个 Tool（名字 "skill.<id>.<script>"）
//	SkillTools        : 聚合多个启用 skill 的工具目录，并进 RunReActLoop 的 tools
//	SkillRunner       : 按工具名路由到脚本并执行，返回 stdout（错误回填为文本）
//	CombinedDispatcher: "skill." 前缀走 SkillRunner，其余走 MCP dispatcher
//
// 安全：脚本能跑任意命令，生产前需叠加设计文档 6.1 的 allow/deny 白名单 + 日志。
// 这里给出可执行骨架 + 超时；命令构造可注入（CommandBuilder）便于测试与定制。

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SkillToolPrefix 是 skill 工具名的命名空间前缀，用于路由区分 MCP 工具。
const SkillToolPrefix = "skill"

// skillToolName 拼出 "skill.<id>.<script>"。
func skillToolName(skillID, script string) string {
	return SkillToolPrefix + "." + skillID + "." + script
}

// AsMCPTools 把一个 skill 的每个脚本转成一个 provider 无关的 Tool。
// 入参 schema 统一为 {args:[]string, stdin?:string}：脚本按命令行参数 + 可选 stdin 调用。
func (s Skill) AsMCPTools() []Tool {
	out := make([]Tool, 0, len(s.Scripts))
	for _, sc := range s.Scripts {
		desc := fmt.Sprintf("运行 skill「%s」的脚本 %s", s.Name, sc.Name)
		if d := strings.TrimSpace(s.Description); d != "" {
			desc += "（" + d + "）"
		}
		out = append(out, Tool{
			Name:        skillToolName(s.ID, sc.Name),
			Description: desc,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"args": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "传给脚本的命令行参数",
					},
					"stdin": map[string]any{
						"type":        "string",
						"description": "通过标准输入传给脚本的内容（可选）",
					},
				},
				"additionalProperties": false,
			},
		})
	}
	return out
}

// SkillTools 聚合多个启用 skill 的脚本工具，供并进 RunReActLoop 的 tools。
func SkillTools(skills []Skill) []Tool {
	var out []Tool
	for _, s := range skills {
		out = append(out, s.AsMCPTools()...)
	}
	return out
}

// CommandBuilder 由调用方/测试注入：把脚本 + 解析出的 args 构造成待执行命令。
// 返回的 *exec.Cmd 的 Stdin/Stdout/Stderr 由 SkillRunner 接管。
type CommandBuilder func(ctx context.Context, sc SkillScript, args []string) (*exec.Cmd, error)

// DefaultCommandBuilder 按脚本语言族选解释器（python/node/bash），cmd/bat 直接执行。
func DefaultCommandBuilder(ctx context.Context, sc SkillScript, args []string) (*exec.Cmd, error) {
	var bin string
	var pre []string
	switch sc.Lang {
	case "python":
		bin, pre = "python", []string{sc.Path}
	case "node":
		bin, pre = "node", []string{sc.Path}
	case "shell":
		bin, pre = "bash", []string{sc.Path}
	case "cmd":
		bin, pre = sc.Path, nil
	default:
		return nil, fmt.Errorf("不支持的脚本语言: %q", sc.Lang)
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("找不到解释器 %q: %w", bin, err)
	}
	return exec.CommandContext(ctx, resolved, append(pre, args...)...), nil
}

// SkillRunner 按工具名路由到脚本并执行。零值不可用，请用 NewSkillRunner。
type SkillRunner struct {
	scripts map[string]SkillScript // 工具名 → 脚本
	build   CommandBuilder
	timeout time.Duration
}

// NewSkillRunner 用启用的 skill 建立工具名 → 脚本的路由表。
// build 为 nil 时用 DefaultCommandBuilder；timeout<=0 时默认 30s。
func NewSkillRunner(skills []Skill, build CommandBuilder, timeout time.Duration) *SkillRunner {
	if build == nil {
		build = DefaultCommandBuilder
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	m := map[string]SkillScript{}
	for _, s := range skills {
		for _, sc := range s.Scripts {
			m[skillToolName(s.ID, sc.Name)] = sc
		}
	}
	return &SkillRunner{scripts: m, build: build, timeout: timeout}
}

// Handles 判断某工具名是否由 skill runner 负责（已登记的 skill 工具）。
func (r *SkillRunner) Handles(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.scripts[name]
	return ok
}

// Dispatch 执行 name 对应的脚本，返回 stdout。脚本非零退出/超时作为文本结果返回，
// 便于 LLM 自我纠正（与 MCP 的 IsError 文本回填约定一致）。
func (r *SkillRunner) Dispatch(ctx context.Context, name string, args map[string]any) (string, error) {
	sc, ok := r.scripts[name]
	if !ok {
		return "", fmt.Errorf("未知 skill 工具: %q", name)
	}
	cliArgs, stdin := parseSkillArgs(args)

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd, err := r.build(cctx, sc, cliArgs)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	runErr := cmd.Run()
	out := stdout.String()
	if runErr != nil {
		// 错误（含非零退出/超时）回填为文本，附 stderr，不中断对话循环。
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Sprintf("[skill error] %s\n%s", msg, out), nil
	}
	return out, nil
}

// parseSkillArgs 从工具调用参数里取 args([]string) 与 stdin(string)。
// 容忍 args 是 []any（JSON 反序列化常见形态）。
func parseSkillArgs(args map[string]any) ([]string, string) {
	var cli []string
	switch v := args["args"].(type) {
	case []string:
		cli = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				cli = append(cli, s)
			} else {
				cli = append(cli, fmt.Sprint(e))
			}
		}
	}
	stdin, _ := args["stdin"].(string)
	return cli, stdin
}

// CombinedDispatcher 把 skill 工具与 MCP 工具的调度合一：已登记的 skill 工具走
// SkillRunner，其余交给 mcp。传给 RunReActLoop 的 dispatch 即可同时调两类工具。
func CombinedDispatcher(sr *SkillRunner, mcp MCPDispatcher) MCPDispatcher {
	return func(ctx context.Context, name string, args map[string]any) (string, error) {
		if sr.Handles(name) {
			return sr.Dispatch(ctx, name, args)
		}
		if mcp == nil {
			return "", fmt.Errorf("无 MCP dispatcher 处理工具: %q", name)
		}
		return mcp(ctx, name, args)
	}
}
