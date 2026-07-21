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
	// Providers 绑定:空 = 不限制;非空 = 只能用路由解析到这些 Provider 之一的请求
	// 例:["deepseek", "deepseek-anthropic"] 表示 deepseek 的 OpenAI 和
	// Anthropic 兼容端点都能用(共享同一个 API key)
	Providers     []string
	// P34: ProviderKeyIDs 绑定具体凭证(ProviderAPIKey.ID)
	// 空 = 不限制(用该 provider 的所有 key 池里挑)
	// 非空 = 只用 ID 在这个集合里的 Provider Key(精准锁定凭证)
	// 例:[5, 7] 表示只能用 ProviderAPIKey 表里 ID=5 和 ID=7 的上游 key
	ProviderKeyIDs []uint
	AllowedModels  []string
	RateLimit      RateLimitConfig
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
	ErrKeyProviderMismatch = errors.New("auth: key is bound to a different provider")
)

// CheckProvider 验证 key 是否能用指定 provider
// 当 key.Providers 为空时,允许任意 provider(返回 nil)
// 否则只允许 providerName 在 key.Providers 里
func (a *Authenticator) CheckProvider(key *GatewayKey, providerName string) error {
	if key == nil {
		return ErrUnknownKey
	}
	if len(key.Providers) == 0 {
		return nil // 未绑定,任意 provider 都行
	}
	for _, p := range key.Providers {
		if p == providerName {
			return nil
		}
	}
	return ErrKeyProviderMismatch
}

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
	a.addKeys(keys)
	return a
}

// addKeys 内部 helper:把 keys 加到 map(也用于 Reload)
// KeyHash 是"密钥原值"(明文),内部统一 hash 后存入 keys map
func (a *Authenticator) addKeys(keys []GatewayKey) {
	for i := range keys {
		k := keys[i]
		if k.KeyHash == "" && k.Name != "" {
			// 兼容测试:用 name 当密钥
			k.KeyHash = k.Name
		}
		if k.ID == "" {
			k.ID = k.Name
		}
		// 统一把"密钥原值"hash 后作为 map key
		hashedKey := hashKey(k.KeyHash)
		k.KeyHash = hashedKey // 存 hash 后的版本,lookup 时也用 hash 比较
		a.keys[hashedKey] = &k
		if _, ok := a.rpm[k.ID]; !ok {
			a.rpm[k.ID] = &rpmCounter{}
		}
		if _, ok := a.tpm[k.ID]; !ok {
			a.tpm[k.ID] = &tpmCounter{usage: make(map[int64]int64)}
		}
	}
}

// Reload 重新加载 Gateway Keys(P14 热重载)
// 保留现有 keys 的 RPM/TPM 计数(避免重置限流窗口)
func (a *Authenticator) Reload(keys []GatewayKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = make(map[string]*GatewayKey, len(keys))
	a.addKeys(keys)
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

	// token 与 KeyHash 都统一走 hashKey 函数比较
	// (config 里的 KeyHash 是明文时会被 addKeys 内部 hash 一次)
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
