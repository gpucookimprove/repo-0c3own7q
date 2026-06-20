# awy_service Agent Skills / MCP（L3）完整设计方案

> 目标：让 awy_service 同时充当 **MCP Client（消费 CC Switch 注册的 MCP server）**
> 和 **MCP Host（把 LLM 包装成支持 MCP tool/resource/prompt 的 agent runtime）**，
> 实现和 Claude Desktop / Cursor 同等级的工具能力，并保留 P9 / B 套餐的速度优势。
>
> **R1 修订（连接层定稿）**：保留 L3 的多轮 ReAct 循环 + MCPRouter，但把「连接层」
> 换成：**(A) 读 `~/.claude.json` 的 `mcpServers` 作为权威来源**（不再抠 CC Switch 私有
> db）+ **(B) 用官方 MCP Go SDK 取代手写 JSON-RPC**；单机自用让 SDK 直接管 stdio
> server 进程（不另起 gateway）。详见下方「零、连接层定稿（R1）+ 落地改动清单」。

---

## 零、连接层定稿（R1）+ 落地改动清单

> 决策：**保留** 多轮 ReAct/MCPRouter/tool_calls（轴2 那套照旧），**只换发现层 + 协议层**。

### 0.1 三层定稿

| 层 | 原设计 | R1 定稿 | 理由 |
| -- | ------ | ------- | ---- |
| 发现层 | 读 `~/.cc-switch/cc-switch.db` 的 `mcp_servers` 表 | **读 `~/.claude.json` 的 `mcpServers`** + `fsnotify` 监听；`cc-switch.db` 仅作一次性导入/兜底 | 私有 schema 易碎、抢文件锁、拿不到实时变更；`~/.claude.json` 是 CC Switch 已同步的公开标准格式（与 Claude Desktop 同形），关掉的 server 已自动移除 |
| 协议层 | 手写 `mcp_client.go` + `mcp_protocol.go`（JSON-RPC over stdio，2024-11-05） | **官方 `modelcontextprotocol/go-sdk` client** | 白拿 stdio + Streamable HTTP 传输、initialize/list/call、协议版本协商、cancel、重连；删 ~600 行易碎代码 |
| 进程层 | awy_service 自 spawn + 自写健康检查 | 单机自用：**SDK 直接管 stdio server**（`MCPManager` 退化成对 SDK session 的薄封装）；要远程/多用户再加 gateway | 单机省一个常驻组件；与 CLI 工具的「谁来 spawn」边界靠 awy_service 自己的启用集划清 |

### 0.2 落地改动清单（按文件）

**新增**
- `internal/agent/config_source.go`：读 `~/.claude.json` 的 `mcpServers` → `[]MCPServerSpec`；`fsnotify` 监听变更→ 通知 `MCPManager` 增量启停；提供 `ImportFromCCSwitch()` 仅作一次性导入。

**改写（取代原计划的新增文件）**
- ~~`mcp_protocol.go`~~ / ~~`mcp_client.go`~~：**删除**，改用官方 SDK 的 `mcp.Client` / `mcp.ClientSession` 与其类型。
- `mcp_manager.go`：不再手 `os/exec` + 手写 JSON-RPC；改为用 SDK 的 stdio transport 起 session，`Start/Stop/Restart/ListTools/CallTool` 全部转调 SDK。spec 来源从 `config_source.go`。
- `mcp_router.go`：**保留**。`LLMToolSpecs`/`Dispatch` 接口不变，底层 `CallTool` 走 SDK。
- `skill_loader.go` / `skill_runner_server.go`：**保留**思路，内置 skill server 也用 SDK 的 server 端实现暴露。
- `service.go`（`ChatStreamWithTools` 多轮循环）：**保留**，不动。
- `config.go`：`MCPAllowedTools`/`MCPDeniedTools` **保留**；去掉 cc-switch.db 路径配置，新增 `ClaudeConfigPath`（默认 `~/.claude.json`）。

**依赖**
- 新增 `github.com/modelcontextprotocol/go-sdk`。去掉「读 cc-switch.db」对 `modernc.org/sqlite` 的依赖（除非保留导入兜底）。

**轴2（多 provider）**：见 `ccswitch-integration-alternatives.md` 第七节，`ToolCallingAdapter`（OpenAI 兼容/Anthropic/Gemini）+ 工具名命名空间 + Schema 清洗，已在 `skeleton/` 给出可编译实现，独立于本连接层改动。

