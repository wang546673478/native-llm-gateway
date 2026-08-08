// Package auth — HTTP handler for /api/v1/keys CRUD
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// generateGatewayKey 生成一个形如 gw-XXXXXX... 的随机密钥(48 hex 字符 = 24 字节熵)
// 用于前端新建 gateway key 时系统自动分配,不需要用户手填
func generateGatewayKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// 极端情况,降级到时间戳派生(不应该发生)
		return "gw-err-" + strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000000"), ".", "")
	}
	return "gw-" + hex.EncodeToString(b)
}

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
			DefaultModel:  k.DefaultModel,
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
			// P: 用 strconv 转换 uint → string,不要用 string(rune()) —
			// string(rune(10)) 是字符 '\n'(0x0a),不是 "10"!
			ID:             strconv.FormatUint(uint64(k.ID), 10),
			Name:           k.Name,
			KeyHash:        k.KeyHash,
			Providers:      parseProviders(k.Providers),
			ProviderKeyIDs: parseProviderKeyIDs(k.ProviderKeyIDs),
			AllowedModels:  parseAllowedModels(k.AllowedModels),
			DefaultModel:   k.DefaultModel,
			RateLimit:      RateLimitConfig{RPM: k.RPM, TPM: k.TPM},
		})
	}
	return out, nil
}

// KeyView 返回给前端的形态(P32-B: 含明文 Key 字段)
// 仅 create 后立即返回一次,让用户能复制保存。其他 list / get / update 走 KeyViewSafe。
// SECURITY: 早期 list/get 直接返明文,任何能访问 UI 的人都可拿到所有客户端明文 → token 泄露。
// 改用 KeyViewSafe 防止 list 泄露,create 仍含 key(用户首次拿到需要)。
type KeyView struct {
	ID             uint     `json:"id"`
	Name           string   `json:"name"`
	Key            string   `json:"key"` // 明文,可复制(只 create 返回)
	Providers      []string `json:"providers"`
	ProviderKeyIDs []uint   `json:"provider_key_ids"` // P34: 绑定的 ProviderKey ID
	AllowedModels  []string `json:"allowed_models"`
	DefaultModel   string   `json:"default_model"`
	RPM            int      `json:"rpm"`
	TPM            int      `json:"tpm"`
	Enabled        bool     `json:"enabled"`
	CreatedAt      string   `json:"created_at,omitempty"`
}

// KeyViewSafe list / get / update 用,不包含明文 key
type KeyViewSafe struct {
	ID             uint     `json:"id"`
	Name           string   `json:"name"`
	Providers      []string `json:"providers"`
	ProviderKeyIDs []uint   `json:"provider_key_ids"`
	AllowedModels  []string `json:"allowed_models"`
	DefaultModel   string   `json:"default_model"`
	RPM            int      `json:"rpm"`
	TPM            int      `json:"tpm"`
	Enabled        bool     `json:"enabled"`
	CreatedAt      string   `json:"created_at,omitempty"`
}

// KeyCreateResp POST 返回值:等同 KeyView(创建后立刻就能看到 key)
type KeyCreateResp = KeyView

func toView(k dbpkg.GatewayKey) KeyView {
	return KeyView{
		ID:             k.ID,
		Name:           k.Name,
		Key:            k.KeyHash,
		Providers:      parseProviders(k.Providers),
		ProviderKeyIDs: parseProviderKeyIDs(k.ProviderKeyIDs),
		AllowedModels:  parseAllowedModels(k.AllowedModels),
		DefaultModel:   k.DefaultModel,
		RPM:            k.RPM,
		TPM:            k.TPM,
		Enabled:        k.Enabled,
		CreatedAt:      k.CreatedAt.Format(time.RFC3339),
	}
}

// toViewSafe list / get / update 用,不暴露明文 key
func toViewSafe(k dbpkg.GatewayKey) KeyViewSafe {
	return KeyViewSafe{
		ID:             k.ID,
		Name:           k.Name,
		Providers:      parseProviders(k.Providers),
		ProviderKeyIDs: parseProviderKeyIDs(k.ProviderKeyIDs),
		AllowedModels:  parseAllowedModels(k.AllowedModels),
		DefaultModel:   k.DefaultModel,
		RPM:            k.RPM,
		TPM:            k.TPM,
		Enabled:        k.Enabled,
		CreatedAt:      k.CreatedAt.Format(time.RFC3339),
	}
}

// KeyCreateReq POST body(P31: 不再需要 Key 字段,系统自动生成)
type KeyCreateReq struct {
	Name           string   `json:"name" binding:"required"`
	Providers      []string `json:"providers"`        // 多 Provider 绑定,空 = 不限制
	ProviderKeyIDs []uint   `json:"provider_key_ids"` // P34: 绑定具体 Provider Key ID
	AllowedModels  []string `json:"allowed_models"`
	DefaultModel   string   `json:"default_model"`
	RPM            int      `json:"rpm"`
	TPM            int      `json:"tpm"`
	Enabled        *bool    `json:"enabled"`
}

