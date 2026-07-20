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
	for k, vs := range src {
		if isHopByHop(k) {
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

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// writeJSONError 统一写错误响应(OpenAI 格式,与规格书 9.1 一致)
func writeJSONError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
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
