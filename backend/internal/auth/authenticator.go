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
	tpm   map[string]*tpmCounter // key ID → tpm 计数(token)
}

// New 构造 Authenticator
func New(keys []GatewayKey) *Authenticator {
	a := &Authenticator{
		keys: make(map[string]*GatewayKey, len(keys)),
		rpm:  make(map[string]*rpmCounter, len(keys)),
		tpm:  make(map[string]*tpmCounter, len(keys)),
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
		a.tpm[k.ID] = &tpmCounter{usage: make(map[int64]int64)}
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

// CheckTPM 检查 token 配额
//   - limitTPM=0 → 不限
//   - estimatedTokens 是预估消耗(可以填 0 表示不预订,只追加)
// 返回 (allowed, reason)
//   allowed=false 时 reason 描述被拒原因
func (a *Authenticator) CheckTPM(keyID string, limitTPM int, estimatedTokens int64) (bool, string) {
	if limitTPM <= 0 {
		return true, ""
	}
	a.mu.RLock()
	c, ok := a.tpm[keyID]
	a.mu.RUnlock()
	if !ok {
		return true, ""
	}
	allowed, _ := c.AllowTokens(limitTPM, estimatedTokens)
	if !allowed {
		return false, "tpm_exceeded"
	}
	return true, ""
}

// RecordTokens 把实际用量追加到 TPM 计数
func (a *Authenticator) RecordTokens(keyID string, tokens int64) {
	if tokens <= 0 {
		return
	}
	a.mu.RLock()
	c, ok := a.tpm[keyID]
	a.mu.RUnlock()
	if !ok {
		return
	}
	c.RecordTokens(tokens)
}

// rpmCounter 简单的 1-minute 滑动窗口计数器
type rpmCounter struct {
	mu    sync.Mutex
	hits  []time.Time
	limit int
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

// tpmCounter token 计数器(按分钟累计)
// 用 map[minute_bucket]token 累加,旧 bucket 自然过期
type tpmCounter struct {
	mu    sync.Mutex
	usage map[int64]int64 // minute unix timestamp → tokens used
	limit int
}

// AllowTokens 检查剩余配额是否够用 tokens
// 返回 (allowed, usedInWindow)
// 简单实现:不允许"超量预订",而是请求进来时检查当前用量 + tokens 是否超 limit
func (c *tpmCounter) AllowTokens(limit int, tokens int64) (bool, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	bucket := now.Unix() / 60

	// 清掉上一分钟及之前的 bucket
	for b := range c.usage {
		if b < bucket {
			delete(c.usage, b)
		}
	}

	used := c.usage[bucket]
	if used+tokens > int64(limit) {
		return false, used
	}
	c.usage[bucket] = used + tokens
	return true, used + tokens
}

// RecordTokens 强制累加(用于请求结束后追加实际用量)
func (c *tpmCounter) RecordTokens(tokens int64) {
	if tokens <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := time.Now().Unix() / 60
	c.usage[bucket] += tokens
}
