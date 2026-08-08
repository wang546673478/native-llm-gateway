package proxy

import (
	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/auth"
)

// GatewayKeyContext 单一职责:把 magic key 提取封装为类型
// 之前 5 处 c.Get("gateway_key") 散落在 proxy.go 5 个函数里,改 auth 字段要改 5 处
// 改为:GatewayKeyContext 提供显式方法,所有调用都走这里
type GatewayKeyContext interface {
	Get(c *gin.Context) *auth.GatewayKey
	GetRequired(c *gin.Context) *auth.GatewayKey
	ID(c *gin.Context) string
}

type gatewayKeyContext struct {
	keyField string
	idField  string
}

func NewGatewayKeyContext(keyField, idField string) GatewayKeyContext {
	return &gatewayKeyContext{keyField: keyField, idField: idField}
}

var defaultGatewayKeyContext = NewGatewayKeyContext("gateway_key", "gateway_key_id")

func (g *gatewayKeyContext) Get(c *gin.Context) *auth.GatewayKey {
	if c == nil {
		return nil
	}
	v, ok := c.Get(g.keyField)
	if !ok {
		return nil
	}
	gk, _ := v.(*auth.GatewayKey)
	return gk
}

func (g *gatewayKeyContext) GetRequired(c *gin.Context) *auth.GatewayKey {
	gk := g.Get(c)
	if gk == nil {
		panic("proxy: gateway_key not set in context — auth middleware misconfigured")
	}
	return gk
}

func (g *gatewayKeyContext) ID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return c.GetString(g.idField)
}
