// Package auth 实现客户端 Gateway API Key 认证
// 对应规格书 5.9
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GatewayKey 是客户端可以使用的 Gateway 凭证
type GatewayKey struct {
	ID            string
	Name          string
	KeyHash       string
	AllowedModels []string
	RateLimit     RateLimitConfig
}

// RateLimitConfig RPM/TPM 限制
type RateLimitConfig struct {
	RPM int // requests per minute
	TPM int // tokens per minute(预留,P7 暂不强制)
}

// Errors
var (
	ErrMissingAuthHeader = errors.New("auth: missing Authorization header")
	ErrInvalidAuthFormat = errors.New("auth: Authorization must be Bearer <token>")
	ErrUnknownKey        = errors.New("auth: unknown gateway key")
	ErrModelNotAllowed   = errors.New("auth: model not allowed for this key")
)

// Authenticator 持有所有 Gateway Keys
// 简单内存存储,启动时从 config 加载;生产可换 DB 后端(P7)
type Authenticator struct {
	mu    sync.RWMutex
	keys  map[string]*GatewayKey // hash → key
	rpm   map[string]*rpmCounter // key ID → rpm 计数
}

// New 构造 Authenticator
func New(keys []GatewayKey) *Authenticator {
	a := &Authenticator{
		keys: make(map[string]*GatewayKey, len(keys)),
		rpm:  make(map[string]*rpmCounter, len(keys)),
	}
	for i := range keys {
		k := keys[i]
		if k.KeyHash == "" && k.Name != "" {
			// 兼容未 hash 的 key(测试用):用 name 当 hash
			k.KeyHash = hashKey(k.Name)
		}
		if k.ID == "" {
			k.ID = k.Name
		}
		a.keys[k.KeyHash] = &k
		a.rpm[k.ID] = &rpmCounter{}
	}
	return a
}

// hashKey 计算 SHA256
func hashKey(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// Authenticate 验证 Bearer token,返回对应的 GatewayKey
func (a *Authenticator) Authenticate(r *http.Request) (*GatewayKey, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, ErrMissingAuthHeader
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return nil, ErrInvalidAuthFormat
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return nil, ErrInvalidAuthFormat
	}

	hash := hashKey(token)
	a.mu.RLock()
	key, ok := a.keys[hash]
	a.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownKey
	}
	return key, nil
}

// CheckAllowed 检查 model 是否在该 key 允许的范围内
func (a *Authenticator) CheckAllowed(key *GatewayKey, model string) error {
	if len(key.AllowedModels) == 0 {
		return nil
	}
	for _, allowed := range key.AllowedModels {
		if allowed == "*" || allowed == model {
			return nil
		}
	}
	return ErrModelNotAllowed
}

// AllowRequest 检查 RPM 限制,返回 true 表示放行
func (a *Authenticator) AllowRequest(keyID string, limitRPM int) bool {
	if limitRPM <= 0 {
		return true
	}
	a.mu.RLock()
	c, ok := a.rpm[keyID]
	a.mu.RUnlock()
	if !ok {
		return true
	}
	return c.allow(limitRPM)
}

// rpmCounter 简单的 1-minute 滑动窗口计数器
type rpmCounter struct {
	mu      sync.Mutex
	hits    []time.Time
	limit   int
}

func (c *rpmCounter) allow(limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)

	// 清掉窗口外
	newHits := c.hits[:0]
	for _, t := range c.hits {
		if t.After(cutoff) {
			newHits = append(newHits, t)
		}
	}
	c.hits = newHits

	if len(c.hits) >= limit {
		return false
	}
	c.hits = append(c.hits, now)
	return true
}
