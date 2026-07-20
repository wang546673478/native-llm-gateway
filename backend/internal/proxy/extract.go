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
