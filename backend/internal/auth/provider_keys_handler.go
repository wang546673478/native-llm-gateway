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
	return s.db.WithContext(ctx).Where("id = ?", id).Delete(&dbpkg.ProviderAPIKey{}).Error
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
type ProviderKeyView struct {
	ID           uint      `json:"id"`
	ProviderName string    `json:"provider_name"`
	Name         string    `json:"name"`
	// KeyMasked 是脱敏后的 key(只显示前 8 + 后 4 字符)
	KeyMasked    string    `json:"key_masked"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func toProviderKeyView(k dbpkg.ProviderAPIKey) ProviderKeyView {
	return ProviderKeyView{
		ID:           k.ID,
		ProviderName: k.ProviderName,
		Name:         k.Name,
		KeyMasked:    maskKey(k.KeyHash),
		Enabled:      k.Enabled,
		CreatedAt:    k.CreatedAt,
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
}

// NewProviderKeysHandler 构造 handler
func NewProviderKeysHandler(db *gorm.DB) *ProviderKeysHandler {
	return &ProviderKeysHandler{store: NewProviderKeyStore(db)}
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
		views = append(views, toProviderKeyView(r))
	}
	c.JSON(http.StatusOK, gin.H{
		"keys":   views,
		"count":  len(views),
		"provider": providerName,
	})
}

type createProviderKeyReq struct {
	Name    string `json:"name"`
	Key     string `json:"key" binding:"required"`
	Enabled *bool  `json:"enabled"`
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
	k := &dbpkg.ProviderAPIKey{
		ProviderName: providerName,
		Name:         name,
		KeyHash:      req.Key,
		Enabled:      enabled,
	}
	if err := h.store.Create(c.Request.Context(), k); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "create_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, toProviderKeyView(*k))
}

func (h *ProviderKeysHandler) delete(c *gin.Context) {
	idStr := c.Param("id")
	var id uint
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
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