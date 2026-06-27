# Skill 无法加入 AI 对话 —— 代码现状分析与改进方案

> 目标：回答「AI 对话功能里，skill 为什么没法加入到对话中」，定位代码断点，
> 给出可落地的改进清单。**本文档只做分析与方案，不改业务代码。**
>
> 关联设计：`agent-skill-mcp-l3-design.md`（L3 / R1 连接层定稿）、
> `ccswitch-integration-alternatives.md`（轴2 多 provider）。
> 关联实现：`skeleton/agent/`（轴2 适配层）+ `skeleton/mcpconn/`（轴1 连接层）。

---

## 一、结论（TL;DR）

**当前 skeleton 里根本没有「skill → 对话」的代码路径，所以 skill 一定加不进对话。**
不是某个函数有 bug，而是整条链路缺失：

1. 设计文档把 skill 列成两类落地文件（`skill_loader.go` / `skill_runner_server.go`），
   但 `skeleton/` 里**这两个文件都不存在**——全仓库除了注释/README，零处 `Skill` 代码
   （`grep -ri skill skeleton/` 只命中 README 和一句包注释）。
2. 多轮 ReAct 循环 `RunReActLoop`（`skeleton/agent/example_loop.go`）的入参里
   **没有任何 skill 维度**：只有 `tools []Tool`（来自 MCP server）和
   `history []ChatMessage`。skill 既进不了 `tools`，也进不了 `history`。
3. 设计文档 5.5 节用到的两个关键接缝函数——
   `buildSystemPromptWithSkills(req.Mode, req.EnabledSkills)`（L1：把 SKILL.md 注入
   system prompt）和 `Skill.AsMCPTools()`（L2/L3：把 SKILL 脚本包成工具）——
   **在 skeleton 中都没有实现**。
4. API 层设计了 `AgentChatReq.Skills []string` / `/api/agent/skills/*`
   （设计文档 4.7、十节），但 skeleton 的循环签名收不到这个字段，
   即使前端勾选了 skill，后端也无处接住。

也就是说：MCP server 的 tools 能进对话（轴1+轴2 已打通），
**但 skill 这一路从加载、到转工具/转 prompt、到塞进请求，整段都还没接线。**

---

## 二、现状梳理：什么已经通了

| 能力 | 文件 | 状态 |
|------|------|------|
| 读 `~/.claude.json` 的 mcpServers + 去重 + fsnotify 热更新 | `skeleton/mcpconn/config_source.go` | ✅ 已实现 |
| 用官方 go-sdk 起停 MCP server、`ListTools`/`CallTool`、`Sync` | `skeleton/mcpconn/mcp_manager.go` | ✅ 已实现 |
| 工具名命名空间（`server.tool` ↔ `server__tool`） | `skeleton/agent/toolcall.go` (`NameMapper`) | ✅ 已实现 |
| OpenAI / Anthropic / Gemini 三家适配器 + schema 清洗 | `skeleton/agent/adapter_*.go`、`schema.go` | ✅ 已实现 |
| 弱模型 prompt 式工具降级 | `skeleton/agent/fallback.go` | ✅ 已实现 |
| 多轮 ReAct 循环（仅 MCP tools） | `skeleton/agent/example_loop.go` | ⚠️ 实现了，但不含 skill |

结论：**轴1（MCP 工具来源）和轴2（多 provider 调用）这两条轴都打通了**，
对话里能调 MCP 工具。缺的只有 **skill 这条支线**。

---

## 三、断点逐条定位（skill 在哪一步掉链子）

### 断点 1：没有 SkillLoader —— skill 根本没被加载

设计文档 4.6 / 三节 / 十节都列了：

```
internal/agent/
  skill_loader.go          新  ~180 行  扫描 ~/.cc-switch/skills + 转 MCP tools
  skill_runner_server.go   新  ~150 行  内置 MCP server（SDK server 端）跑 SKILL scripts
```

`skeleton/` 里这两个文件**不存在**。没有 `LoadAllSkills()`、没有 `Skill` 结构体、
没有 `AsMCPTools()`。skill 连「被读出来」这一步都没有。

### 断点 2：ReAct 循环签名没有 skill 维度

