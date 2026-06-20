package agent

import (
	"context"
	"io"
	"testing"
)

// memStream 是 RawStream 的内存实现，逐条吐出预置的 SSE data 行。
type memStream struct {
	lines [][]byte
	i     int
}

func (m *memStream) Next() ([]byte, error) {
	if m.i >= len(m.lines) {
		return nil, io.EOF
	}
	l := m.lines[m.i]
	m.i++
	return l, nil
}

func lines(ss ...string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

func TestNameMapperRoundTrip(t *testing.T) {
	m := NewNameMapper()
	safe := m.ToSafe("codegraph.search_definition")
	if safe != "codegraph__search_definition" {
		t.Fatalf("safe = %q", safe)
	}
	if !safeNameRe.MatchString(safe) {
		t.Fatalf("safe name not OpenAI-legal: %q", safe)
	}
	if raw := m.ToRaw(safe); raw != "codegraph.search_definition" {
		t.Fatalf("raw = %q", raw)
	}
	// 非法字符被替换
	if got := m.ToSafe("ns.tool with space"); !safeNameRe.MatchString(got) {
		t.Fatalf("illegal chars not sanitized: %q", got)
	}
}

func TestSanitizeForOpenAI(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
	}
	out := SanitizeForOpenAI(in)
	if out["additionalProperties"] != false {
		t.Fatalf("additionalProperties not false")
	}
	req, ok := out["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "q" {
		t.Fatalf("required = %v", out["required"])
	}
	// 不能改到原 schema
	if _, exists := in["additionalProperties"]; exists {
		t.Fatalf("input schema was mutated")
	}
}

func TestSanitizeForGeminiDropsUnsupported(t *testing.T) {
	in := map[string]any{
		"type":                 "object",
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"additionalProperties": false,
		"properties": map[string]any{
			"when": map[string]any{"type": "string", "format": "uri"},
		},
	}
	out := SanitizeForGemini(in)
	if _, ok := out["$schema"]; ok {
		t.Fatalf("$schema not dropped")
	}
	if _, ok := out["additionalProperties"]; ok {
		t.Fatalf("additionalProperties not dropped")
	}
	props := out["properties"].(map[string]any)
	when := props["when"].(map[string]any)
	if _, ok := when["format"]; ok {
		t.Fatalf("unsupported format 'uri' not dropped")
	}
}

func TestOpenAIParseStreamToolCall(t *testing.T) {
	names := NewNameMapper()
	a := NewOpenAIAdapter(names, true)
	// 让映射表知道安全名 → 原名
	if _, err := a.EncodeTools([]Tool{{Name: "codegraph.search_definition", InputSchema: map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	raw := &memStream{lines: lines(
		`{"choices":[{"delta":{"content":"let me search"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"codegraph__search_definition","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"foo\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)}
	var got string
	res, err := a.ParseStream(context.Background(), raw, func(d StreamDelta) { got += d.Text })
	if err != nil {
		t.Fatal(err)
	}
	if got != "let me search" || res.Text != "let me search" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("toolcalls = %d", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "codegraph.search_definition" {
		t.Fatalf("toolcall = %+v", tc)
	}
	if tc.Args["q"] != "foo" {
		t.Fatalf("args = %v", tc.Args)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", res.FinishReason)
	}
}

func TestAnthropicParseStreamToolCall(t *testing.T) {
	names := NewNameMapper()
	a := NewAnthropicAdapter(names)
	_, _ = a.EncodeTools([]Tool{{Name: "fs.read_file", InputSchema: map[string]any{}}})
	raw := &memStream{lines: lines(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"reading"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs__read_file"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":\"/tmp/x\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)}
	res, err := a.ParseStream(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "reading" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("toolcalls = %d", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "fs.read_file" || tc.Args["path"] != "/tmp/x" {
		t.Fatalf("toolcall = %+v", tc)
	}
}

func TestGeminiParseStreamToolCall(t *testing.T) {
	names := NewNameMapper()
	a := NewGeminiAdapter(names)
	_, _ = a.EncodeTools([]Tool{{Name: "time.now", InputSchema: map[string]any{}}})
	raw := &memStream{lines: lines(
		`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"time__now","args":{"tz":"UTC"}}}]},"finishReason":"STOP"}]}`,
	)}
	res, err := a.ParseStream(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "time.now" {
		t.Fatalf("toolcalls = %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Args["tz"] != "UTC" {
		t.Fatalf("args = %v", res.ToolCalls[0].Args)
	}
}

func TestResolveProfileDomestic(t *testing.T) {
	cases := map[string]ProtocolFamily{
		"volcengine": FamilyOpenAI,
		"DeepSeek":   FamilyOpenAI,
		"claude-3-5": FamilyAnthropic,
		"gemini-pro": FamilyGemini,
		"unknown-xx": FamilyOpenAI, // 默认
	}
	for provider, want := range cases {
		if got := ResolveProfile(provider).Family; got != want {
			t.Fatalf("%s: family = %s, want %s", provider, got, want)
		}
	}
}

func TestAdapterForTypes(t *testing.T) {
	if AdapterFor("claude", nil).Family() != "anthropic" {
		t.Fatal("claude should map to anthropic")
	}
	if AdapterFor("gemini", nil).Family() != "gemini" {
		t.Fatal("gemini should map to gemini")
	}
	if AdapterFor("deepseek", nil).Family() != "openai" {
		t.Fatal("deepseek should map to openai")
	}
}

func TestParsePromptToolCall(t *testing.T) {
	out := `好的，我来查一下。{"tool":"codegraph.search_definition","args":{"q":"Foo"}}`
	tc, ok := ParsePromptToolCall(out)
	if !ok || tc.Name != "codegraph.search_definition" || tc.Args["q"] != "Foo" {
		t.Fatalf("tc = %+v ok=%v", tc, ok)
	}
	if _, ok := ParsePromptToolCall("就是普通回答，没有工具"); ok {
		t.Fatal("should not parse a tool call from plain text")
	}
}
