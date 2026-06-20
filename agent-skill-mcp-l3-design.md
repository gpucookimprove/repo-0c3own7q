# awy_service Agent Skills / MCP（L3）完整设计方案

> 目标：让 awy_service 同时充当 **MCP Client（消费 CC Switch 注册的 MCP server）**
> 和 **MCP Host（把 LLM 包装成支持 MCP tool/resource/prompt 的 agent runtime）**，
> 实现和 Claude Desktop / Cursor 同等级的工具能力，并保留 P9 / B 套餐的速度优势。

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

CC Switch 已在 `mcp_servers` 表里管理了你机器上的 MCP server 配置，
本机当前已注册的（探测自 `~/.cc-switch/cc-switch.db`）：

```
codegraph    → stdio: codegraph serve --mcp
node_repl    → stdio: node_repl.exe（带 env）
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

```
awy_service/
  internal/agent/
    mcp_protocol.go          （新）MCP types：Tool/Resource/Prompt/InitializeResult
    mcp_client.go            （新）单个 MCP server 的 JSON-RPC 客户端
    mcp_manager.go           （新）启停 MCP server 进程，管理生命周期
    mcp_router.go            （新）把启用的 MCP tools 聚合 → 暴露给 LLM
    skill_loader.go          （新）扫描 ~/.cc-switch/skills，转成 prompts/tools
    skill_runner_server.go   （新）内置 MCP server 跑 SKILL scripts

    llm_client.go            （改）支持 OpenAI/Anthropic 的 tool_calls 协议
    service.go               （改）ChatStream 改为多轮 ReAct 循环
    config.go                （改）+MCPAllowedTools / MCPDeniedTools

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

### 4.1 MCPManager（进程管理）

```go
// internal/agent/mcp_manager.go
type MCPServerSpec struct {
    Id      string            // 来自 CC Switch.mcp_servers.id
    Name    string
    Command string            // 例如 "codegraph"
    Args    []string          // 例如 ["serve", "--mcp"]
    Env     map[string]string
    Type    string            // "stdio" / "sse"
    Enabled bool              // CC Switch.enabled_codex / enabled_claude
}

type MCPServerProcess struct {
    Spec    MCPServerSpec
    Cmd     *exec.Cmd
    Client  *MCPClient        // 见 4.2
    Healthy atomic.Bool
}

type MCPManager struct {
    mu       sync.RWMutex
    servers  map[string]*MCPServerProcess
    workDir  string
}

// 关键方法
func (m *MCPManager) Start(spec MCPServerSpec) error    // spawn + initialize
func (m *MCPManager) Stop(id string)
func (m *MCPManager) Restart(id string) error           // 健康检查失败时
func (m *MCPManager) ListTools() []MCPTool              // 跨所有 server 聚合
func (m *MCPManager) CallTool(name string, args map[string]any) (any, error)
func (m *MCPManager) HealthCheck()                       // 后台 goroutine
```

启停策略：

- awy_service 启动时从 CC Switch db 读 `mcp_servers`，spawn 状态为 enabled 的
- 每个 server 跑 stdio 子进程（Linux/Mac）或 conhost（Windows）
- 进程崩溃 → 5s 后重启（指数退避，最多 3 次）
- 主进程退出时优雅关闭所有子进程（SIGTERM → 2s grace → SIGKILL）

### 4.2 MCPClient（JSON-RPC 协议）

实现 [MCP Protocol 2024-11-05 spec](https://spec.modelcontextprotocol.io/) 子集：

- `initialize` / `initialized`
- `tools/list` / `tools/call`
- `resources/list` / `resources/read`
- `prompts/list` / `prompts/get`
- 通知（capabilities / log / progress）

```go
// internal/agent/mcp_client.go
//
// JSON-RPC 2.0 over stdio：每行一条 JSON 消息（newline-delimited，
// 不是 LSP 的 Content-Length）。
type MCPClient struct {
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    pending map[uint64]chan *jsonrpcResp  // request id → response channel
    seq     atomic.Uint64
    mu      sync.Mutex
}

func (c *MCPClient) Initialize(ctx context.Context) (*InitializeResult, error)
func (c *MCPClient) ListTools(ctx context.Context) ([]Tool, error)
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error)
func (c *MCPClient) ListResources(ctx context.Context) ([]Resource, error)
func (c *MCPClient) ReadResource(ctx context.Context, uri string) (*ResourceContents, error)
```

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

| 阶段     | 内容                                                 | 工作量 | 增量收益                     |
| -------- | ---------------------------------------------------- | ------ | ---------------------------- |
| **L3-A** | 仅实现 MCPManager + MCPClient（能 spawn/list/call）  | 1 天   | 可调通 codegraph + node_repl |
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

L3 新增层：
  + MCPManager（spawn / 监控 MCP server 进程）
  + MCPClient（JSON-RPC over stdio）
  + LLM tool_calls 协议
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

### 后端 awy_service（13 个文件）

```
internal/agent/
  mcp_protocol.go            新  ~120 行  MCP types：Tool/Resource/Prompt/InitializeResult
  mcp_client.go              新  ~250 行  JSON-RPC over stdio + initialize/list_tools/call_tool
  mcp_manager.go             新  ~200 行  spawn / restart / health-check 多个 server
  mcp_router.go              新  ~150 行  聚合 tools + LLMToolSpecs + Dispatch
  skill_loader.go            新  ~180 行  扫描 ~/.cc-switch/skills + 转 MCP tools
  skill_runner_server.go     新  ~150 行  内置 MCP server 跑 SKILL scripts
  llm_client.go              改  +180 行  支持 tool_calls 流式（OpenAI + Anthropic）
  service.go                 改  +120 行  ChatStreamWithTools 多轮循环
  config.go                  改  +20 行   MCPAllowedTools / MCPDeniedTools

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

## 附录 A：依赖与外部工具

- Go 标准库：`os/exec`、`bufio`、`encoding/json`、`net/http`
- 已有依赖：`modernc.org/sqlite`（读 cc-switch.db）
- **无新依赖**：MCP JSON-RPC 自己实现，比引入第三方 SDK 更精简

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