### 0.3 实施顺序
1. `config_source.go` 读 `~/.claude.json` + 去重 + `fsnotify`（可单测，先不接 SDK）。
2. 引入官方 SDK，重写 `mcp_manager.go` 用 SDK session 起 codegraph/node_repl，跑通 `tools/list` + `tools/call`。
3. 接回 `mcp_router.go` + `service.go` 多轮循环（这两层不改）。
4. `config.go` 去 db 路径、加 `ClaudeConfigPath`；保留 allow/deny。
5. SkillLoader 走 SDK server 暴露。

---

## 一、什么是 MCP（背景）

MCP = **Model Context Protocol**，由 Anthropic 在 2025 年定义的 JSON-RPC 协议。
把 LLM 与外部能力解耦成三件事：

| 概念        | 说明                              | 例子                                         |
| ----------- | --------------------------------- | -------------------------------------------- |
| **Tools**     | LLM 可调用的函数                  | `read_file(path)` / `run_sql(query)` / `search_web(q)` |
| **Resources** | LLM 可以"读"的只读数据源           | `git://repo/HEAD` / `file:///path/to/spec.md` |
| **Prompts**   | 预制的 prompt 模板（含可填变量）    | "做代码 review" / "写 commit message"        |

每个 MCP server 是一个独立进程，通过 **stdio** 或 **SSE/HTTP** 与 host 通信。

CC Switch 管理你机器上的 MCP server 配置，并在启用时**同步写入各 CLI 工具的标准
配置文件**。awy_service 的**权威来源取 `~/.claude.json` 的 `mcpServers`**（公开、与
Claude Desktop 同形、关掉的 server 已自动移除），而**不是** CC Switch 的私有
`cc-switch.db`（R1 修订，理由见「零」节）。本机当前已注册的（示例）：

```jsonc
// ~/.claude.json
{
  "mcpServers": {
    "codegraph": { "command": "codegraph", "args": ["serve", "--mcp"] },
    "node_repl": { "command": "node_repl.exe", "env": { /* ... */ } }
  }
}
```

社区还有大量可选 MCP server：
`mcp-server-filesystem`、`mcp-server-git`、`mcp-server-sqlite`、`mcp-server-postgres`、
`mcp-server-puppeteer`、`mcp-server-fetch` 等。

---

## 二、L3 架构总览

```
┌─────────────────────────────────────────────────┐
│  awy_web 前端                                    │
│  /agent/chat 视图                                │
│  - Agent / Provider / Skills / MCP-tools 选择    │
└─────────────────────────────────────────────────┘
                  │ POST /api/agent/chat-stream
                  ▼
┌─────────────────────────────────────────────────┐
│  awy_service                                    │
│  internal/agent/                                │
│                                                 │
│  ┌───────────────────────────────────────────┐ │
│  │  ChatStream Loop (新)                     │ │
│  │  ┌──────────┐                             │ │
│  │  │  LLM     │ ◄── system + tools spec     │ │
│  │  │ (火山/   │                             │ │
│  │  │  Claude) │ ── tool_call ──┐            │ │
│  │  └──────────┘                ▼            │ │
│  │       ▲              ┌──────────────┐     │ │
│  │       │              │ MCPRouter    │     │ │
│  │       │              │ (新)         │     │ │
│  │       │              └──────────────┘     │ │
│  │       │                     │             │ │
│  │       │                     ▼             │ │
│  │  tool_result ◄── execute(tool, args)      │ │
│  └───────────────────────────────────────────┘ │
│                  │                             │
│                  ▼                             │
│  ┌──────────────────────────────────────────┐  │
│  │  MCPManager (新)                         │  │
│  │  ── manages multiple MCP servers ──      │  │
│  │  • spawn(stdio cmd) → process            │  │
│  │  • initialize / list_tools / list_res    │  │
│  │  • call_tool(name, args) → result        │  │
│  │  • health check + reconnect              │  │
│  └──────────────────────────────────────────┘  │
│                  │                             │
└──────────────────┼─────────────────────────────┘
                   │ stdio JSON-RPC
        ┌──────────┼─────────────┐
        ▼          ▼             ▼
   ┌─────────┐┌─────────┐  ┌──────────┐
   │codegraph││node_repl│  │filesystem│
   │  MCP    ││  MCP    │  │   MCP    │
   │ server  ││ server  │  │  server  │
   └─────────┘└─────────┘  └──────────┘
```

---

## 三、目录结构（新增 / 修改）

> R1：`mcp_protocol.go` / `mcp_client.go` 被官方 SDK 取代（删除）；新增 `config_source.go`。

