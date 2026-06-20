# awy_service ↔ CC Switch 集成方案评审：现方案 + 更优替代

> 针对 `agent-skill-mcp-l3-design.md` 中「本地连接 CC Switch」这一环的评审。
> L3 的多轮 ReAct 循环 / MCPRouter / tool_calls 协议本身没问题，本文只聚焦
> **awy_service 怎么发现 + 怎么连上 MCP server / SKILL** 这一层，给出更稳的做法。

---

## 一、现方案在做什么（"本地连接 CC Switch"）

设计文档里这一层做了两件事：

1. **发现配置**：awy_service 用 `modernc.org/sqlite` 直接读
   `~/.cc-switch/cc-switch.db` 的 `mcp_servers` / `skills` 表，拿到 server 列表
   和 `enabled_codex / enabled_claude` 启停状态。
2. **自己当 Host**：awy_service 内部实现 `MCPManager`（`os/exec` spawn 子进程）
   + 手写 `MCPClient`（stdio 上的 JSON-RPC，目标 spec 2024-11-05），自己起
   codegraph / node_repl 等进程并 `tools/list`、`tools/call`。

也就是 **直接耦合 CC Switch 的私有 SQLite schema**，并且 **awy_service 重新实现
一遍 MCP host**。

---

## 二、现方案的问题

| # | 问题 | 说明 |
|---|------|------|
| 1 | **schema 耦合、易碎** | `cc-switch.db` 是第三方桌面 app（farion1231/cc-switch，10w+★，Tauri，活跃迭代）的内部实现细节。表结构（`mcp_servers`、`apps.claude/codex` 列、`skills` 表）没有对外契约，任何一次升级都可能改字段 → awy_service 静默挂掉。 |
| 2 | **SQLite 文件锁 / WAL 争用** | CC Switch（Rust 进程）持有这个 db。awy_service 在它写入时并发读，容易撞 `database is locked`。只读 + `busy_timeout` 能缓解，但仍是隐患。 |
| 3 | **拿不到实时变更** | 启动时读一次快照；用户在 CC Switch GUI 里开关某个 server，不重启 awy_service 就不生效（除非额外做 poll / 文件监听）。 |
| 4 | **进程归属重复（最严重）** | CC Switch 的本职就是把 MCP 配置 **写进各 CLI 工具的 live 配置文件**，再由 CLI 工具去 spawn server。如果 awy_service **又自己 spawn 同一批 stdio server**，就会出现重复进程、重复状态（codegraph 索引锁、node_repl 会话）、http/sse server 端口冲突。 |
| 5 | **重复造 MCP client** | 手写 JSON-RPC + 生命周期 + 重连 + 协议版本协商 + capabilities + cancel + progress，是大量易错细节，而这些 SDK 已经实现并测试过。文档"无新依赖、自己实现"对一个还在演进的协议来说是省小钱花大钱。 |
| 6 | **协议版本过时** | 文档目标是 MCP `2024-11-05`。当前最新是 `2025-06-18`（中间 `2025-03-26` 引入 **Streamable HTTP** 取代旧的 HTTP+SSE 传输，并加了结构化 tool 输出、elicitation 等）。照旧 spec 写完即返工。 |

---

## 三、关键事实：CC Switch 已经把配置写成「标准格式」了

查 CC Switch 官方文档（`docs/user-manual/zh/3-extensions/3.1-mcp.md`）确认：
开关某个 app 时，CC Switch 会把启用的 MCP server **同步进各 CLI 工具的 live 配置**：

| 开关 | 写入的标准配置文件 | 字段 |
|------|------|------|
| Claude | `~/.claude.json` | `mcpServers` |
| Codex | `~/.codex/config.toml` | `[mcp_servers]` |
| Gemini | `~/.gemini/settings.json` | `mcpServers` |
| OpenCode | `~/.config/opencode/opencode.json` | `mcp` |
| Hermes | `~/.hermes/config.yaml` | `mcp_servers` |

