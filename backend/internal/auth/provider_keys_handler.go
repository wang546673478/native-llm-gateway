// Package auth — ProviderAPIKey 仓库 + HTTP handler
// 管理每个 Provider 的上游 LLM API key(替代 config.yaml 里的 providers.x.keys[])
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// ProviderKeyStore ProviderAPIKey 仓库
type ProviderKeyStore interface {
	List(ctx context.Context, providerName string) ([]dbpkg.ProviderAPIKey, error)
	Create(ctx context.Context, k *dbpkg.ProviderAPIKey) error
	Delete(ctx context.Context, id uint) error
	GetPlainKeys(ctx context.Context, providerName string) ([]string, error) // 内部用,返回明文
}

type gormProviderKeyStore struct{ db *gorm.DB }

func NewProviderKeyStore(db *gorm.DB) ProviderKeyStore { return &gormProviderKeyStore{db: db} }

func (s *gormProviderKeyStore) List(ctx context.Context, providerName string) ([]dbpkg.ProviderAPIKey, error) {
	var out []dbpkg.ProviderAPIKey
	q := s.db.WithContext(ctx).Model(&dbpkg.ProviderAPIKey{})
	if providerName != "" {
		q = q.Where("provider_name = ?", providerName)
	}
	if err := q.Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormProviderKeyStore) Create(ctx context.Context, k *dbpkg.ProviderAPIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	k.UpdatedAt = k.CreatedAt
	return s.db.WithContext(ctx).Create(k).Error
}

func (s *gormProviderKeyStore) Delete(ctx context.Context, id uint) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&dbpkg.ProviderAPIKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// GetPlainKeys 返回某个 provider 的所有 enabled key 的明文(给 KeyPool 用)
func (s *gormProviderKeyStore) GetPlainKeys(ctx context.Context, providerName string) ([]string, error) {
	var keys []dbpkg.ProviderAPIKey
	if err := s.db.WithContext(ctx).
		Where("provider_name = ? AND enabled = ?", providerName, true).
		Order("id ASC").
		Find(&keys).Error; err != nil {
		return nil, err
	}
	plain := make([]string, 0, len(keys))
	for _, k := range keys {
		plain = append(plain, k.KeyHash)
	}
	return plain, nil
}

// ProviderKeyView 返回给前端(不含明文 key)
// P48: 加 BillingSource — 路由时按这个字段选 tier(token_plan 优先)
// P68: 加 Status 字段,前端可显示 QUOTA_EXCEEDED 等运行时状态
type ProviderKeyView struct {
	ID            uint      `json:"id"`
	ProviderName  string    `json:"provider_name"`
	Name          string    `json:"name"`
	// KeyMasked 是脱敏后的 key(只显示前 8 + 后 4 字符)
	KeyMasked     string    `json:"key_masked"`
	Enabled       bool      `json:"enabled"`
	// P68: 运行时状态 — "ACTIVE" / "COOLING" / "QUOTA_EXCEEDED" / "DISABLED" 等
	// 由 poolLookup 在 list 时填充。如果 pool 找不到该 key,fallback 到 "ACTIVE"。
	Status        string    `json:"status"`
	BillingSource string    `json:"billing_source"` // P48: token_plan / api / free
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toProviderKeyView(k dbpkg.ProviderAPIKey, status string) ProviderKeyView {
	if status == "" {
		status = "ACTIVE"
	}
	return ProviderKeyView{
		ID:            k.ID,
		ProviderName:  k.ProviderName,
		Name:          k.Name,
		KeyMasked:     maskKey(k.KeyHash),
		Enabled:       k.Enabled,
		Status:        status,
		BillingSource: k.BillingSource,
		CreatedAt:     k.CreatedAt,
		UpdatedAt:    k.UpdatedAt,
	}
}

// maskKey 脱敏:前 8 + ... + 后 4
func maskKey(k string) string {
	if len(k) <= 16 {
		if len(k) > 8 {
			return k[:4] + "..." + k[len(k)-4:]
		}
		return k[:2] + "..."
	}
	return k[:8] + "..." + k[len(k)-4:]
}

// ProviderKeysHandler CRUD for /api/v1/providers/:name/api-keys
type ProviderKeysHandler struct {
	store ProviderKeyStore
	// P35: reload hook — Create/Delete 后调一次,让 Server 重建 Pool 并注入到 Provider
	reload func(providerName string)
	// P68: 查运行时 key status (QUOTA_EXCEEDED / DISABLED 等)
	// 由 server.go 注入。如果 nil,list 时 status 默认为 "ACTIVE"。
	keyStatusLookup func(providerName, keyID string) string
	// P68: 把指定 key 标 QUOTA_EXCEEDED(admin 端 e2e / 调试用)
	quotaMarkFunc func(providerName, keyID string)
}

// NewProviderKeysHandler 构造 handler
func NewProviderKeysHandler(db *gorm.DB, reload func(providerName string)) *ProviderKeysHandler {
	return &ProviderKeysHandler{store: NewProviderKeyStore(db), reload: reload}
}

// SetKeyStatusLookup P68: 注入 status lookup(从 Pool 查 key 当前状态)
// 在 server.go 启动时设置,after buildKeyPools 完毕。
func (h *ProviderKeysHandler) SetKeyStatusLookup(fn func(providerName, keyID string) string) {
	h.keyStatusLookup = fn
}

// SetQuotaMarkFunc P68: 注入 quota mark func(把 key 标 QUOTA_EXCEEDED)
// 由 server.go 启动时设置
func (h *ProviderKeysHandler) SetQuotaMarkFunc(fn func(providerName, keyID string)) {
	h.quotaMarkFunc = fn
}

// Register 挂到 r.Group
func (h *ProviderKeysHandler) Register(r *gin.RouterGroup) {
	// 单独在 Providers 那块路径下
	// r.GET("/providers/:name/api-keys", h.list)
	// r.POST("/providers/:name/api-keys", h.create)
	// ...
	// 为了方便,接受任意 :name(GET/POST) 走 r.Group
}

// RegisterOn 在指定 group 上注册路由(供 server.go 调用)
// 路由前缀应为 /api/v1
func (h *ProviderKeysHandler) RegisterOn(r *gin.RouterGroup) {
	r.GET("/providers/:name/api-keys", h.list)
	r.POST("/providers/:name/api-keys", h.create)
	r.DELETE("/providers/:name/api-keys/:id", h.delete)
	// P68: 手动 mark key 为 QUOTA_EXCEEDED(调试 / 强制触发 UI 状态)
	r.POST("/providers/:name/api-keys/:id/mark-quota-exceeded", h.markQuotaExceeded)
}

func (h *ProviderKeysHandler) list(c *gin.Context) {
	providerName := c.Param("name")
	rows, err := h.store.List(c.Request.Context(), providerName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "detail": err.Error()})
		return
	}
	views := make([]ProviderKeyView, 0, len(rows))
	for _, r := range rows {
		status := ""
		if h.keyStatusLookup != nil {
			status = h.keyStatusLookup(providerName, fmt.Sprintf("%d", r.ID))
		}
		views = append(views, toProviderKeyView(r, status))
	}
	c.JSON(http.StatusOK, gin.H{
		"keys":   views,
		"count":  len(views),
		"provider": providerName,
	})
}

