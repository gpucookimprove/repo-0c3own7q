// Package mcpconn 是 L3 设计文档 R1 连接层定稿的可落地骨架：
//
//   - config_source.go：发现层 —— 读 ~/.claude.json 的 mcpServers 作为权威来源，
//     归一化成 []MCPServerSpec、按 command+args 去重，并用 fsnotify 监听热更新。
//   - mcp_manager.go：协议层 + 进程层 —— 用官方 modelcontextprotocol/go-sdk
//     的 client/session 起停 MCP server，聚合 tools、路由 tools/call。
//
// 把这两个文件搬进 awy_service 的 internal/agent/ 即可（包名按 awy 约定改）。
package mcpconn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// MCPServerSpec 是 provider 无关的、归一化后的单个 MCP server 配置。
type MCPServerSpec struct {
	Name    string            // mcpServers 的 key，例如 "codegraph"
	Command string            // stdio：可执行文件，例如 "codegraph"
	Args    []string          // stdio：参数，例如 ["serve","--mcp"]
	Env     map[string]string // 额外环境变量
	Type    string            // "stdio" | "http"
	URL     string            // http 类：Streamable HTTP endpoint
}

// rawServer 对应 ~/.claude.json 里 mcpServers 的一项。
// 兼容两种形态：stdio（command/args/env）与 http（type:"http"/url）。
type rawServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Type    string            `json:"type"`
	URL     string            `json:"url"`
}

// claudeConfig 只取我们关心的 mcpServers 字段，其余忽略。
type claudeConfig struct {
	MCPServers map[string]rawServer `json:"mcpServers"`
}

// DefaultClaudeConfigPath 返回 ~/.claude.json 的绝对路径。
func DefaultClaudeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// LoadFromClaudeJSON 读取并归一化 mcpServers，按 command+args（http 按 url）去重。
// 文件不存在时返回空切片而非报错（首次未配置是正常的）。
func LoadFromClaudeJSON(path string) ([]MCPServerSpec, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	return parseClaudeConfig(data)
}

func parseClaudeConfig(data []byte) ([]MCPServerSpec, error) {
	var cfg claudeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 claude.json 失败: %w", err)
	}

	// 按名字排序后处理，保证去重结果稳定可测。
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := map[string]bool{}
	out := make([]MCPServerSpec, 0, len(names))
	for _, name := range names {
		r := cfg.MCPServers[name]
		spec := normalize(name, r)
		key := dedupKey(spec)
		if seen[key] {
			continue // 同一个 server 被写进多份配置：去重
		}
		seen[key] = true
		out = append(out, spec)
	}
	return out, nil
}

func normalize(name string, r rawServer) MCPServerSpec {
	typ := r.Type
	if typ == "" {
		if r.URL != "" {
			typ = "http"
		} else {
			typ = "stdio"
		}
	}
	return MCPServerSpec{
		Name:    name,
		Command: r.Command,
		Args:    r.Args,
		Env:     r.Env,
		Type:    typ,
		URL:     r.URL,
	}
}

// dedupKey：http 按 url，stdio 按 command+args（忽略 name/env）。
func dedupKey(s MCPServerSpec) string {
	if s.Type == "http" {
		return "http:" + s.URL
	}
	return "stdio:" + s.Command + "\x00" + strings.Join(s.Args, "\x00")
}

// ConfigSource 监听 ~/.claude.json，变更时回调最新的归一化 specs。
type ConfigSource struct {
	path    string
	watcher *fsnotify.Watcher
}

// NewConfigSource 创建监听器但不开始监听；path 为空时用默认路径。
func NewConfigSource(path string) (*ConfigSource, error) {
	if path == "" {
		p, err := DefaultClaudeConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &ConfigSource{path: path, watcher: w}, nil
}

// Load 读取当前 specs（不依赖 watcher，可单独调用）。
func (c *ConfigSource) Load() ([]MCPServerSpec, error) {
	return LoadFromClaudeJSON(c.path)
}

// Watch 监听配置文件所在目录的写入/重命名事件，去抖后回调最新 specs。
// 监听目录而非文件本身：很多编辑器是「写临时文件 + rename」，直接 watch 文件会丢事件。
// 阻塞直到 done 关闭；返回时自动关闭 watcher。
func (c *ConfigSource) Watch(done <-chan struct{}, onChange func([]MCPServerSpec)) error {
	dir := filepath.Dir(c.path)
	if err := c.watcher.Add(dir); err != nil {
		return err
	}
	defer c.watcher.Close()

	target := filepath.Clean(c.path)
	for {
		select {
		case <-done:
			return nil
		case ev, ok := <-c.watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(ev.Name) != target {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			specs, err := c.Load()
			if err != nil {
				continue // 文件可能正被写到一半，等下一个事件
			}
			if onChange != nil {
				onChange(specs)
			}
		case _, ok := <-c.watcher.Errors:
			if !ok {
				return nil
			}
		}
	}
}

// Close 释放 watcher（Watch 正常返回时已关闭，这里幂等兜底）。
func (c *ConfigSource) Close() error {
	if c.watcher != nil {
		return c.watcher.Close()
	}
	return nil
}