```
awy_service/
  internal/agent/
    config_source.go         （新 R1）读 ~/.claude.json mcpServers + fsnotify 监听 + 去重
    mcp_manager.go           （改 R1）用官方 SDK session 启停 server，不再手 os/exec
    mcp_router.go            （新）把启用的 MCP tools 聚合 → 暴露给 LLM（保留）
    skill_loader.go          （新）扫描 ~/.cc-switch/skills，转成 prompts/tools
    skill_runner_server.go   （新）内置 MCP server（SDK server 端）跑 SKILL scripts

    # 删除（R1）：mcp_protocol.go / mcp_client.go → 改用 github.com/modelcontextprotocol/go-sdk

    llm_client.go            （改）支持 OpenAI/Anthropic/Gemini 的 tool_calls 协议（见 alternatives 第七节 + skeleton/）
    service.go               （改）ChatStream 改为多轮 ReAct 循环（保留）
    config.go                （改）+MCPAllowedTools / MCPDeniedTools +ClaudeConfigPath（去 db 路径）

  internal/handler/agent/
    agent_chat_stream_handler.go  （改）多轮 tool_call 时仍流式
    agent_skills_handler.go  （新）GET /api/agent/skills, POST /api/agent/skills/toggle
    agent_mcp_handler.go     （新）GET /api/agent/mcp/status, POST /api/agent/mcp/restart

awy_web/playground/src/
  api/agent/index.ts         （改）新增 mcp/skills API + ChatStream 协议扩展
  views/agent/index.vue      （改）工具条 +「Skills」+「MCP Tools」抽屉
  views/agent/SkillsPanel.vue          （新）Skills + MCP servers 管理面板
  views/agent/components/ToolProgressBubble.vue  （新）"🔧 正在调用 xxx" 进度气泡
```

---

## 四、核心模块设计

### 4.1 MCPManager（进程管理，R1：薄封装官方 SDK）

> R1：`MCPServerSpec` 来源改为 `config_source.go`（读 `~/.claude.json`）；`MCPClient`
> 字段换成官方 SDK 的 `*mcp.ClientSession`，进程 + JSON-RPC 全交给 SDK。

```go
// internal/agent/mcp_manager.go
type MCPServerSpec struct {
    Name    string            // ~/.claude.json mcpServers 的 key
    Command string            // stdio：例如 "codegraph"
    Args    []string          // 例如 ["serve", "--mcp"]
    Env     map[string]string
    Type    string            // "stdio" / "http"（优先 Streamable HTTP）
    URL     string            // http 类用
}

type MCPServerProcess struct {
    Spec    MCPServerSpec
    Session *mcp.ClientSession // 官方 SDK：进程/连接/JSON-RPC 都在里面
    Healthy atomic.Bool
}

type MCPManager struct {
    mu       sync.RWMutex
    client   *mcp.Client        // 官方 SDK client
    servers  map[string]*MCPServerProcess
}

// 关键方法（签名不变，底层全转调 SDK）
func (m *MCPManager) Start(spec MCPServerSpec) error    // SDK: client.Connect(stdio/http transport)
func (m *MCPManager) Stop(name string)                  // session.Close()
func (m *MCPManager) Restart(name string) error
func (m *MCPManager) ListTools() []MCPTool              // 跨所有 session 聚合 session.ListTools
func (m *MCPManager) CallTool(name string, args map[string]any) (any, error) // session.CallTool
func (m *MCPManager) Sync(specs []MCPServerSpec)        // 与 config_source 的最新集对账，增量启停
```

启停策略（R1）：

- 启动时 `config_source.go` 读 `~/.claude.json` 的 `mcpServers` → `[]MCPServerSpec`（去重），
  对每个 spec 用 SDK 起 session（stdio transport 起子进程；http 直接连）
- `fsnotify` 监听 `~/.claude.json` 变更 → `MCPManager.Sync()` 增量启停（解决实时性）
- 进程崩溃/连接断 → SDK 负责重连；外层再叠加 5s 指数退避、最多 3 次
- 主进程退出时 `session.Close()` 优雅关闭（SDK 处理子进程收尾）
- awy_service 维护**自己的启用集**（SkillsPanel 勾选），与「CC Switch 给哪个 CLI 启用」解耦

### 4.2 MCPClient → 官方 SDK（R1：删除手写实现）

