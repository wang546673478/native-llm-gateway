package proxy

import (
	"bytes"
	"encoding/json"
)

// bodyMeta 仅解析 model / stream,其他字段原样保留
type bodyMeta struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// extractModelAndStream 从原始 body 中抽出 model 和 stream
// 失败时 model 返回空,isStream 返回 false,err 返回具体原因
//
// 重要:此函数**只读** body,绝不改写。Provider 拿到的依然是原始字节。
func extractModelAndStream(body []byte) (model string, isStream bool, err error) {
	if len(body) == 0 {
		return "", false, nil
	}
	var m bodyMeta
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // 不要严格;未知字段会被忽略
	if err := json.Unmarshal(body, &m); err != nil {
		// 解码失败:可能是非 JSON(例如 multipart),返回空 model 让后续流程
		// 用 direct-model lookup 兜底
		return "", false, nil
	}
	return m.Model, m.Stream, nil
}

// rewriteModelField 用 JSON 解析 + 序列化重写 body 里的 model 字段
// 用 map[string]interface{} 而不是 struct 是为了保留其他未知字段
// (Anthropic tools / OpenAI 各种 extra_body 都有未声明字段)
//   - body 不是合法 JSON:返回原 body + false
//   - 解析成功:重写 model 字段返回新 body + true
//
// 与 Gateway 的"body 透传"原则一致:只改 model 这一个字段,其他不变。
func rewriteModelField(body []byte, newModel string) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}