支持传输类型：`stdio` / `http` / `sse`。关闭开关时会**从 live 配置里移除**该 server。

**结论**：`~/.claude.json` 的 `mcpServers`（和 Claude Desktop 完全同形、公开、稳定的
格式）已经就是「当前启用的 MCP server」的权威来源 —— 没必要去抠 CC Switch 的私有 db。

---

## 四、更优替代方案

### 方案 A —— 读「标准 CLI 配置文件」，而不是 CC Switch 私有 db ✅ 性价比最高

awy_service 改成消费 `~/.claude.json` 的 `mcpServers`（公开、文档化、与 Claude
Desktop 同形），而不是 `cc-switch.db`。

- 收益：schema 稳定；无锁争用；与 CC Switch 版本解耦；**启停状态天然已反映**
  （CC Switch 关掉的 server 已从该文件移除）。
- 用 `fsnotify` 监听该文件 → 实时热更新（解决问题 3）。
- 改动极小（把"读 db"换成"读 json"），却一次性消掉问题 1/2/3。
- 可保留读 `cc-switch.db` 仅作为「一次性导入 / 兜底」。

> 这是单点最高杠杆的改动。

### 方案 B —— 用官方/成熟 MCP Go SDK，别手写 client

- `modelcontextprotocol/go-sdk`（**官方**，与 Google 合作维护，~4.7k★）：client+server、
  stdio + **Streamable HTTP** 传输、session 管理、tool/resource/prompt、跟随最新 spec。
- 或 `mark3labs/mcp-go`（社区最流行，~8.8k★）。

直接删掉 `mcp_protocol.go` / `mcp_client.go` 和 `mcp_manager.go` 的大半（~600 行
易碎协议代码），白拿协议版本协商、Streamable HTTP、cancel、重连。解决问题 5/6。
代价是引入 1 个依赖 —— 但它就是协议参考实现。

### 方案 C —— 用单一 MCP 网关/聚合器，awy_service 不再管 N 个进程

跑一个 gateway（`TBXark/mcp-proxy` ~0.7k★ / `samanhappy/mcphub` ~2.2k★ /
`sparfenyuk/mcp-proxy` ~2.6k★），由它：

- 读一份 mcp.json，
- spawn / 持有所有 stdio server，
- 聚合成 **单个 Streamable HTTP/SSE 端点**对外（可带 per-tool 路由、鉴权、
  tool allow/deny、可观测性）。

awy_service 就退化成「**一个 HTTP 端点的瘦 MCP client**」——
没有 `os/exec`、没有子进程监管、`MCPManager` 的健康检查/重启代码几乎归零（解决问题 4）。
`mcphub` 还自带 Web UI + 分组 + 智能路由。

- 代价：多一个常驻组件要部署。单机自用可能偏重；多用户 / 远程 / server 很多时非常值。

### 方案 D —— awy_service 自持 mcp.json，彻底与 CC Switch 解耦

把 CC Switch 当成一个**导入器**（一次性「从 CC Switch / 从 ~/.claude.json 导入」），
之后 awy_service 自己是唯一事实源。归属最干净、运行时零依赖 CC Switch；
代价是失去"GUI 里一开关就自动同步"的便利。

### 传输选择

能用 **Streamable HTTP（2025 spec）** 的 server 优先用它；只有纯本地 server 才用
stdio。不要再投入旧的 SSE 传输或只盯 2024-11-05。

---

## 五、方案对比

| 维度 | 现方案<br>(读 cc-switch.db + 自起进程) | A 读 ~/.claude.json | B 官方 SDK | C 网关聚合 | D 自持 mcp.json |
|------|------|------|------|------|------|
| 与 CC Switch 耦合 | 私有 schema（强耦合） | 标准格式（弱耦合） | —（正交） | 弱 | 无 |
| 配置稳定性 | 差 | 好 | — | 好 | 最好 |
| 实时热更新 | 需自己做 | fsnotify 即可 | — | gateway 热重载 | 自管 |
| 进程重复风险 | 高 | 仍需自己起→中 | 仍需自己起→中 | 无 | 取决于实现 |
| awy_service 代码量 | 最大（自写协议+进程管理） | 中 | 小 | 最小 | 中 |
| 协议演进/Streamable HTTP | 要自己跟 | 要自己跟 | 自动 | 自动 | 取决于实现 |
| 部署复杂度 | 低 | 低 | 低 | 中（多一组件） | 低 |
| 远程/多用户扩展 | 差 | 差 | 中 | 好 | 中 |