> R1 修订：**不再手写 JSON-RPC**。改用官方 `github.com/modelcontextprotocol/go-sdk`，
> 它已覆盖 initialize/`tools`/`resources`/`prompts`/通知，以及 **stdio + Streamable HTTP**
> 两种传输、协议版本协商、cancel、重连。目标 spec 跟随 SDK（当前 `2025-06-18`），
> 不再锁死 `2024-11-05`。

```go
// 用官方 SDK 起一个 stdio server 的 client session：
import "github.com/modelcontextprotocol/go-sdk/mcp"

client := mcp.NewClient(&mcp.Implementation{Name: "awy_service", Version: "L3"}, nil)

// stdio：SDK 负责 spawn 子进程 + JSON-RPC
tr := &mcp.CommandTransport{Command: exec.Command(spec.Command, spec.Args...)}
sess, err := client.Connect(ctx, tr, nil)

tools, _ := sess.ListTools(ctx, nil)
res, _ := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
// http 类则用 mcp.StreamableClientTransport{Endpoint: spec.URL}
```

原 `MCPClient` / `mcp_protocol.go` 的所有手写类型与生命周期代码全部由 SDK 取代删除。

### 4.3 MCP 协议核心 types

```go
// internal/agent/mcp_protocol.go
type Tool struct {
    Name        string         `json:"name"`
    Description string         `json:"description"`
    InputSchema map[string]any `json:"inputSchema"`  // JSON Schema
}

type ToolResult struct {
    Content []ToolContent `json:"content"`
    IsError bool          `json:"isError,omitempty"`
}

type ToolContent struct {
    Type string `json:"type"`  // "text" / "image" / "resource"
    Text string `json:"text,omitempty"`
    // image / resource 字段...
}

type Resource struct {
    URI         string `json:"uri"`
    Name        string `json:"name"`
    Description string `json:"description"`
    MimeType    string `json:"mimeType"`
}
```

### 4.4 MCPRouter（工具聚合 + 暴露给 LLM）

```go
// internal/agent/mcp_router.go
type MCPRouter struct {
    mgr *MCPManager
}

// LLMToolSpecs 转成 OpenAI / Anthropic 的 function calling 协议格式：
// 每个 MCP tool 用 "<server>.<tool>" 命名空间避免冲突。
func (r *MCPRouter) LLMToolSpecs(enabled []string) []OpenAITool {
    // 例：
    //   {
    //     name: "codegraph.search_definition",
    //     description: "...",
    //     parameters: { type: "object", properties: {...} }
    //   }
}

// Dispatch 解析 LLM 的 tool_call，路由到对应 MCPClient.
func (r *MCPRouter) Dispatch(ctx context.Context, toolName string, args map[string]any) (string, error)
```

### 4.5 ChatStreamWithTools 多轮 ReAct 循环（核心难点）

LLM tool_calls 不是一次 chat 完事 —— 是**循环**：

1. awy_service 发请求（含 system + tools spec + 历史 messages）
2. LLM 流式返回 → 可能是 text，也可能是 tool_call
3. 如果是 text：流式推给前端 SSE delta
4. 如果是 tool_call：
   - 暂停推 delta
   - awy_service 执行 `MCPRouter.Dispatch`
   - 把 tool_result 拼成新一轮请求（含历史 messages）
   - 回到第 1 步继续
5. 直到 LLM 返回 `finish_reason=stop`