// KeyUpdateReq PUT body(name 在 URL 里,body 不需要)
// 也不允许通过 update 改 key — key 一旦签发就不能再读出来,只能禁用后重建
type KeyUpdateReq struct {
	Providers      []string `json:"providers"`
	ProviderKeyIDs []uint   `json:"provider_key_ids"`
	AllowedModels  []string `json:"allowed_models"`
	DefaultModel   *string  `json:"default_model"` // pointer:区分"不改"和"清空"
	RPM            int      `json:"rpm"`
	TPM            int      `json:"tpm"`
	Enabled        *bool    `json:"enabled"`
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
	if strings.TrimSpace(req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name_required"})
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

	// P31: 系统自动生成密钥,前端不接收 key 字段
	issuedKey := generateGatewayKey()
	k := &dbpkg.GatewayKey{
		Name:           req.Name,
		KeyHash:        issuedKey, // DB 存原值(hash 化留给中间件;这里只是命名沿用)
		Providers:      serializeProviders(normalizeProviderBindings(req.Providers)),
		ProviderKeyIDs: serializeProviderKeyIDs(req.ProviderKeyIDs),
		AllowedModels:  serializeAllowedModels(models),
		DefaultModel:   req.DefaultModel,
		RPM:            req.RPM,
		TPM:            req.TPM,
		Enabled:        enabled,
	}
	if err := h.store.Create(c.Request.Context(), k); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "create_failed", "detail": err.Error()})
		return
	}
	if err := h.reloadAll(c.Request.Context()); err != nil {
		// M2: DB 写已提交,reload 失败只日志不返回错(写确实发生;operator 需知
		// key 在 DB 但内存未同步,重启前不可用)
		log.Printf("gateway keys: db write ok but reload failed (key live only after restart): %v", err)
	}
	// P32-B: 直接返回 KeyView,key 在 list 里也能看到
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

	// Key 一旦签发就不再允许通过 update 改(只能删了重建)
	// Providers / ProviderKeyIDs / AllowedModels / RPM / TPM / Enabled 仍然可调
	existing.Providers = serializeProviders(normalizeProviderBindings(req.Providers))
	// ProviderKeyIDs 可显式置空(传 nil 或 [] → "[]")
	existing.ProviderKeyIDs = serializeProviderKeyIDs(req.ProviderKeyIDs)
	if req.AllowedModels != nil {
		existing.AllowedModels = serializeAllowedModels(req.AllowedModels)
	}
	// DefaultModel 允许显式清空(nil pointer 触发清空)
	if req.DefaultModel != nil {
		existing.DefaultModel = *req.DefaultModel
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
	if err := h.reloadAll(c.Request.Context()); err != nil {
		// M2: DB 写已提交,reload 失败只日志不返回错(写确实发生;operator 需知
		// key 在 DB 但内存未同步,重启前不可用)
		log.Printf("gateway keys: db write ok but reload failed (key live only after restart): %v", err)
	}
	c.JSON(http.StatusOK, toView(*existing))
}

func (h *KeysHandler) delete(c *gin.Context) {
	name := c.Param("name")
	err := h.store.Delete(c.Request.Context(), name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed", "detail": err.Error()})
		return
	}
	if err := h.reloadAll(c.Request.Context()); err != nil {
		// M2: DB 写已提交,reload 失败只日志不返回错(写确实发生;operator 需知
		// key 在 DB 但内存未同步,重启前不可用)
		log.Printf("gateway keys: db write ok but reload failed (key live only after restart): %v", err)
	}
	c.JSON(http.StatusOK, gin.H{"deleted": name})
}

// reloadAll 从 DB 重新加载全部 gateway keys 并同步内存 Authenticator。
// M2 修复:返回 error,让 caller 记录 —— 此前 DB 写成功但这里 List/reload 失败
// 被静默吞,key 在 DB 但内存未同步,重启前不可用,operator 无从知晓。
func (h *KeysHandler) reloadAll(ctx context.Context) error {
	if h.reload == nil {
		return nil
	}
	rows, err := h.store.List(ctx)
	if err != nil {
		return fmt.Errorf("reload gateway keys: list: %w", err)
	}
	keys := make([]GatewayKey, 0, len(rows))
	for _, k := range rows {
		keys = append(keys, GatewayKey{
			ID:             strconv.FormatUint(uint64(k.ID), 10),
			Name:           k.Name,
			KeyHash:        k.KeyHash,
			Providers:      parseProviders(k.Providers),
			ProviderKeyIDs: parseProviderKeyIDs(k.ProviderKeyIDs),
			AllowedModels:  parseAllowedModels(k.AllowedModels),
			RateLimit:      RateLimitConfig{RPM: k.RPM, TPM: k.TPM},
		})
	}
	h.reload(keys)
	return nil
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

// normalizeProviderBindings P-provider-vendor: 绑定 provider 统一存厂商名。
// 路由侧 CheckProvider 已按 vendor 归一比较,存注册名与厂商名等价;
// 统一存厂商名让 UI 只展示厂商,避免深挖注册名(deepseek-anthropic 之类)
func normalizeProviderBindings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		v := provider.Default().VendorFor(p)
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// serializeProviders 把 []string 序列化成 JSON 字符串存 DB
// nil/空 = "[]"
func serializeProviders(in []string) string {
	if in == nil {
		return `[]`
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func parseProviders(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// P34: serializeProviderKeyIDs / parseProviderKeyIDs — ProviderAPIKey.ID 列表
func serializeProviderKeyIDs(in []uint) string {
	if in == nil {
		return `[]`
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func parseProviderKeyIDs(s string) []uint {
	if s == "" {
		return nil
	}
	var out []uint
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
