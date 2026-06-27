# awy-agent-skeleton

`agent-skill-mcp-l3-design.md` 的**可落地 Go 代码骨架**，两个包：

- `agent/`：**轴2**——多 LLM provider 的 function-calling 适配层（文档第七节 / `ccswitch-integration-alternatives.md`）。
- `mcpconn/`：**轴1 + R1 连接层**——读 `~/.claude.json` 的发现层 + 基于官方 `go-sdk` 的连接/进程层。

> 独立可编译 module。把 `agent/` + `mcpconn/` 下的文件搬进 awy_service 的 `internal/agent/`
> （包名改成 awy 约定）、轴2 接上真实 HTTP transport 即可。

## 设计：两条轴解耦

```
轴1 MCP server（工具来源）         轴2 LLM provider（谁来调）
  └─► []Tool（provider 无关）  ──►  ToolCallingAdapter（每协议族一个）
```

M 个 server 共享一份 `[]Tool`；N 个 provider 只是 N 个适配器。**不是 M×N。**

## 文件

| 文件 | 职责 |
|------|------|
| `toolcall.go` | 共享类型（`Tool` / `ToolCall` / `ChatMessage`）、`ToolCallingAdapter` 接口、`NameMapper`（工具名命名空间双向映射，解决 OpenAI 不允许点号） |
| `schema.go` | 各 provider 的 JSON Schema 清洗：`SanitizeForOpenAI` / `SanitizeForGemini` / `SanitizeForAnthropic`（深拷贝，绝不改原 schema） |
| `adapter_openai.go` | OpenAI 兼容适配器（覆盖 OpenAI/Codex + 火山/DeepSeek/千问/豆包/Kimi/智谱）：tools 编码、`tool_calls` delta 流式拼接、结果回填 |
| `adapter_anthropic.go` | Claude 适配器：`input_schema`、`tool_use`/`tool_result` content block、`content_block_*` 流式事件 |
| `adapter_gemini.go` | Gemini 适配器：`functionDeclarations` / `functionCall` / `functionResponse` |
| `registry.go` | provider → 协议族 → adapter 的映射（`ResolveProfile` / `AdapterFor`），内置常见国产 provider 默认档案 |
| `fallback.go` | 弱模型 prompt 式工具调用降级（对应文档 L1/L2） |
| `skill_loader.go` | **L1 skill 注入**：扫描 `~/.cc-switch/skills/<id>/SKILL.md`，解析 frontmatter + 正文（`LoadAllSkills`），按 id 过滤启用集（`FilterEnabled`），把启用 skill 正文拼成 system prompt（`BuildSystemPromptWithSkills`）。无 YAML 依赖 |
| `example_loop.go` | 把 adapter 接进 5.5 多轮 ReAct 循环的接缝示例（`RunReActLoop`）；起点用 `BuildSystemPromptWithSkills` 把启用 skill 注入为首条 system 消息 |
| `adapter_test.go` | 三家流式解析 + 命名映射 + schema 清洗 + provider 路由的单测 |
| `skill_loader_test.go` | skill 加载/frontmatter 解析/过滤/system 注入（含 `RunReActLoop` 注入校验）的单测 |

### `mcpconn/`（轴1 + R1 连接层）

| 文件 | 职责 |
|------|------|
| `config_source.go` | 发现层：读 `~/.claude.json` 的 `mcpServers` → `[]MCPServerSpec`，按 command+args（http 按 url）去重；`ConfigSource.Watch` 用 `fsnotify` 监听热更新 |
| `mcp_manager.go` | 协议+进程层：对官方 `modelcontextprotocol/go-sdk` 的薄封装——`Start/Stop/Restart/Sync/ListTools/CallTool`，stdio 走 `CommandTransport`、http 走 `StreamableClientTransport`；ListTools 加 `<server>.` 命名空间（与轴2 `NameMapper` 对接） |
| `config_source_test.go` | 解析/归一/去重 + 缺文件返回空 + `fsnotify` 热更新回调的单测 |
| `mcp_manager_test.go` | 用 SDK 的 **in-memory transport + 内存 server**（带 echo 工具）跑通 ListTools/CallTool/Stop，不起真实子进程 |

## 处理的几个坑（见文档第七节）

1. **工具名不能带点**：`codegraph.search_definition` 对 OpenAI/Gemini 非法 →
   `NameMapper` 转成 `codegraph__search_definition` 并可还原。
2. **JSON Schema 方言**：OpenAI strict 要 `additionalProperties:false`+全 required；
   Gemini 剥掉 `$ref`/`$schema`/组合关键字/不支持的 format。
3. **能力探测 + 降级**：`ProviderProfile.SupportsTools=false` → 走 `fallback.go`。
4. **并行 tool_call**：循环里 `for _, tc := range res.ToolCalls` 兼容一轮多个。

## 跑测试

```bash
cd skeleton
go test ./...
```

## 接入 awy_service 时要补的

- `LLMTransport`：真实的 HTTP 请求 + SSE reader（实现 `RawStream`）。各 provider
  的 endpoint/鉴权用 CC Switch 当前 provider 的配置。
- Gemini 的 `systemInstruction` 字段（骨架里暂并进首条 user）。
- 真实 provider 档案：可用 CC Switch 实际配置覆盖 `builtinProfiles`。