type createProviderKeyReq struct {
	Name          string `json:"name"`
	Key           string `json:"key" binding:"required"`
	Enabled       *bool  `json:"enabled"`
	// P48: 创建 key 时指定计费来源(token_plan / api / free)
	// 默认 "api"(向后兼容);为空时也用 "api"
	BillingSource string `json:"billing_source"`
}

func (h *ProviderKeysHandler) create(c *gin.Context) {
	providerName := c.Param("name")
	if providerName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing_provider_name"})
		return
	}
	var req createProviderKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": err.Error()})
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key_required"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		// 默认 name = "default" 或第 N 个
		existing, _ := h.store.List(c.Request.Context(), providerName)
		name = fmt.Sprintf("key-%d", len(existing)+1)
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	billingSource := strings.TrimSpace(req.BillingSource)
	if billingSource == "" {
		billingSource = "api" // 默认
	}
	// 简单校验,避免脏数据
	switch billingSource {
	case "token_plan", "api", "free":
		// ok
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid_billing_source",
			"detail":  "billing_source must be one of: token_plan, api, free",
			"got":     billingSource,
		})
		return
	}
	k := &dbpkg.ProviderAPIKey{
		ProviderName:  providerName,
		Name:          name,
		KeyHash:       req.Key,
		Enabled:       enabled,
		BillingSource: billingSource,
	}
	if err := h.store.Create(c.Request.Context(), k); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "create_failed", "detail": err.Error()})
		return
	}
	// P35: 触发 Pool reload,新 key 立刻生效
	if h.reload != nil {
		h.reload(providerName)
	}
	// 新建的 key 默认 ACTIVE
	c.JSON(http.StatusCreated, toProviderKeyView(*k, "ACTIVE"))
}

func (h *ProviderKeysHandler) delete(c *gin.Context) {
	idStr := c.Param("id")
	var id uint
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	// P35: 删除前先查这个 key 属于哪个 provider(用于 reload)
	rows, err := h.store.List(c.Request.Context(), "")
	var providerName string
	if err == nil {
		for _, r := range rows {
			if r.ID == id {
				providerName = r.ProviderName
				break
			}
		}
	}
	err = h.store.Delete(c.Request.Context(), id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed", "detail": err.Error()})
		return
	}
	// P35: 触发 Pool reload(查不到 providerName 也 reload 全量是 OK 的)
	if h.reload != nil {
		h.reload(providerName)
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// markQuotaExceeded POST /api/v1/providers/:name/api-keys/:id/mark-quota-exceeded
// P68: 手动把 key 标 QUOTA_EXCEEDED(用来测试 e2e / 调试 UI 状态徽章)。
// 在生产环境这个 endpoint 主要是给 admin 当作 "force-disable-pending-quota-restore" 工具,
// 等价于真实的 quota_exceeded 触发但不需要真的耗尽 quota。
func (h *ProviderKeysHandler) markQuotaExceeded(c *gin.Context) {
	providerName := c.Param("name")
	idStr := c.Param("id")
	var id uint
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if h.quotaMarkFunc == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "quota_mark_not_wired"})
		return
	}
	h.quotaMarkFunc(providerName, fmt.Sprintf("%d", id))
	c.JSON(http.StatusOK, gin.H{"marked_quota_exceeded": true})
}

// BuildProviderPools 从 DB 读所有 provider 的 enabled key,
// 构造 provider_name → *keypool.Pool 的映射
// Authenticator 启动时调一次,之后 reload 也会调
func BuildProviderPools(ctx context.Context, store ProviderKeyStore, poolCfg keypool.Config) (map[string]*keypool.Pool, error) {
	if store == nil {
		return map[string]*keypool.Pool{}, nil
	}
	// 先拿所有 provider(按名字分组)
	rows, err := store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	byProvider := map[string][]string{}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		byProvider[r.ProviderName] = append(byProvider[r.ProviderName], r.KeyHash)
	}
	pools := make(map[string]*keypool.Pool, len(byProvider))
	for name, keys := range byProvider {
		if len(keys) == 0 {
			continue
		}
		pools[name] = keypool.BuildPoolFromStrings(name, keys, poolCfg)
	}
	return pools, nil
}

// 防止引入但没用
var _ = errors.New