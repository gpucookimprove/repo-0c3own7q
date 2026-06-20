package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Prompt 式工具调用降级。
//
// 当 provider 不支持原生 function-calling（部分国产模型 FC 弱/只非流式支持）时，
// 把工具目录塞进 system prompt，要求模型在需要调用工具时只输出一个 JSON：
//
//	{"tool":"<server>.<tool>","args":{...}}
//
// 然后从模型的文本输出里解析出该 JSON。这对应设计文档的 L1/L2 路径。
// 原生 FC 优先，弱模型自动降级到这里。

// BuildToolPrompt 生成描述工具目录的 system 段落。
func BuildToolPrompt(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("你可以调用以下工具。需要调用时，只输出一个 JSON 对象，")
	b.WriteString("格式为 {\"tool\":\"工具名\",\"args\":{参数}}，不要输出多余文字。\n")
	b.WriteString("不需要调用工具时，正常回答。可用工具：\n")
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema)
		b.WriteString(fmt.Sprintf("- %s: %s\n  参数 schema: %s\n", t.Name, t.Description, string(schema)))
	}
	return b.String()
}

// 匹配文本里的第一个 JSON 对象（贪婪到最后一个 '}'）。
var jsonObjRe = regexp.MustCompile(`(?s)\{.*\}`)

type promptToolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// ParsePromptToolCall 从模型文本里尝试解析一次 prompt 式工具调用。
// 第二个返回值表示是否解析到工具调用；false 时按普通文本回复处理。
func ParsePromptToolCall(text string) (ToolCall, bool) {
	m := jsonObjRe.FindString(text)
	if m == "" {
		return ToolCall{}, false
	}
	var p promptToolCall
	if err := json.Unmarshal([]byte(m), &p); err != nil || p.Tool == "" {
		return ToolCall{}, false
	}
	return ToolCall{ID: p.Tool, Name: p.Tool, Args: p.Args}, true
}
