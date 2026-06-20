package agent

// JSON Schema 按 provider 清洗/降级。
//
// MCP tool 的 inputSchema 是较完整的 JSON Schema，但各 provider 接受的子集不同：
//   - OpenAI（strict 模式）：要求 type:object，additionalProperties:false，
//     且 properties 里所有 key 都进 required。
//   - Gemini：schema 子集更窄——不支持 $ref / $schema / additionalProperties，
//     format 仅少数枚举，不认 oneOf/anyOf/allOf 等组合关键字。
//   - Anthropic：基本透传标准 JSON Schema，无需降级。
//
// 这些函数都返回深拷贝，绝不原地改 MCP 的 schema（它被多个 provider 共享）。

// cloneSchema 深拷贝任意 JSON 值（map/slice/标量）。
func cloneSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = cloneSchema(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = cloneSchema(val)
		}
		return out
	default:
		return v
	}
}

// ensureObject 保证一个最小可用的 object schema（provider 都要求顶层是 object）。
func ensureObject(s map[string]any) map[string]any {
	if s == nil {
		s = map[string]any{}
	}
	if _, ok := s["type"]; !ok {
		s["type"] = "object"
	}
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	return s
}

// SanitizeForOpenAI 产出 OpenAI strict 友好的 schema：
// additionalProperties:false，且把所有 property 都列入 required。
func SanitizeForOpenAI(schema map[string]any) map[string]any {
	s := ensureObject(cloneSchema(schema).(map[string]any))
	s["additionalProperties"] = false
	if props, ok := s["properties"].(map[string]any); ok {
		req := make([]any, 0, len(props))
		for k := range props {
			req = append(req, k)
		}
		s["required"] = req
		for k, p := range props {
			if pm, ok := p.(map[string]any); ok {
				props[k] = SanitizeForOpenAI(pm) // 递归处理嵌套 object
			}
		}
	}
	return s
}

// geminiAllowedFormats 是 Gemini 认可的 string format 子集。
var geminiAllowedFormats = map[string]bool{
	"enum": true, "date-time": true,
}

// geminiDropKeys 是 Gemini 不认、需要剥掉的关键字。
var geminiDropKeys = map[string]bool{
	"$ref": true, "$schema": true, "$defs": true, "definitions": true,
	"additionalProperties": true, "oneOf": true, "anyOf": true, "allOf": true,
	"not": true, "patternProperties": true, "const": true,
}

// SanitizeForGemini 递归剥掉 Gemini 不支持的关键字与 format。
func SanitizeForGemini(schema map[string]any) map[string]any {
	cloned, _ := cloneSchema(schema).(map[string]any)
	return sanitizeGemini(ensureObject(cloned))
}

func sanitizeGemini(s map[string]any) map[string]any {
	for k := range geminiDropKeys {
		delete(s, k)
	}
	if f, ok := s["format"].(string); ok && !geminiAllowedFormats[f] {
		delete(s, "format")
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, p := range props {
			if pm, ok := p.(map[string]any); ok {
				props[k] = sanitizeGemini(pm)
			}
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		s["items"] = sanitizeGemini(items)
	}
	return s
}

// SanitizeForAnthropic 基本透传（Anthropic 接受标准 JSON Schema），仅保证顶层是 object。
func SanitizeForAnthropic(schema map[string]any) map[string]any {
	return ensureObject(cloneSchema(schema).(map[string]any))
}