---

## 六、推荐组合（对文档改动最小）

L3 的 **多轮 ReAct 循环 / MCPRouter / tool_calls** 全部保留，只换"连接层"：

1. **发现层**：`读 cc-switch.db` → **读 `~/.claude.json` 的 `mcpServers` + fsnotify 监听**
   （方案 A）。`cc-switch.db` 最多作为一次性导入/兜底。
2. **协议层**：手写 `mcp_client.go` / `mcp_protocol.go` → **官方 Go SDK client**（方案 B）。
3. **进程层（按规模二选一）**：
   - 单机自用：让 awy_service 用 SDK 直接管 stdio server（省掉 gateway）。
   - 要远程 / 多用户 / server 多：前面加 **gateway**，awy_service 只连一个端点（方案 C）。
4. **SKILL**：`SkillLoader` 思路保留，但通过同一套 SDK server 抽象暴露。

净效果：删掉文档里 `mcp_protocol.go`/`mcp_client.go` 和 `mcp_manager.go` 的大半，
连接更稳、协议自动跟进，且去掉了「awy_service 与 CC Switch / CLI 工具重复 spawn 同一
server」这个最大隐患。

---

## 七、有多个 provider（Claude / Codex / 国产模型）怎么处理

关键：把两条**互相独立的轴**分开，别耦在一起。

```
轴1 = MCP server（工具来源）        轴2 = LLM provider（谁来调）
codegraph / node_repl / fetch ...   Claude / Codex / 火山豆包 / DeepSeek / 千问 ...
        │                                   │
        └──────► MCPRouter（provider 无关）◄─┘
                 统一工具目录(JSON Schema) → 各 provider 适配
```

**不是 M×N。** M 个 server 连接是共享的，N 个 provider 只是 N 个小适配器，
中间 MCPRouter 是 provider 无关的。换模型不影响工具，加工具不影响模型。

### 7.1 轴1：多来源 MCP 配置（发现层）

CC Switch 会把**同一批** server 同时写进多个 CLI 的配置文件（claude/codex/gemini…），
格式不同但 server 定义是同一个。awy_service 不是其中任何一个 CLI 工具，所以：

- **选一个权威源**，别合并 5 个文件。推荐 `~/.claude.json` 的 `mcpServers`
  （JSON、最完整、与 Claude Desktop 同形）。
- 各格式（JSON `mcpServers` / TOML `[mcp_servers]`）写薄适配器 → 归一成内部
  `MCPServerSpec`，**按 name 或 command+args 去重**（同一 server 在多文件里只连一次）。
- awy_service 维护**自己的启用集**，与「CC Switch 给 Claude Code 开了哪些」解耦
  —— 「给 Claude Code 启用」≠「给 awy_service 启用」。这正好对应文档里的
  SkillsPanel：用户在 awy_service 自己的面板里勾选。
- 想要最干净：走**方案 D**（awy_service 自持 mcp.json + 「从 CC Switch 导入」）
  或**方案 C**（gateway 把多来源归一成一个端点）。

> 注意：gateway（方案 C）只解决轴1，对轴2 没用 —— provider 适配器仍然要在 awy_service 里。

### 7.2 轴2：多 LLM provider（function-calling 协议层）

MCP 给的是 provider 无关的工具目录，但每家 LLM 的 function-calling 线格式不同。
需要一个 **ToolCallingAdapter** 接口，按 provider 协议族实现：