`skeleton/agent/example_loop.go:26-35`：

```go
func RunReActLoop(
    ctx context.Context,
    provider string,
    tools []Tool,            // ← 只有 MCP tools，没有 skills
    history []ChatMessage,   // ← system+user，没有 skill 注入
    maxRounds int,
    transport LLMTransport,
    dispatch MCPDispatcher,  // ← 只路由到 MCP server，不路由 skill 脚本
    onDelta func(StreamDelta),
) (StreamResult, error)
```

对比设计文档 5.5 的 `ChatStreamWithTools`：

```go
messages := []ChatMessage{
    {Role: "system", Content: buildSystemPromptWithSkills(req.Mode, req.EnabledSkills)}, // ← 未实现
    {Role: "user",   Content: req.Message},
}
tools := s.mcpRouter.LLMToolSpecs(req.EnabledMCPTools) // ← 只有 MCP，没并入 skill tools
```

`buildSystemPromptWithSkills` 在 skeleton 里没有任何实现。**L1 注入路径缺失。**

### 断点 3：skill tools 没有并进 `tools` 目录

即使将来实现了 `Skill.AsMCPTools()`，目前也没有任何地方把它的产物
`append` 进传给 `RunReActLoop` 的 `tools []Tool`。**L2/L3 工具路径缺失。**

### 断点 4：Dispatcher 不认 skill 工具

`MCPDispatcher`（`example_loop.go:18`）只按 `<server>.<tool>` 路由到
`mcpconn.Manager.CallTool`。skill 脚本（`run_skill_script(...)`）没有对应的
执行分支。即便 LLM 真的发起了 skill 工具调用，也会被
`mcp_manager.go:184-186` 的 `未连接的 server` 拒掉。

### 断点 5：API / 前端字段悬空

设计文档 4.7：`AgentChatReq.Skills []string`、`GET/POST /api/agent/skills/*`。
后端循环收不到 `Skills`，所以前端 SkillsPanel 勾的状态传到后端就**丢了**。

### 断点 6（隐患）：Gemini 的 system 注入会被降级

`skeleton/agent/adapter_gemini.go:75-80`：

```go
case "system":
    // Gemini 用单独的 systemInstruction 字段，调用方处理；这里并进 user。
    out = append(out, map[string]any{"role": "user", "parts": ...})
```

即使 L1 注入做了，对 Gemini 来说 skill 的 system 指令被并进了普通 user 消息，
没有走 `systemInstruction`。多轮循环里这条「伪 system」还会反复夹在历史中间，
**指令权重和位置都不对**，skill 行为会不稳定。

---

## 四、改进方案（落地清单，按优先级）

> 原则：先用最低成本把 skill「能进对话」打通（L1），再按需上工具化（L2/L3）。
> 与设计文档的分层一致：L1（system 注入）→ L2（脚本工具）→ L3（内置 MCP server）。

### P0 —— 让 skill 能进对话（L1，最快见效）

**1. 新增 `skill_loader.go`：加载 + 选择性输出。**

```go
type Skill struct {
    ID, Name, Description string
    Directory             string
    SkillMD               string          // 去掉 frontmatter 的正文
    Scripts               []SkillScript    // scripts/ 下可执行（L2 用）
    Enabled               bool
}

// L1：扫描 ~/.cc-switch/skills/*/SKILL.md，解析 frontmatter + 正文。
func LoadAllSkills(home string) ([]Skill, error)

// L1：把启用的 skill 正文拼成一段 system prompt。
func BuildSystemPromptWithSkills(mode string, enabled []Skill) string
```

**2. 给 ReAct 循环加 skill 入口**（改 `example_loop.go` 的 `RunReActLoop` 或在
设计的 `ChatStreamWithTools` 里）：在构造 `history` 时，把
`BuildSystemPromptWithSkills(...)` 作为/并入首条 `system` 消息。

```go
sys := BuildSystemPromptWithSkills(req.Mode, enabledSkills)
history := []ChatMessage{{Role: "system", Content: sys}, {Role: "user", Content: req.Message}}
```

> 仅此一步，brainstorming 这类「纯工作流型」skill 就能立刻进对话——
> 对应文档「选项 X：先 L1（1 小时）」。