```go
// internal/agent/service.go
//
// LLM tool_calls 不是一次 chat 完事 —— 是循环：
//   1. 发请求（含 system + tools spec + 历史 messages）
//   2. LLM 流式返回 text 或 tool_call
//   3. text 直接推给前端 SSE delta
//   4. tool_call → MCPRouter.Dispatch → 结果拼回 messages → 回到 1
//   5. 直到 finish_reason=stop（或达到最大轮次）
//
// 这个循环必须在后端跑，前端只看到流式增量 + 中间 tool_call 状态
// （比如显示"正在执行：codegraph.search_definition..."）。
func (s *Service) ChatStreamWithTools(ctx context.Context, req ChatRequest, out chan<- LLMStreamEvent) {
    defer close(out)

    messages := []ChatMessage{
        {Role: "system", Content: buildSystemPromptWithSkills(req.Mode, req.EnabledSkills)},
        {Role: "user",   Content: req.Message},
    }
    tools := s.mcpRouter.LLMToolSpecs(req.EnabledMCPTools)

    const maxRounds = 8 // 防死循环
    for round := 0; round < maxRounds; round++ {
        // (1) 调 LLM，流式拿 text + tool_calls
        textBuf, toolCalls, finishReason, err := s.llm.StreamChat(ctx, ep, messages, tools, perDeltaCallback(out))
        if err != nil {
            out <- LLMStreamEvent{Type: LLMEventError, Err: err}
            return
        }

        // (2) 没有 tool_call → 推完 text 就结束
        if len(toolCalls) == 0 || finishReason == "stop" {
            out <- LLMStreamEvent{Type: LLMEventDone, Reason: finishReason}
            return
        }

        // (3) 有 tool_call：推一个"工具调用进度"事件给前端
        for _, tc := range toolCalls {
            out <- LLMStreamEvent{
                Type:     LLMEventToolStart,
                ToolName: tc.Name,
                ToolArgs: tc.Args,
            }

            // 真正执行 MCP tool
            execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
            result, err := s.mcpRouter.Dispatch(execCtx, tc.Name, tc.Args)
            cancel()

            if err != nil {
                result = fmt.Sprintf("[tool error] %s", err.Error())
            }

            out <- LLMStreamEvent{
                Type:       LLMEventToolEnd,
                ToolName:   tc.Name,
                ToolResult: result,
            }

            // (4) 把 tool_result 拼回 messages，准备下一轮
            messages = append(messages, ChatMessage{
                Role:      "assistant",
                ToolCalls: []ToolCall{{Id: tc.Id, Name: tc.Name, Args: tc.Args}},
            })
            messages = append(messages, ChatMessage{
                Role:       "tool",
                ToolCallId: tc.Id,
                Content:    result,
            })
        }
        // 继续下一轮（LLM 看到 tool_result 后会决定继续调还是给最终答复）
    }
    // 超过 8 轮还没结束 → 强制收尾
    out <- LLMStreamEvent{Type: LLMEventError, Err: errors.New("超出最大工具调用轮次（8）")}
}
```

新事件类型：

```go
const (
    LLMEventTextDelta LLMEventType = iota
    LLMEventDone
    LLMEventError
    LLMEventToolStart  // 新：通知前端"开始执行 tool xxx"
    LLMEventToolEnd    // 新：通知前端"tool xxx 完成，结果 yyy"
)
```

---

### 4.6 SkillLoader（CC Switch SKILL 转 MCP）

让 SKILL 也走 MCP 协议（统一 abstraction）：

```go
// internal/agent/skill_loader.go
//
// CC Switch SKILL 不是天然的 MCP server，但可以用以下两种方式包装：
//
// (a) 把 SKILL.md 注入 system prompt（L1 路径，简单）
// (b) 把 SKILL/scripts/*.{py,cjs,sh} 包装成 MCP tools（L3 路径，强）
//
// 这里实现 (b)：把每个 SKILL 用一个内置 "skill_runner" MCP server 包起来：
//   awy_service 内置 MCP server，暴露：
//     - list_skills() → 列出所有 SKILL
//     - get_skill_doc(name) → 拿 SKILL.md 内容
//     - run_skill_script(skill, script_name, args) → 跑 scripts/xxx.{py,cjs}

type Skill struct {
    Id          string
    Name        string
    Directory   string
    SkillMD     string            // 解析后的 SKILL.md 内容（去掉 YAML frontmatter）
    Frontmatter SkillFrontmatter  // name + description
    Scripts     []SkillScript     // scripts/ 下找到的可执行
    EnabledFor  map[string]bool   // codex / claude / gemini ...
}

type SkillScript struct {
    Path string // 绝对路径
    Name string // 文件名（去后缀）
    Lang string // python / node / shell / cmd
}

// LoadAllSkills 扫描 ~/.cc-switch/skills/ + 读 cc-switch.db.skills 表的 enabled 状态。
func LoadAllSkills(homeDir string, db *sql.DB) ([]Skill, error)

// AsMCPTools 把 SKILL 转成 MCP tool 定义（每个 script 一个 tool）。
func (s *Skill) AsMCPTools() []Tool
```

---

### 4.7 配置入口 + 新 API

#### awy.api 新增

```api
// /api/agent/skills/list       GET  → 当前可用 skill 列表 + enabled 状态
// /api/agent/skills/toggle     POST → 启用/禁用某个 skill
// /api/agent/mcp/list          GET  → 当前 MCP server 状态
// /api/agent/mcp/restart       POST → 重启某个 MCP server

type AgentSkill {
    Id          string         `json:"id"`
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Source      string         `json:"source"`     // "skill" / "mcp"
    Enabled     bool           `json:"enabled"`
    Tools       []AgentMCPTool `json:"tools,omitempty"`
}

type AgentMCPTool {
    Name        string `json:"name"`        // "codegraph.search_definition"
    Description string `json:"description"`
    Schema      string `json:"schema,omitempty"`  // JSON Schema 字符串
}

type AgentSkillToggleReq {
    Id      string `json:"id"`
    Enabled bool   `json:"enabled"`
}
```