| 协议族 | 覆盖的 provider | tools 格式 | 工具调用返回 | 结果回填 |
|------|------|------|------|------|
| **OpenAI 兼容** | OpenAI / Codex / **火山方舟 / DeepSeek / 千问 / 豆包 / Kimi / 智谱**（绝大多数国产都走 OpenAI 兼容端点） | `tools:[{type:function,function:{name,parameters}}]` | `tool_calls`（delta 带 index 流式拼接） | `role:"tool"` 消息 |
| **Anthropic** | Claude | `tools:[{name,input_schema}]` | assistant 的 `tool_use` content block | `tool_result` content block |
| **Gemini** | Gemini CLI | `functionDeclarations` | `functionCall` part | `functionResponse` part |

```go
type ToolCallingAdapter interface {
    EncodeTools(tools []mcp.Tool) any                         // 统一目录 → 本家 tools spec
    StreamChat(ctx, msgs, tools, onDelta) (text string, calls []ToolCall, finish string, err error)
    EncodeAssistantToolCall(tc ToolCall) ChatMessage          // 写回历史
    EncodeToolResult(tc ToolCall, result string) ChatMessage
}
// 实现：OpenAIAdapter（≈所有国产）/ AnthropicAdapter / GeminiAdapter
// CC Switch 已知每个 provider 的协议族 → provider → 选 adapter
```

文档里 `llm_client.go(改 +180 行 支持 OpenAI + Anthropic)` 就是这层，但要补成接口化、
并加上 Gemini 和下面几个**坑**：

1. **工具名不能带点**：OpenAI 工具名必须 `^[a-zA-Z0-9_-]+$`，文档的
   `codegraph.search_definition` 命名空间**对 OpenAI 非法**。改用 `codegraph__search_definition`
   并维护一张映射回 `<server>.<tool>`。（这是文档里的一个具体 bug。）
2. **JSON Schema 方言差异**：OpenAI strict 模式要 `additionalProperties:false` + 全
   required；Gemini 的 schema 子集更窄（不支持 `$ref`、format 有限）。要按 provider
   **清洗/降级** MCP tool 的 inputSchema。
3. **能力探测 + 降级**：部分国产模型 FC 弱或只在非流式支持。需要 per-provider
   `supports_tools` 标志；不支持的就回退**「prompt 式工具调用」**（让模型在文本里吐
   JSON action 再解析）—— 正好对应文档的 L1/L2。原生 FC 优先，弱模型自动降级。
4. **工具数量爆 context**：server 多 → tools spec 巨大，拖垮弱模型。靠 SkillsPanel
   「只发已启用的工具」+ 命名空间过滤；必要时上「按需 list 工具的 meta-tool」。
5. **并行 tool_call**：OpenAI/Anthropic 一轮可返回多个 tool_call，部分国产只返回一个。
   ReAct 循环两种都要兼容（文档第 5.5 节的 `for _, tc := range toolCalls` 已是对的方向）。

### 7.3 小结

- **工具来源（轴1）**：归一成一份去重列表（读 `~/.claude.json` / 自持 mcp.json / gateway），
  和 provider 无关，**不随 provider 增多**。
- **模型（轴2）**：3 个协议族适配器（OpenAI 兼容 ≈ 覆盖所有国产、Anthropic、Gemini）
  + 工具名清洗 + Schema 降级 + 弱模型 prompt 降级。
- CC Switch 在这里只负责**切 provider / 管 key**；awy_service 读当前 provider →
  选对应 adapter，工具层完全复用。

---

## 附：来源
- CC Switch：`github.com/farion1231/cc-switch`，官方文档 `docs/user-manual/zh/3-extensions/3.1-mcp.md`
- 官方 MCP Go SDK：`github.com/modelcontextprotocol/go-sdk`
- `github.com/mark3labs/mcp-go`
- 网关：`github.com/TBXark/mcp-proxy`、`github.com/samanhappy/mcphub`、`github.com/sparfenyuk/mcp-proxy`
- MCP spec：当前最新 `2025-06-18`（`2025-03-26` 引入 Streamable HTTP）