**3. 打通 API：** 给 `AgentChatReq` 接住 `Skills []string`，加
`GET /api/agent/skills`（列表 + enabled）、`POST /api/agent/skills/toggle`。
前端勾选 → 带 id → 后端 `LoadAllSkills` 过滤出启用集 → 进 system。

### P1 —— skill 脚本工具化（L2）

**4. 实现 `Skill.AsMCPTools() []Tool`：** 每个 `scripts/xxx.{py,cjs,sh}` 映射成
一个 `Tool`（名字建议 `skill.<skill>.<script>`，沿用 `NameMapper` 命名空间）。

**5. 在循环外把 skill tools 并进 `tools`：**

```go
tools := mcpRouter.LLMToolSpecs(req.EnabledMCPTools)
for _, sk := range enabledSkills {
    tools = append(tools, sk.AsMCPTools()...)
}
```

**6. 扩展 Dispatcher 路由：** `MCPDispatcher` 增加 skill 分支——
名字前缀是 `skill.` 时执行对应脚本（带超时、工作目录、参数白名单），
否则走 `mcpconn.Manager.CallTool`。

### P2 —— skill 作为内置 MCP server（L3，最统一）

**7. `skill_runner_server.go`：** 用官方 go-sdk 的 **server 端** 起一个内置
`skill_runner` MCP server，暴露 `list_skills` / `get_skill_doc` /
`run_skill_script`，再把它当成一个普通 server 注册进 `mcpconn.Manager`。
这样 skill 和 MCP 工具就走**完全一致**的发现/调用链路，Dispatcher 不用特判。

### P3 —— 修隐患

**8. Gemini system 注入：** `EncodeMessages` 不要把 `system` 并进 user；
把 system 内容单独提出来，由调用方塞进请求体的 `systemInstruction` 字段
（设计文档「接入 awy_service 时要补的」已记此项）。

**9. 安全边界：** skill 脚本可执行任意命令，落地前补上设计文档 6.1 的
allow/deny 白名单 + 调用日志 + 危险操作提示，再开 L2/L3。

---

## 五、改动影响面（按文件）

| 文件 | 改动 | 层级 |
|------|------|------|
| `skill_loader.go` | 新增：加载 + `BuildSystemPromptWithSkills` + `AsMCPTools` | L1/L2 |
| `example_loop.go` / `service.go` | 改：循环入口接 skill（system 注入 + 并 tools） | L1/L2 |
| `mcp_router.go` / dispatcher | 改：Dispatch 识别 skill 工具 | L2 |
| `skill_runner_server.go` | 新增：内置 SDK server 暴露 skill | L3 |
| `adapter_gemini.go` | 改：system → `systemInstruction` | 隐患 |
| API（`agent_skills_handler.go` + `AgentChatReq`） | 新增/改：skill 列表 + toggle + 请求字段 | L1 |
| 前端 `SkillsPanel.vue` / `index.ts` | 改：勾选状态带到后端 | L1 |

---

## 六、最小验证路径

1. 放一个 `~/.cc-switch/skills/brainstorming/SKILL.md`。
2. 实现 `LoadAllSkills` + `BuildSystemPromptWithSkills`，单测：启用 1 个 skill →
   断言生成的 system 文本包含其正文。
3. 在 `RunReActLoop` 起点注入该 system，对一个内存 transport 跑一轮，
   断言请求体里带上了 skill 指令（即「skill 进了对话」）。
4. L2：给该 skill 加一个 `scripts/echo.py`，断言 `AsMCPTools()` 产出 1 个工具，
   且 Dispatcher 能路由执行。

---

## 七、一句话回答用户

skill 之所以加不进对话，是因为 **skeleton 里压根没写 skill 的加载/注入/路由代码**
（`skill_loader.go`、`buildSystemPromptWithSkills`、`AsMCPTools` 全缺，
ReAct 循环也没有 skill 入参）。只要先补 **P0 的 L1 注入**（加载 SKILL.md →
拼进 system prompt → API 带上启用集），skill 就能立即进对话；
工具型 skill 再按 P1/P2 逐步上。
