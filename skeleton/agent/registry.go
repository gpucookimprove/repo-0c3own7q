package agent

import "strings"

// ProtocolFamily 是 provider 的协议族。CC Switch 已知每个 provider 属于哪一族，
// awy_service 读当前 provider → 映射到 family → 选 adapter。
type ProtocolFamily string

const (
	FamilyOpenAI    ProtocolFamily = "openai"
	FamilyAnthropic ProtocolFamily = "anthropic"
	FamilyGemini    ProtocolFamily = "gemini"
)

// ProviderProfile 描述一个 CC Switch provider 的工具调用能力。
type ProviderProfile struct {
	Provider      string         // CC Switch provider id，如 "volcengine" / "deepseek" / "claude"
	Family        ProtocolFamily //
	SupportsTools bool           // 是否支持原生 function-calling；false → 走 prompt 降级
	Strict        bool           // OpenAI 兼容端点是否启用 strict schema
}

// builtinProfiles 是常见 provider → 协议族的默认映射。
// 绝大多数国产模型走 OpenAI 兼容端点。可被 CC Switch 实际配置覆盖。
var builtinProfiles = map[string]ProviderProfile{
	"openai":     {Family: FamilyOpenAI, SupportsTools: true, Strict: true},
	"codex":      {Family: FamilyOpenAI, SupportsTools: true, Strict: true},
	"azure":      {Family: FamilyOpenAI, SupportsTools: true, Strict: true},
	"volcengine": {Family: FamilyOpenAI, SupportsTools: true}, // 火山方舟
	"doubao":     {Family: FamilyOpenAI, SupportsTools: true}, // 豆包
	"deepseek":   {Family: FamilyOpenAI, SupportsTools: true},
	"qwen":       {Family: FamilyOpenAI, SupportsTools: true}, // 千问
	"dashscope":  {Family: FamilyOpenAI, SupportsTools: true},
	"moonshot":   {Family: FamilyOpenAI, SupportsTools: true}, // Kimi
	"zhipu":      {Family: FamilyOpenAI, SupportsTools: true}, // 智谱 GLM
	"glm":        {Family: FamilyOpenAI, SupportsTools: true},
	"claude":     {Family: FamilyAnthropic, SupportsTools: true},
	"anthropic":  {Family: FamilyAnthropic, SupportsTools: true},
	"gemini":     {Family: FamilyGemini, SupportsTools: true},
	"google":     {Family: FamilyGemini, SupportsTools: true},
}

// ResolveProfile 根据 provider id（大小写/别名不敏感）返回能力档案，
// 找不到时默认按 OpenAI 兼容、不开 strict 处理。
func ResolveProfile(provider string) ProviderProfile {
	key := strings.ToLower(strings.TrimSpace(provider))
	if p, ok := builtinProfiles[key]; ok {
		p.Provider = provider
		return p
	}
	for name, p := range builtinProfiles {
		if strings.Contains(key, name) {
			p.Provider = provider
			return p
		}
	}
	return ProviderProfile{Provider: provider, Family: FamilyOpenAI, SupportsTools: true}
}

// AdapterFor 按 provider 选择并构造对应 adapter，共享同一个 NameMapper
// （保证安全名 → 原名 的映射在编码工具和解析回调间一致）。
func AdapterFor(provider string, names *NameMapper) ToolCallingAdapter {
	if names == nil {
		names = NewNameMapper()
	}
	p := ResolveProfile(provider)
	switch p.Family {
	case FamilyAnthropic:
		return NewAnthropicAdapter(names)
	case FamilyGemini:
		return NewGeminiAdapter(names)
	default:
		return NewOpenAIAdapter(names, p.Strict)
	}
}
