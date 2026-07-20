// Package auth — HTTP handler for /api/v1/keys CRUD
package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// KeysHandler 处理 /api/v1/keys 的 CRUD
// 改动通过 Reload() 推到 Authenticator,Auth 中间件下一次请求就生效
type KeysHandler struct {
	store  KeyStore
	reload func(keys []GatewayKey) // 写完调用,触发 Authenticator.Reload
}

// NewKeysHandler 构造 handler
func NewKeysHandler(db *gorm.DB, reload func(keys []GatewayKey)) *KeysHandler {
	return &KeysHandler{store: NewKeyStore(db), reload: reload}
}

// Register 挂到 router
func (h *KeysHandler) Register(r *gin.RouterGroup) {
	r.GET("/keys", h.list)
	r.POST("/keys", h.create)
	r.PUT("/keys/:name", h.update)
	r.DELETE("/keys/:name", h.delete)
}

// SeedFromConfig 把 config 里的 keys 同步到 DB(已存在则跳过)
// 通常在启动时调用一次,把 YAML 里的初始 keys 落库,UI 就能看到
func SeedFromConfig(ctx context.Context, db *gorm.DB, cfgKeys []GatewayKey) error {
	store := NewKeyStore(db)
	for _, k := range cfgKeys {
		existing, err := store.GetByName(ctx, k.Name)
		if err != nil {
			return err
		}
		if existing != nil {
			continue // 不覆盖,DB 已经存在
		}
		models := k.AllowedModels
		if models == nil {
			models = []string{"*"}
		}
		enabled := true
		newK := &dbpkg.GatewayKey{
			Name:          k.Name,
			KeyHash:       k.KeyHash,
			AllowedModels: serializeAllowedModels(models),
			RPM:           k.RateLimit.RPM,
			TPM:           k.RateLimit.TPM,
			Enabled:       enabled,
		}
		if err := store.Create(ctx, newK); err != nil {
			return err
		}
	}
	return nil
}

// LoadFromDB 从 DB 加载所有 keys,供 Authenticator 初始化用
func LoadFromDB(ctx context.Context, db *gorm.DB) ([]GatewayKey, error) {
	store := NewKeyStore(db)
	rows, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GatewayKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, GatewayKey{
			ID:            string(rune(k.ID)),
			Name:          k.Name,
			KeyHash:       k.KeyHash,
			AllowedModels: parseAllowedModels(k.AllowedModels),
			RateLimit:     RateLimitConfig{RPM: k.RPM, TPM: k.TPM},
		})
	}
	return out, nil
}

// KeyView 返回给前端的形态(不含密钥 hash)
type KeyView struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	Enabled       bool     `json:"enabled"`
}

func toView(k dbpkg.GatewayKey) KeyView {
	return KeyView{
		Name:          k.Name,
		AllowedModels: parseAllowedModels(k.AllowedModels),
		RPM:           k.RPM,
		TPM:           k.TPM,
		Enabled:       k.Enabled,
	}
}

// KeyCreateReq POST body
type KeyCreateReq struct {
	Name          string   `json:"name" binding:"required"`
	Key           string   `json:"key" binding:"required"`
	AllowedModels []string `json:"allowed_models"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	Enabled       *bool    `json:"enabled"`
}

// KeyUpdateReq PUT body(name 在 URL 里,body 不需要)
type KeyUpdateReq struct {
	Key           string   `json:"key"`
	AllowedModels []string `json:"allowed_models"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	Enabled       *bool    `json:"enabled"`
}

func (h *KeysHandler) list(c *gin.Context) {
	rows, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "detail": err.Error()})
		return
	}
	views := make([]KeyView, 0, len(rows))
	for _, k := range rows {
		views = append(views, toView(k))
	}
	c.JSON(http.StatusOK, gin.H{"keys": views, "count": len(views)})
}

func (h *KeysHandler) create(c *gin.Context) {
	var req KeyCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Key) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name_and_key_required"})
		return
	}

	models := req.AllowedModels
	if models == nil {
		models = []string{"*"}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	k := &dbpkg.GatewayKey{
		Name:          req.Name,
		KeyHash:       req.Key, // Repository 会原样存;Authenticator 内部 hash
		AllowedModels: serializeAllowedModels(models),
		RPM:           req.RPM,
		TPM:           req.TPM,
		Enabled:       enabled,
	}
	if err := h.store.Create(c.Request.Context(), k); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "create_failed", "detail": err.Error()})
		return
	}
	h.reloadAll(c.Request.Context())
	c.JSON(http.StatusCreated, toView(*k))
}

func (h *KeysHandler) update(c *gin.Context) {
	name := c.Param("name")
	var req KeyUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": err.Error()})
		return
	}
	existing, err := h.store.GetByName(c.Request.Context(), name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup_failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}

	if strings.TrimSpace(req.Key) != "" {
		existing.KeyHash = req.Key
	}
	if req.AllowedModels != nil {
		existing.AllowedModels = serializeAllowedModels(req.AllowedModels)
	}
	existing.RPM = req.RPM
	existing.TPM = req.TPM
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}

	if err := h.store.Update(c.Request.Context(), name, existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update_failed", "detail": err.Error()})
		return
	}
	h.reloadAll(c.Request.Context())
	c.JSON(http.StatusOK, toView(*existing))
}

func (h *KeysHandler) delete(c *gin.Context) {
	name := c.Param("name")
	if err := h.store.Delete(c.Request.Context(), name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed", "detail": err.Error()})
		return
	}
	h.reloadAll(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"deleted": name})
}

func (h *KeysHandler) reloadAll(ctx context.Context) {
	if h.reload == nil {
		return
	}
	rows, err := h.store.List(ctx)
	if err != nil {
		return
	}
	keys := make([]GatewayKey, 0, len(rows))
	for _, k := range rows {
		enabled := k.Enabled
		keys = append(keys, GatewayKey{
			ID:            string(rune(k.ID)),
			Name:          k.Name,
			KeyHash:       k.KeyHash,
			AllowedModels: parseAllowedModels(k.AllowedModels),
			RateLimit:     RateLimitConfig{RPM: k.RPM, TPM: k.TPM},
			// 暂时忽略 Enabled 字段(Authenticator 暂不支持 disable,留 TODO)
		})
		_ = enabled
	}
	h.reload(keys)
}

// serializeAllowedModels 把 []string 序列化成 JSON 字符串存 DB
func serializeAllowedModels(in []string) string {
	if in == nil {
		return `["*"]`
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func parseAllowedModels(s string) []string {
	if s == "" {
		return []string{"*"}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{"*"}
	}
	if out == nil {
		return []string{"*"}
	}
	return out
}

// (移除早期手写的 JSON helper,改用 encoding/json)
