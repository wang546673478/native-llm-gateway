package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// hop-by-hop headers per RFC 7230,Gateway 透传响应时必须删除
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// copyResponseHeaders 把 Provider 响应 headers 复制到 gin context
// 跳过 hop-by-hop headers 和 Content-Length(由 Gin 按 body 长度自动设置)
func copyResponseHeaders(c *gin.Context, src map[string][]string) {
	connectionHeaders := make(map[string]struct{})
	for key, values := range src {
		if !strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				if token = strings.TrimSpace(strings.ToLower(token)); token != "" {
					connectionHeaders[token] = struct{}{}
				}
			}
		}
	}
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		if _, nominated := connectionHeaders[strings.ToLower(k)]; nominated {
			continue
		}
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
}

// copyRelayResponseHeaders replaces Gateway defaults with the upstream
// end-to-end header set. A Gateway trace is added only when the upstream did
// not provide its own request ID.
func copyRelayResponseHeaders(c *gin.Context, src http.Header, traceID string) {
	clear(c.Writer.Header())
	copyResponseHeaders(c, src)
	if c.Writer.Header().Get("X-Request-Id") == "" && traceID != "" {
		c.Writer.Header().Set("X-Request-Id", traceID)
	}
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// writeJSONError 统一写 OpenAI 形状的错误响应。
func writeJSONError(c *gin.Context, status int, errType, message string) {
	// P: error 响应里带 trace_id 字段,让客户端能直接拿来在 access_log 里反查
	// (X-Request-Id header 同时设了,JSON body 也带一份,方便不读 header 的客户端)
	traceID := c.GetString("trace_id")
	body := gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	}
	if traceID != "" {
		body["trace_id"] = traceID
	}
	if traceID != "" {
		c.Writer.Header().Set("X-Request-Id", traceID)
	}
	c.JSON(status, body)
}

// writeGatewayError 把 gateway 层面的错误(502 等)按协议格式回客户端
func writeGatewayError(c *gin.Context, proto provider.Protocol, message string) {
	switch proto {
	case provider.ProtocolAnthropic:
		c.JSON(http.StatusBadGateway, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "gateway_error",
				"message": message,
			},
		})
	case provider.ProtocolGoogle:
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"code":    502,
				"message": message,
				"status":  "GATEWAY_ERROR",
			},
		})
	default: // openai
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"type":    "gateway_error",
				"message": message,
			},
		})
	}
}

// writeJSON marshals v to JSON or returns marshal error
func toJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":{"type":"internal","message":"marshal failed"}}`)
	}
	return b
}