#### ChatRequest 扩展

```api
type AgentChatReq {
    Agent    string   `json:"agent"`
    Message  string   `json:"message"`
    Mode     string   `json:"mode,optional"`
    Provider string   `json:"provider,optional"`
    Effort   string   `json:"effort,optional"`

    // 新增：用户在前端选中要启用的 skills / mcp tools
    Skills    []string `json:"skills,optional"`     // skill id 列表
    MCPTools  []string `json:"mcpTools,optional"`   // "<server>.<tool>" 形式
}
```

---

### 4.8 前端 UX（SkillsPanel）

工具条加一个图标按钮 → 点开抽屉：

```
┌─────────────────────────────────────────┐
│ Skills 管理                          [X] │
├─────────────────────────────────────────┤
│  CC Switch SKILLS                       │
│  ☑ brainstorming                        │
│      在写代码前帮你做需求分析            │
│      [ tools: 0 / 仅 prompt 模式 ]      │
│  ☑ find-skills                          │
│  ☐ frontend-skill                       │
│  ...                                    │
│                                         │
│  MCP Servers                            │
│  ☑ codegraph        🟢 已连接 12 tools   │
│      ☑ search_definition                │
│      ☑ get_callers                      │
│      ☐ rebuild_index                    │
│      ...                                │
│  ☐ node_repl        🟡 已禁用            │
│  ☐ filesystem       ❌ 未配置（去 CC Switch 加） │
└─────────────────────────────────────────┘
```

启用状态持久化到 `localStorage` `awy.agent.skills.enabled`，每次发消息时带到后端。

---

## 五、实施分阶段（避免一次做太大）

> R1：L3-A 改为「`config_source.go` 读 `~/.claude.json` + 官方 SDK 起 session」，不再手写 MCPClient。

| 阶段     | 内容                                                 | 工作量 | 增量收益                     |
| -------- | ---------------------------------------------------- | ------ | ---------------------------- |
| **L3-A** | config_source（读 ~/.claude.json）+ 官方 SDK 管 session（list/call） | 1 天   | 可调通 codegraph + node_repl |
| **L3-B** | LLM 客户端加 tool_calls 协议 + ChatStream 多轮循环   | 1 天   | LLM 能真正调用 MCP tool      |
| **L3-C** | SkillLoader（SKILL → 内置 MCP server）               | 0.5 天 | SKILL 全功能可用             |
| **L3-D** | 前端 SkillsPanel + 启用状态管理                      | 0.5 天 | 完整用户 UX                  |
| **L3-E** | 健康检查 + 自动重启 + 日志 + 单测                    | 0.5 天 | 生产可用                     |

**总计：~3.5 天**

---

## 六、风险与边界

### 6.1 安全风险（重要）

MCP tool 能跑任意命令（filesystem 写文件 / node_repl 执行 JS / shell 命令）—— **必须**：

- 启动时弹出"危险操作"警告（与 workspace 模式同级）
- 每个 tool 调用记日志（who / when / args / result）
- 提供"工具白名单"yaml：

```yaml
Agent:
  MCPAllowedTools:
    - codegraph.*       # 全部 codegraph tool
    - filesystem.read_* # 仅读类
  MCPDeniedTools:
    - filesystem.delete_*
    - shell.exec
```

### 6.2 性能影响

- LLM 多轮循环 → 总耗时 = N × LLM 单轮（N 通常 1~3）
- 每个 tool_call 加 LLM 一次往返（~1s）+ tool 执行（~毫秒~秒）
- "你好" 这种闲聊用不到 tool → 速度同 P9（火山 1.5s）
- 复杂任务（搜代码 + 改文件）→ 5~30s

### 6.3 流式体验

LLM 决定 tool_call 时会**先停止 text 流**，跑完工具再继续。前端需要：

- 显示 "🔧 正在执行：codegraph.search_definition (param=...)"
- 工具完成后再恢复打字
- 用户随时可点「停止」取消整个循环

---

## 七、与 P9 / B 套餐的兼容关系

L3 不是替换，**而是叠加**：

```
当前已有：
  ✅ P9 直连 LLM API（火山 / OpenAI / Anthropic）
  ✅ B 套餐：HTTP/2 + 连接池 + 启动预热 + 前端打字预热

L3 新增层（R1）：
  + config_source（读 ~/.claude.json mcpServers + fsnotify）
  + MCPManager（用官方 SDK session 管 MCP server）
  + 官方 modelcontextprotocol/go-sdk（取代手写 JSON-RPC）
  + LLM tool_calls 协议（OpenAI 兼容/Anthropic/Gemini）
  + 多轮 ReAct 循环
  + Skills/MCP 启用管理 UI

不变的：
  ✅ ChatRequest 同步路径（/api/agent/chat）
  ✅ ChatStream 流式路径（/api/agent/chat-stream）
  ✅ CC Switch provider 切换
  ✅ 火山 / 豆包 / 米库 等 provider 继续用
```

**对用户的影响**：

- 如果用户**不启用任何 skill / MCP tool** → 行为完全和现在一样（火山 1.5s 流式）
- 启用一个或多个 skill → tools spec 加入 LLM 请求；LLM 自主决定是否调用
- 复杂任务（"帮我搜一下 awy.api 里所有 GET 接口"）会被 LLM 自动路由到 codegraph.search

---

## 八、与现有竞品的对照

| 特性                  | Claude Desktop          | Cursor    | Codex CLI    | **awy_service L3**         |
| --------------------- | ----------------------- | --------- | ------------ | -------------------------- |
| MCP server 支持       | ✅                       | ✅         | ⚠️（部分）   | ✅                          |
| 浏览器 UI             | ❌                       | ❌         | ❌            | ✅                          |
| 多 LLM provider       | ❌（仅 Anthropic）       | ⚠️        | ✅            | ✅✅（CC Switch 全支持）     |
| 流式 + 取消           | ✅                       | ✅         | ❌            | ✅                          |
| 自定义 SKILL          | ✅                       | ⚠️        | ✅            | ✅                          |
| 国产 LLM              | ❌                       | ⚠️        | ❌            | ✅✅（火山 / DeepSeek / 千问 / 豆包） |
| 部署难度              | 单机桌面                 | 单机桌面  | 单机 CLI     | **网页可远程访问**          |

L3 完成后 awy_service 在多 LLM + 网页 UI + 国产 provider 这三点上全面胜出。

---

## 九、决策矩阵

### 9.1 是否值得做 L3？

| 你的使用场景                                  | 推荐                  |
| --------------------------------------------- | --------------------- |
| 主要做闲聊 / 短问答                           | ❌ L1 就够了           |
| 写代码 / 改 bug，希望 LLM 能搜代码             | ✅✅ 必须 L3            |
| 偶尔需要让 LLM 调用 git / 文件操作            | ⚠️ L1 + 自己手动跑命令也行 |
| 长期想做"我自己的 ChatGPT 网页"产品           | ✅✅ L3 是基础设施       |

### 9.2 实施选项（4 选 1）

#### 选项 X：先 L1（1 小时）

- SKILL.md 注入 system prompt
- 前端复选框开关 skill
- 跑个一两周再说要不要 L3

#### 选项 Y：直接 L3 完整版（3.5 天）

- MCPManager + MCPClient + tool_calls 多轮循环 + UI
- 真正能跑 codegraph 搜代码、node_repl 执行 JS
- 投资换回长期使用

#### 选项 Z：L3 最小可用（1.5 天）

- 只支持你 CC Switch 已配置的 2 个 MCP server（codegraph / node_repl）
- 不做 SkillLoader（SKILL 走 L1 注入）
- 不做 SkillsPanel（前端固定开/关，单 checkbox）
- 后续按需扩展

#### 选项 W：暂时不做

- 当前 P9+B 套餐"火山豆包 1.5s 流式"已够日常用
- 等 awy_service 真有 SKILL/MCP 刚需时再做

---

## 十、文件清单（L3 完整版）

### 后端 awy_service（R1 修订后 ~11 个文件）

> R1：删 `mcp_protocol.go` / `mcp_client.go`（官方 SDK 取代）；新增 `config_source.go`。

```
internal/agent/
  config_source.go           新  ~120 行  读 ~/.claude.json mcpServers + fsnotify + 去重 + ImportFromCCSwitch
  mcp_manager.go             改  ~120 行  用官方 SDK session 起/停 server（不再手 os/exec + JSON-RPC）
  mcp_router.go              新  ~150 行  聚合 tools + LLMToolSpecs + Dispatch
  skill_loader.go            新  ~180 行  扫描 ~/.cc-switch/skills + 转 MCP tools
  skill_runner_server.go     新  ~150 行  内置 MCP server（SDK server 端）跑 SKILL scripts
  llm_client.go              改  +200 行  ToolCallingAdapter：OpenAI 兼容/Anthropic/Gemini（见 alternatives 第七节 + skeleton/）
  service.go                 改  +120 行  ChatStreamWithTools 多轮循环
  config.go                  改  +20 行   MCPAllowedTools / MCPDeniedTools / ClaudeConfigPath
  # 删除：mcp_protocol.go、mcp_client.go → github.com/modelcontextprotocol/go-sdk

internal/handler/agent/
  agent_chat_stream_handler.go  改  +50 行  SSE 增加 tool-start / tool-end 事件
  agent_skills_handler.go    新  ~80 行   GET/POST /api/agent/skills/*
  agent_mcp_handler.go       新  ~80 行   GET /api/agent/mcp/*

awy.api                       改  +60 行  AgentSkill / AgentMCPTool / Toggle 等类型
```

### 前端 awy_web（4 个文件）

```
playground/src/api/agent/
  index.ts                   改  +120 行  Skills/MCP 类型 + listSkills/toggleSkill API + 流式新事件

playground/src/views/agent/
  index.vue                  改  +80 行   工具条加 Skills/MCP 按钮 + tool 进度气泡
  SkillsPanel.vue            新  ~250 行  Skills + MCP servers 管理抽屉
  components/ToolProgressBubble.vue  新  ~80 行  消息流中"🔧 正在调用 xxx"气泡
```

---

## 十一、最终回答

### 问：使用本机的 skills 有什么方案？

| 方案    | 实现难度       | 收益                                          | 时间       |
| ------- | -------------- | --------------------------------------------- | ---------- |
| **L1**  | ⭐ 简单         | SKILL.md 注入 system prompt；工作流类 SKILL 可用（brainstorming） | 1 小时     |
| **L2**  | ⭐⭐⭐ 中        | function calling 调脚本；工具型 SKILL 也能用（xlsx/anysearch）    | 3~4 小时   |
| **L3**  | ⭐⭐⭐⭐⭐ 难      | 完整 MCP 集成；媲美 Claude Desktop / Cursor                       | 3.5 天     |

### 推荐路径

- 自用 / 学习 → **X（先 L1）**：最快见效，跑通后再加码
- 产品 / 跟 Cursor 对标 → **Z（L3 最小）**：先支持 codegraph + node_repl，后续扩展
- 生态吃透 → **Y（L3 完整版）**：一次到位

---

## 附录 A：依赖与外部工具（R1 修订）

- Go 标准库：`os/exec`、`encoding/json`、`net/http`
- **新增** `github.com/modelcontextprotocol/go-sdk`（官方，与 Google 合作维护）：MCP client/server、stdio + Streamable HTTP、版本协商/cancel/重连
- **新增** `github.com/fsnotify/fsnotify`：监听 `~/.claude.json` 变更
- `modernc.org/sqlite`：**仅在保留「从 cc-switch.db 一次性导入」兜底时**才需要；默认连接层不依赖
- ~~「无新依赖、MCP JSON-RPC 自己实现」~~：R1 撤销——对仍在演进的协议，手写是省小钱花大钱

## 附录 B：与 codex CLI 调 SKILL 的差异

| 维度       | codex CLI 调 SKILL              | awy_service L3 调 SKILL                |
| ---------- | -------------------------------- | --------------------------------------- |
| 启动开销    | 每次都 spawn 一个 codex 子进程   | LLM HTTP 直连，零启动                    |
| 流式        | 部分支持，有 buffering bug       | 原生 SSE，逐 token                      |
| 取消        | SIGTERM 杀子进程，~500ms         | `ctrl.abort()` 立即                     |
| 并发        | chatMu 串行                      | 完全并发                                |
| 多 provider | 受限（只有 Anthropic + 火山等）  | 全 CC Switch provider                   |
| 工具范围    | codex 内置 + 当前 SKILL          | CC Switch 全 MCP server + 全 SKILL      |

## 附录 C：实施前的准备清单

1. ☐ 确认要做的层级（X / Y / Z / W）
2. ☐ CC Switch GUI 把要用的 MCP server / SKILL 都启用
3. ☐ 阅读本设计文档
4. ☐ 阅读官方 [MCP Spec 2024-11-05](https://spec.modelcontextprotocol.io/)（仅 Y/Z 选项）
5. ☐ 切到 ACT MODE，按 5.1 ~ 5.5 阶段顺序实施

