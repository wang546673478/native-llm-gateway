// Package server 负责 Gateway 服务的启动、编排和优雅关停
// 对应规格书 5.x 服务生命周期
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/adminauth"
	"github.com/wang546673478/native-llm-gateway/internal/api/http/handler"
	"github.com/wang546673478/native-llm-gateway/internal/api/http/middleware"
	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/circuit"
	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/fingerprint"
	"github.com/wang546673478/native-llm-gateway/internal/inflight"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/metrics"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/mimo"
	"github.com/wang546673478/native-llm-gateway/internal/provider/relay"
	"github.com/wang546673478/native-llm-gateway/internal/proxy"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
	"github.com/wang546673478/native-llm-gateway/internal/router"
	"github.com/wang546673478/native-llm-gateway/internal/usage"
)

// Server 持有所有运行时依赖
type Server struct {
	cfg     *config.Config
	logger  *zap.Logger
	db      *gorm.DB
	manager *provider.Manager
	// modelStoreAdapter 把 database.ProviderModelStore 适配成 provider.ModelStore,
	// 首次启动(New)与热重载(Reload)都用它读 DB provider_models 注入 manager 定价。
	modelStoreAdapter provider.ModelStore
	router            *router.Router
	engine            *proxy.Engine
	pools             map[string]*keypool.Pool
	poolsMu           sync.RWMutex // F4: 保护 s.pools 字段读(s.pools[name])/写(= newPools)
	auth              *auth.Authenticator
	usageC            *usage.Collector
	usageR            *usage.Repository
	metricsC          *metrics.Collector
	accessR           *accesslog.Recorder // P67: 接入日志 Recorder
	quotaM            *quotacheck.Manager // P68: 配额恢复 worker
	inflightR         *inflight.Registry  // P-inflight: 活跃请求内存快照表
	adminAuthM        *adminauth.Manager  // P-admin-auth: 管理员认证管理器
	// fpEnabled 设备指纹归一化开关原子值(运行时热切,PATCH /api/v1/fingerprint 翻转)。
	// 指针:engine 闭包与 server 持有同一个实例,热切换共享。默认 = cfg.Fingerprint 开关。
	// atomic 保证热路径只读一次、无锁。
	fpEnabled *atomic.Bool
	// fpSnapshot 启动时 Capture 一次的网关环境快照。热切换只翻 enabled,不改快照。
	fpSnapshot fingerprint.Snapshot
	http       *http.Server
}

// New 构造 Server
func New(cfg *config.Config, logger *zap.Logger, db *gorm.DB, manager *provider.Manager) (*Server, error) {
	// P3+P4+P5: 构造 KeyPool map + Router + Proxy
	// P30:从 DB (provider_api_keys 表) 读 key 而不是 config.yaml
	pools := buildKeyPools(cfg, db, logger)

	// P-relay-independent: 为已加载的中转站构建 KeyPool
	registry := provider.Default()
	for name := range manager.GetAll() {
		if registry.IsRelay(name) {
			vendor := registry.VendorFor(name)
			if _, exists := pools[vendor]; !exists {
				pool := buildProviderPool(cfg, db, vendor, logger)
				pools[vendor] = pool
				logger.Info("built keypool for relay station", zap.String("vendor", vendor), zap.String("name", name))
			}
		}
	}

	// P-state-persist: 恢复上次优雅关停的 key 状态(QE/COOLING/余额)。
	// 必须在 quotacheck.NewManager(injectCallbacks)之前 — QE key 恢复后
	// 立即被 callback 重入堆,不等 poll 重新确认
	restoreKeyStateSnapshots(pools, cfg, logger)
	r := router.NewRouter(logger, manager, pools, router.Config{
		Aliases:         toRouterAliases(cfg.Routing.Aliases, cfg.Routing.Chains),
		DefaultStrategy: cfg.Routing.DefaultStrategy,
		CatchAll:        toRouterCatchAll(cfg.Routing.CatchAll, cfg.Routing.Chains),
	})
	// P-per-key-circuit: 熔断器已下沉到 keypool(per-key)。
	// 2026-08-06 之前是 per-provider — 一把 key 5 个 5xx 连坐整 provider 的
	// healthy key(实测:weige 出问题,key-1 一起被跳过,全链掉 deepseek)。
	// 现在每把 key 独立熔断,配置从 provider config 传入 pool(buildKeyPools)

	// P7: Authenticator(从 DB 加载;config keys 在启动时被 seed 到 DB)
	var authn *auth.Authenticator
	if cfg.Auth.Enabled {
		// 把 config 里的 keys seed 到 DB
		gkKeys := make([]auth.GatewayKey, 0, len(cfg.Auth.Keys))
		for _, k := range cfg.Auth.Keys {
			gkKeys = append(gkKeys, auth.GatewayKey{
				Name:          k.Name,
				KeyHash:       k.Key,
				AllowedModels: k.AllowedModels,
				RateLimit:     auth.RateLimitConfig{RPM: k.RateLimit.RPM, TPM: k.RateLimit.TPM},
			})
		}
		if err := auth.SeedFromConfig(context.Background(), db, gkKeys); err != nil {
			logger.Warn("seed keys from config failed", zap.Error(err))
		}
		// 从 DB 加载所有 keys(含 config seed + UI 添加的)
		dbKeys, err := auth.LoadFromDB(context.Background(), db)
		if err != nil {
			logger.Warn("load keys from DB failed", zap.Error(err))
			dbKeys = gkKeys // fallback to config
		}
		authn = auth.New(dbKeys)
		logger.Info("auth enabled", zap.Int("keys", len(dbKeys)))
	}

	// P8: Usage Collector + Metrics Collector
	usageC := usage.NewCollector(db, cfg.Usage.BatchSize, int(cfg.Usage.FlushInterval.Milliseconds()))
	usageRepo := usage.NewRepository(db)
	metricsC := metrics.NewCollector()

	// P67: AccessLog Recorder(接入日志模块)
	//   - enabled=false:返回 no-op Recorder,proxy 所有调用都静默 no-op
	//   - zero value 字段用 default 兜底
	accessCfg := accesslog.RecorderConfig{
		Enabled:       cfg.Server.AccessLog.Enabled,
		BodyDir:       cfg.Server.AccessLog.BodyDir,
		BufferSize:    cfg.Server.AccessLog.BufferSize,
		BatchSize:     cfg.Server.AccessLog.BatchSize,
		FlushInterval: cfg.Server.AccessLog.FlushInterval,
		Retention:     cfg.Server.AccessLog.Retention,
	}
	if accessCfg.BodyDir == "" {
		accessCfg.BodyDir = config.DefaultAccessLogBodyDir
	}
	if accessCfg.BufferSize == 0 {
		accessCfg.BufferSize = config.DefaultAccessLogBufferSize
	}
	if accessCfg.BatchSize == 0 {
		accessCfg.BatchSize = config.DefaultAccessLogBatchSize
	}
	if accessCfg.FlushInterval == 0 {
		accessCfg.FlushInterval = config.DefaultAccessLogFlushInterval
	}
	if accessCfg.Retention == 0 {
		accessCfg.Retention = config.DefaultAccessLogRetention
	}
	accessR, err := accesslog.NewRecorder(accessCfg, db, logger)
	if err != nil {
		return nil, fmt.Errorf("accesslog new: %w", err)
	}

	// 设备指纹归一化:启动时翻一次开关 + Capture 一次网关环境。
	// fpEnabled 同时被 engine 闭包与 server 持有 —— PATCH /api/v1/fingerprint 翻转即热切换。
	fpEnabled := &atomic.Bool{}
	fpEnabled.Store(cfg.Fingerprint.FingerprintEnabled())
	fpSnap := fingerprint.Capture(cfg.Fingerprint.CanonicalDeviceID)

	// P-inflight: 活跃请求内存快照表(proxy 写、admin handler 读,都走窄接口)
	inflightR := inflight.NewRegistry()

	eng := proxy.NewEngine(proxy.Config{
		Router:        r,
		Logger:        logger,
		Usage:         usage.NewAdapter(usageC),
		Metrics:       metrics.NewAdapter(metricsC),
		TokenRecorder: newAuthTokenRecorder(authn), // P13: TPM 计数(若 auth 启用)
		Authenticator: authn,                       // P19: Provider 绑定检查
		AccessLog:     accessR,                     // P67: 接入日志
		Inflight:      inflightR,                   // P-inflight: 活跃请求快照
		// P-quota-checker: 注入 quotacheck 探测 (proxy 不直接依赖 quotacheck 包)
		QuotaChecker: proxy.CheckQuotaFunc(func(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error) {
			return quotacheck.CheckQuota(ctx, providerName, baseURL, k)
		}),
		// 设备指纹归一化(热切换):闭包每次请求读 fpEnabled,off 时原样透传。
		FingerprintSanitizer: fingerprintSanitizer(fpEnabled, fpSnap),
		MaxRetry:             cfg.Retry.MaxAttempts,
		// 流式写 deadline 续期预算 — 与 http.Server.WriteTimeout 同源,
		// 流式场景下按 chunk 续期成空闲超时(非流式仍是绝对上限)
		WriteTimeout: cfg.Server.WriteTimeout,
		// P-stream-idle-timeout: 流式空闲超时(连续 N 秒没收到 chunk → 认为断流)
		// 默认 10s,快速失败让客户端重试(不等 provider.timeout 60s)
		StreamIdleTimeout: 10 * time.Second,
	})
	// P30:把 DB Pool 注入到每个 Provider(Manager.LoadFromConfig 时 Pool 还是 nil)
	injectPools(manager, pools, logger)

	// Task 6:从 DB provider_models 读 pricing/defaultModels 注入 manager(首次启动)。
	// 必须在 injectPools 之后、LoadFromConfig 已在 main.go 跑过(先于 server.New),
	// 此时 m.providers 已就绪,LoadModelsFromStore 遍历 m.providers 算 defaultModels 正确。
	modelStoreAdapter := providerModelStoreAdapter{s: database.NewProviderModelStore(db)}
	if err := manager.LoadModelsFromStore(context.Background(), modelStoreAdapter); err != nil {
		logger.Warn("load models from store failed (pricing/defaultModels empty)", zap.Error(err))
	}

	// P68: 构造 quotacheck.Manager(quota restore worker)
	endpoints := make(map[string]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if p.Enabled && p.Endpoint != "" {
			endpoints[name] = p.Endpoint
		}
	}
	quotaCfg := quotacheck.ManagerConfig{
		Enabled:           cfg.KeyPool.QuotaEnabled,
		ProbeInitialDelay: cfg.KeyPool.QuotaProbeInitialDelay,
		ProbeMaxBackoff:   cfg.KeyPool.QuotaProbeMaxBackoff,
		ProbeJitterPct:    cfg.KeyPool.QuotaProbeJitterPct,
		PollInterval:      cfg.KeyPool.QuotaPollInterval,
		PollJitterPct:     cfg.KeyPool.QuotaPollJitterPct,
		HTTPTimeout:       cfg.KeyPool.QuotaHTTPTimeout,
		UserAgent:         cfg.KeyPool.QuotaUserAgent,
		WarnThresholdPct:  cfg.KeyPool.QuotaWarnThresholdPct,
	}
	quotaM := quotacheck.NewManager(logger, quotacheck.NewPoolsRef(pools), &quotacheck.StaticProviderLookup{Endpoints: endpoints}, metricsC, quotaCfg)

	// P-admin-auth: 管理员认证管理器(若启用)
	var adminAuthM *adminauth.Manager
	if cfg.AdminAuth.Enabled {
		adminAuthM = adminauth.NewManager(db, adminauth.Config{
			SessionTTL:        cfg.AdminAuth.SessionTTL,
			MaxFailedAttempts: cfg.AdminAuth.MaxLoginAttempts,
			LockDuration:      cfg.AdminAuth.LoginBanDuration,
		})
		logger.Info("admin auth enabled")
	}

	s := &Server{
		cfg:               cfg,
		logger:            logger,
		db:                db,
		manager:           manager,
		modelStoreAdapter: modelStoreAdapter,
		router:            r,
		engine:            eng,
		pools:             pools,
		auth:              authn,
		usageC:            usageC,
		usageR:            usageRepo,
		metricsC:          metricsC,
		accessR:           accessR,
		quotaM:            quotaM,
		inflightR:         inflightR,
		adminAuthM:        adminAuthM,
		fpEnabled:         fpEnabled,
		fpSnapshot:        fpSnap,
	}
	// P-route-order: 启动时把 route_order 的 Level 2 provider 改写加载进 router
	s.reloadProviderOrder()
	return s, nil
}

// providerModelStoreAdapter 把 database.ProviderModelStore 适配成 provider.ModelStore。
// provider 包不 import database(依赖倒置),由 server(顶层编排)把 database 的
// ProviderModel 投影成 provider.DBModelRow(厂商粒度)。
type providerModelStoreAdapter struct {
	s database.ProviderModelStore
}

func (a providerModelStoreAdapter) All(ctx context.Context) ([]provider.DBModelRow, error) {
	rows, err := a.s.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.DBModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, provider.DBModelRow{
			Vendor:                  r.Vendor,
			ModelID:                 r.ModelID,
			CostPerMillionInput:     r.CostPerMillionInput,
			CostPerMillionCacheRead: r.CostPerMillionCacheRead,
			CostPerMillionOutput:    r.CostPerMillionOutput,
		})
	}
	return out, nil
}

func (a providerModelStoreAdapter) ListByVendor(ctx context.Context, vendor string) ([]provider.DBModelRow, error) {
	rows, err := a.s.ListByVendor(ctx, vendor)
	if err != nil {
		return nil, err
	}
	out := make([]provider.DBModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, provider.DBModelRow{
			Vendor:                  r.Vendor,
			ModelID:                 r.ModelID,
			CostPerMillionInput:     r.CostPerMillionInput,
			CostPerMillionCacheRead: r.CostPerMillionCacheRead,
			CostPerMillionOutput:    r.CostPerMillionOutput,
		})
	}
	return out, nil
}

// AllFaces 投影 database.ProviderModelFace → provider.DBFaceRow(注册面粒度)。
// P-model-face: 面→模型归属,让中转站厂商各后缀端点的模型清单互相隔离。
func (a providerModelStoreAdapter) AllFaces(ctx context.Context) ([]provider.DBFaceRow, error) {
	rows, err := a.s.AllFaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.DBFaceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, provider.DBFaceRow{
			Vendor:    r.Vendor,
			Face:      r.Face,
			ModelID:   r.ModelID,
			SortOrder: r.SortOrder,
		})
	}
	return out, nil
}

// P30:把 buildKeyPools 读出来的 Pool 注入到每个 Provider
// Manager.LoadFromConfig 时 Pool 还是 nil(那时 DB 还没读),
// 启动后 Server.New 再注入
func injectPools(manager *provider.Manager, pools map[string]*keypool.Pool, logger *zap.Logger) {
	for name, p := range manager.GetAll() {
		pool, ok := pools[name]
		if !ok {
			continue
		}
		// SetPool 已在 Provider 接口里(编译期强制),直接调用,不再可选 type-assert
		p.SetPool(pool)
		logger.Info("pool injected", zap.String("provider", name))
	}
}

// poolFor 读 s.pools[name](F4 竞态修复:poolsMu RLock,与 ReloadProviderPool 的
// s.pools=newPools 写同步;admin list 闭包用它替代裸 s.pools[providerName] 读)。
func (s *Server) poolFor(providerName string) (*keypool.Pool, bool) {
	s.poolsMu.RLock()
	defer s.poolsMu.RUnlock()
	pool, ok := s.pools[providerName]
	return pool, ok
}

// poolSnapshot 返回全部最新 pools 的浅拷贝(供 admin 注入,读路加锁)。
// 浅拷贝够用:map 里的 *keypool.Pool 是共享指针,读方只需「拿到最新 map 的键值快照」;
// ReloadProviderPool 会整体替换 s.pools(而非就地改某条目),浅拷贝没有迭代中并发写风险。
func (s *Server) poolSnapshot() map[string]*keypool.Pool {
	s.poolsMu.RLock()
	defer s.poolsMu.RUnlock()
	out := make(map[string]*keypool.Pool, len(s.pools))
	for k, v := range s.pools {
		out[k] = v
	}
	return out
}

// buildKeyPools 为每个 enabled Provider 构造一个 KeyPool
// P30:key 从 DB (provider_api_keys 表) 读,不用 config.yaml
func buildKeyPools(cfg *config.Config, db *gorm.DB, logger *zap.Logger) map[string]*keypool.Pool {
	// P-mimo-quota: MIMO 控制台 cookie 启动注入(套餐/余额查询端点用 cookie 鉴权,
	// 非 API key;约 1 天过期,过期后轮询退化保守)。
	// 优先级:DB 持久化值(API 更新过)> config bootstrap(首次写入 DB)。
	// 之后日常更新走 POST /api/v1/providers/mimo/quota-cookie,不用改 config 重启。
	ctx := context.Background()
	cookieStore := auth.NewMimoQuotaCookieStore(db)
	if row, err := cookieStore.Get(ctx); err == nil && row != nil && row.Cookie != "" {
		mimo.SetQuotaCookie(row.Cookie)
	} else if m := cfg.Providers["mimo"]; m.QuotaCookie != "" {
		mimo.SetQuotaCookie(m.QuotaCookie)
		if err := cookieStore.Upsert(ctx, m.QuotaCookie); err != nil {
			logger.Warn("persist mimo quota cookie bootstrap failed", zap.Error(err))
		}
	}
	out := make(map[string]*keypool.Pool)
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	store := auth.NewProviderKeyStore(db)
	// P-provider-vendor: 同一 vendor 的多个注册名(如 deepseek / deepseek-anthropic)
	// 共享同一个 pool — key 厂商级一份,协议由 key 的 Protocols 标记过滤
	vendorPools := make(map[string]*keypool.Pool)
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		vendor := provider.Default().VendorFor(name)
		pool, ok := vendorPools[vendor]
		if !ok {
			poolCfg := toPoolCfg(cfg, vendor, logger)
			pool = buildOnePool(context.Background(), vendor, sched, poolCfg, store, logger,
				loadKeyOrder(db, vendor))
			vendorPools[vendor] = pool
		}
		out[name] = pool
	}
	return out
}

// buildProviderPool 为单个 vendor 构建 KeyPool（中转站热重载时使用）
func buildProviderPool(cfg *config.Config, db *gorm.DB, vendor string, logger *zap.Logger) *keypool.Pool {
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	store := auth.NewProviderKeyStore(db)
	poolCfg := toPoolCfg(cfg, vendor, logger)
	return buildOnePool(context.Background(), vendor, sched, poolCfg, store, logger,
		loadKeyOrder(db, vendor))
}

// toPoolCfg 单一来源:构造 provider pool 的 keypool.Config。
// 低耦合修复:startup(buildKeyPools)与热重载(ReloadProviderPool)此前各自组装
// poolCfg —— 重载路径只设 CoolingDuration,丢了 BreakerFactory(per-key 熔断)
// 和 balancer-less vendor 的 probe 配额模式,导致管理员增删 key 后:
//  1. 该 provider 熔断静默失效;2. glm/qwen/gemini 从 probe(安全)变 poll(永久死 key)。
//
// 统一到这里,两条路径永远用同一份配置。cfg 由调用方传入(buildKeyPools 与
// ReloadProviderPool 都拿得到),不绑 Server 实例。
func toPoolCfg(cfg *config.Config, vendor string, logger *zap.Logger) keypool.Config {
	poolCfg := keypool.Config{
		CoolingDuration: cfg.KeyPool.CoolingDuration,
		// P-per-key-circuit: per-key 熔断器配置(取该 vendor 第一个注册名的,
		// 同一 vendor 共享 pool 时配置一致);0 = 不启用
		BreakerFactory: toBreakerFactory(providerCircuitCfg(cfg, vendor), vendor, logger),
	}
	// B-probe-quota: 该 vendor 没有任何注册名有余额查询 balancer
	// (glm / qwen / gemini)→ probe 模式:配额耗尽不永久标记,每次请求
	// 重新探测(充值即恢复);有 balancer 的(deepseek / minimax)→
	// 默认 poll,由 quotacheck 轮询恢复
	if !vendorHasBalancer(vendor) {
		poolCfg.QuotaRecovery = keypool.QuotaRecoveryProbe
	}
	return poolCfg
}

// providerCircuitCfg 取该 vendor 第一个已启用注册名的熔断配置(共享 pool 时一致)
func providerCircuitCfg(cfg *config.Config, vendor string) config.CircuitBreakerCfg {
	var c config.CircuitBreakerCfg
	for name, p := range cfg.Providers {
		if !p.Enabled || provider.Default().VendorFor(name) != vendor {
			continue
		}
		c = p.CircuitBreaker
		break
	}
	return c
}

// toBreakerFactory P-per-key-circuit: config 的熔断配置 → BreakerFactory 注入 Pool。
// 配置了 failure_threshold > 0 才启用(0 = 不启用,测试/默认场景)
// Pool 通过工厂方法拿到熔断器,keypool 包不再直接 import circuit
func toBreakerFactory(c config.CircuitBreakerCfg, vendorPrefix string, logger *zap.Logger) keypool.BreakerFactory {
	if c.FailureThreshold <= 0 {
		return nil // 不启用
	}
	cbCfg := circuit.Config{
		FailureThreshold: c.FailureThreshold,
		FailureWindow:    c.FailureWindow,
		OpenTimeout:      c.OpenTimeout,
		HalfOpenRequests: c.HalfOpenRequests,
		CountableErrors:  c.CountableErrors,
		ExcludedErrors:   c.ExcludedErrors,
	}
	return func(keyID string) keypool.Breaker {
		br := circuit.New(vendorPrefix+"/"+keyID, cbCfg)
		br.SetLogger(logger)
		return &circuitBreakerAdapter{br: br}
	}
}

// circuitBreakerAdapter 适配 circuit.Breaker → keypool.Breaker 接口
type circuitBreakerAdapter struct {
	br *circuit.Breaker
}

func (a *circuitBreakerAdapter) Allow() bool {
	return a.br.Allow()
}

func (a *circuitBreakerAdapter) RecordSuccess() {
	a.br.RecordSuccess()
}

func (a *circuitBreakerAdapter) RecordFailure(errType string) {
	a.br.RecordFailure(errType)
}

func (a *circuitBreakerAdapter) State() string {
	return string(a.br.State())
}

// vendorHasBalancer 该 vendor 的任意注册名是否注册了余额查询 balancer
// (balancer = 有官方余额接口 → quotacheck 可以轮询恢复)
func vendorHasBalancer(vendor string) bool {
	for name, info := range provider.Default().ListRegisteredInfo() {
		if info.Vendor == vendor && quotacheck.LookupBalancer(name) != nil {
			return true
		}
	}
	return false
}

// buildOnePool P35: 给单个 provider 从 DB 构造 Pool
// 启动时全量构造、运行时热更新都用它
// keyOrder P-route-order(2026-08-10,Level 3):key 名 → 改写位次(小在前),nil/空 = 用默认
// (created_at/id 顺序)。非空时按改写序稳定排序,未列出的 key 保持默认序排后。
func buildOnePool(ctx context.Context, name string, sched keypool.Scheduler, poolCfg keypool.Config, store auth.ProviderKeyStore, logger *zap.Logger, keyOrder map[string]int) *keypool.Pool {
	rows, err := store.List(ctx, name)
	if err != nil {
		logger.Warn("read provider keys from DB failed",
			zap.String("provider", name),
			zap.Error(err))
		return keypool.NewPool(name, nil, sched, poolCfg)
	}
	if len(rows) == 0 {
		logger.Warn("provider has no API keys in DB",
			zap.String("provider", name))
		return keypool.NewPool(name, nil, sched, poolCfg)
	}
	keys := make([]*keypool.Key, 0, len(rows))
	for _, row := range rows {
		if !database.IsEnabled(row.Enabled) {
			continue
		}
		bs := row.BillingSource
		if bs == "" {
			bs = "api" // 兜底
		}
		keys = append(keys, &keypool.Key{
			ID:            fmt.Sprintf("%d", row.ID),
			ProviderName:  name,
			Name:          row.Name,
			Key:           row.KeyHash,
			Status:        keypool.KeyStatusActive,
			BillingSource: bs, // P48: 单 key 计费 tier,Pool.Acquire 按此排序
			// P-provider-vendor: key 可用协议列表(空 = 全部),取 key 时按请求协议过滤
			Protocols: row.Protocols,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	// P-route-order(Level 3):有改写时按改写序稳定排;未列出的 key 按原(创建)序排后。
	if len(keyOrder) > 0 {
		rank := func(name string) (int, bool) { v, ok := keyOrder[name]; return v, ok }
		sort.SliceStable(keys, func(i, j int) bool {
			si, oki := rank(keys[i].Name)
			sj, okj := rank(keys[j].Name)
			switch {
			case oki && okj:
				return si < sj
			case oki:
				return true // 有改写者在前
			default:
				return false // 无改写者保持原序(排后)
			}
		})
	}
	logger.Info("keypool built from DB",
		zap.String("provider", name),
		zap.Int("keys", len(keys)),
	)
	return keypool.NewPool(name, keys, sched, poolCfg)
}

// loadKeyOrder P-route-order(2026-08-10,Level 3):从 route_order 表读某 provider 内
// key 排序改写,name → seq。无 db / 读失败 → 空 map(用默认创建序)。keypool 无用例级错误。
func loadKeyOrder(db *gorm.DB, provider string) map[string]int {
	if db == nil {
		return nil
	}
	rows, err := database.NewRouteOrderStore(db).ListByScope(context.Background(), "key", provider, "")
	if err != nil {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	m := make(map[string]int, len(rows))
	for _, r := range rows {
		m[r.Name] = r.Seq
	}
	return m
}

// reloadProviderOrder P-route-order(2026-08-10):从 route_order 表读 scope=provider 的
// Level 2 改写,热更新到 router 的 ProviderOrder。启动时 + PUT /routing/order 后调用。
// 读失败(表未建等)→ 记日志但不崩溃,沿用默认排序(最早 key 时间)。
func (s *Server) reloadProviderOrder() {
	if s.router == nil || s.db == nil {
		return
	}
	store := database.NewRouteOrderStore(s.db)
	rows, err := store.ListByScope(context.Background(), "provider", "", "")
	if err != nil {
		s.logger.Warn("load provider order failed, keep default",
			zap.Error(err), zap.String("scope", "provider"))
		return
	}
	// ProviderOrder 分两层存储:外层 key = 计费层(token_plan/api),内层 = vendor → seq。
	// route_order 表的 provider 作用域本就按 billing_source 分层(同一 vendor 在
	// token_plan 与 api 各自独立排序)。若平铺成 map[vendor]seq,后读的那层会覆盖另一层
	// (mimo token_plan=0 被 api=1 覆盖 → token_plan 层 mimo 优先失效,2026-08-10 根因)。
	// 必须保持分层,router 按候选面的 billing_source 查对应层。
	m := make(map[string]map[string]int, 4)
	for _, r := range rows {
		bs := r.BillingSource
		if m[bs] == nil {
			m[bs] = make(map[string]int, len(rows))
		}
		m[bs][r.Name] = r.Seq
	}
	s.router.SetProviderOrder(m)
	s.logger.Info("route_order: provider order updated",
		zap.Int("layers", len(m)))
}

// toManagerConfigForReload 把 config 投影成 ManagerConfig(只用于 ReloadPricing,不需要 Pools)
func toManagerConfigForReload(cfg *config.Config, pools map[string]*keypool.Pool) *provider.ManagerConfig {
	mcfg := &provider.ManagerConfig{
		Providers: make(map[string]provider.ManagerProviderConfig, len(cfg.Providers)),
		Pools:     make(map[string]*keypool.Pool, len(pools)),
		// P-provider-timeout: 热重载同频带全局默认(provider.timeout==0 时兜底)
		DefaultTimeout: cfg.Timeouts.ProviderDefault,
	}
	for name, pool := range pools {
		mcfg.Pools[name] = pool
	}
	for name, p := range cfg.Providers {
		proto, _ := provider.ParseProtocol(p.Protocol)
		mcfg.Providers[name] = provider.ManagerProviderConfig{
			Enabled:  p.Enabled,
			Endpoint: p.Endpoint,
			Protocol: proto,
			Timeout:  p.Timeout,
			// P47: 计费来源 — 热重载时也带上
			BillingSource: defaultBillingSource(p.BillingSource),
			// P-responses: Responses API 能力 — 热重载时同步
			ResponsesAPI: p.ResponsesAPI,
			// P-deepseek-thinking: 强制 thinking=disabled — 热重载时同步
			ForceThinkingDisabled: p.ForceThinkingDisabled,
		}
	}
	return mcfg
}

// defaultBillingSource 同 defaultStr,但专门给 BillingSource 用(语义清晰)
func defaultBillingSource(s string) string {
	if s == "" {
		return "api"
	}
	return s
}

// toRouterAliases 把 config 风格别名表转 router 风格
// P39: chain_ref → 从 chains map 展开成 providers
// P53: TargetModel → 短格式(留空 Providers,Router 走 auto-discovery)
func toRouterAliases(in map[string]config.AliasRule, chains map[string][]config.AliasRoute) map[string]router.AliasConfig {
	out := make(map[string]router.AliasConfig, len(in))
	for alias, rule := range in {
		// 决定实际使用的 provider 列表
		var src []config.AliasRoute
		switch {
		case rule.ChainRef != "":
			if chain, ok := chains[rule.ChainRef]; ok {
				src = chain
			}
			// chain_ref 找不到 → src 留空,Router 走 auto-discovery
		case rule.TargetModel != "":
			// P53: 短格式 — TargetModel 模式下,src 留空,Router 会从所有
			// 声明该 model 的 provider 中自动发现
			_ = rule.TargetModel // intentionally not expanded here
		default:
			src = rule.Providers
		}

		ps := make([]router.ProviderRoute, 0, len(src))
		for _, p := range src {
			ps = append(ps, router.ProviderRoute{
				Name: p.Name, Model: p.Model, Priority: p.Priority, Weight: p.Weight,
			})
		}
		out[alias] = router.AliasConfig{
			Alias:       alias,
			Strategy:    rule.Strategy,
			Providers:   ps,
			TargetModel: rule.TargetModel, // P53: 短格式标记
		}
	}
	return out
}

// toRouterCatchAll P-catch-all: 把 routing.catch_all 配置转成 router.AliasConfig。
// 规则结构与 alias 完全一致(长格式 providers / 短格式 target_model),
// 与 toRouterAliases 同一套转换逻辑
func toRouterCatchAll(rule *config.AliasRule, chains map[string][]config.AliasRoute) *router.AliasConfig {
	if rule == nil {
		return nil
	}
	var src []config.AliasRoute
	switch {
	case rule.ChainRef != "":
		src = chains[rule.ChainRef] // 找不到 → 空,Router 走 auto-discovery
	case rule.TargetModel != "":
		// 短格式:src 留空,Router 自动发现声明该 model 的 provider
		_ = rule.TargetModel
	default:
		src = rule.Providers
	}
	ps := make([]router.ProviderRoute, 0, len(src))
	for _, p := range src {
		ps = append(ps, router.ProviderRoute{
			Name: p.Name, Model: p.Model, Priority: p.Priority, Weight: p.Weight,
		})
	}
	return &router.AliasConfig{
		Alias:       "*", // catch-all 的别名占位名(展示用)
		Strategy:    rule.Strategy,
		Providers:   ps,
		TargetModel: rule.TargetModel,
	}
}

// Run 启动 HTTP 服务
func (s *Server) Run(ctx context.Context) error {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	s.registerRoutes(r)

	// P8: 启动 Usage Collector 后台落库协程
	s.usageC.Start(ctx)
	// P67: 启动 AccessLog Recorder(async buffer + retention)
	if s.accessR != nil {
		s.accessR.Start(ctx)
	}
	// P68: 启动 quota restore worker(probing + polling)
	if s.quotaM != nil {
		// 注入关闭根 ctx:热重载(Reload)重新 Start 的 worker 也从它派生,
		// 保证随进程关闭终止(不再泄漏)。
		s.quotaM.SetShutdownCtx(ctx)
		s.quotaM.Start(ctx)
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.http = &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  s.cfg.Server.ReadTimeout,
		WriteTimeout: s.cfg.Server.WriteTimeout,
		IdleTimeout:  s.cfg.Server.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("gateway listening", zap.String("addr", addr))
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received")
		// P-quota-worker: 停止 quota restore / 轮询 worker(否则热重载重启的
		// goroutine 不受控地泄漏,跨进程完全退出)。
		// 放在排空之前:它是后台生产者,不是请求路径的下游消费者,提前停掉
		// 只是少发几轮探测,不会影响在飞请求的落库。
		if s.quotaM != nil {
			s.quotaM.Stop()
		}

		// 顺序要紧(2026-08-26 修 panic + 静默丢数据):
		// 先排空 HTTP,**再**停 usage / accesslog 这两个下游消费者。
		// 原先两者都在 shutdown() 之前关,于是排空窗口里完成的请求:
		//   - accesslog 往已关的 channel 发 → panic(gin Recovery 兜住,但该请求返 500)
		//   - usage 往已停的 collector 发 → 静默丢用量
		// 也就是说关停瞬间在飞的请求既拿不到正确响应也不留账。
		shutdownErr := s.shutdown()

		s.usageC.Stop() // flush 剩余记录
		if s.accessR != nil {
			_ = s.accessR.Close() // flush buffer + stop retention
		}
		// P-state-persist: 排空完成(在飞请求全部结束)后写快照 — 状态最准。
		// reload(SIGTERM)不丢 QE/COOLING/余额,重启后无需 poll 重新确认 2 轮
		s.saveKeyStateSnapshot()
		return shutdownErr
	case err := <-errCh:
		// ListenAndServe 意外退出(端口被占等)。这条分支也要收尾:
		// accesslog worker 现在只由 Close() 终止(不再听 ctx.Done()),
		// 直接 return 会把它连同已接收未落库的条目一起留在原地。
		if s.quotaM != nil {
			s.quotaM.Stop()
		}
		s.usageC.Stop()
		if s.accessR != nil {
			_ = s.accessR.Close()
		}
		if err != nil {
			return fmt.Errorf("http server error: %w", err)
		}
		return nil
	}
}

// shutdown 优雅关闭
func (s *Server) shutdown() error {
	timeout := s.cfg.Server.ShutdownTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.logger.Info("graceful shutdown", zap.Duration("timeout", timeout))
	return s.http.Shutdown(ctx)
}

// keyStateSnapshotPath 快照文件路径。
// SQLite:与 DB 同目录(/tmp/gateway-data/,reload 保留;机器重启 /tmp 清空
// 一起丢,可接受,poll 重新学习)。
// PG:dsn 是 URL,filepath.Dir 会拆出 "postgres:/..." 怪路径(目录不存在,
// 快照静默写失败 → 重启丢 QE/COOLING 状态)→ 统一落 cwd(进程启动目录,
// systemd 下为仓库根,持久)。
func keyStateSnapshotPath(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return filepath.Join(".", "key-state.json")
	}
	return filepath.Join(filepath.Dir(dsn), "key-state.json")
}

// saveKeyStateSnapshot P-state-persist: 把所有 pool 的 key 运行时状态落盘。
// 原子写(临时文件 + rename)避免半截文件;只导状态,不含明文 key
func (s *Server) saveKeyStateSnapshot() {
	path := keyStateSnapshotPath(s.cfg.Database.DSN)
	states := make([]keypool.KeyState, 0, 8)
	// P-provider-vendor: 同一 vendor 的多个注册名共享同一 pool — 按指针去重,
	// 避免 deepseek / deepseek-anthropic 各导出一次(快照文件冗余)
	seen := make(map[*keypool.Pool]bool)
	for _, pool := range s.pools {
		if seen[pool] {
			continue
		}
		seen[pool] = true
		states = append(states, pool.Snapshot()...)
	}
	data, err := json.Marshal(states)
	if err != nil {
		s.logger.Warn("key state snapshot marshal failed", zap.Error(err))
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.logger.Warn("key state snapshot write failed", zap.Error(err))
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.logger.Warn("key state snapshot rename failed", zap.Error(err))
		return
	}
	s.logger.Info("key state snapshot saved",
		zap.String("path", path), zap.Int("keys", len(states)))
}

// restoreKeyStateSnapshots P-state-persist: 启动时从快照恢复 key 状态。
// 必须在 quotacheck.Start(rescanExisting)之前执行 — QE key 恢复后冷启动
// rescan 会立即入堆探测恢复,不用等 poll 重新确认 2 轮(重启后耗尽 key
// 立即跳过,不再被打 429 → COOLING 60s 循环)
func restoreKeyStateSnapshots(pools map[string]*keypool.Pool, cfg *config.Config, logger *zap.Logger) {
	path := keyStateSnapshotPath(cfg.Database.DSN)
	data, err := os.ReadFile(path)
	if err != nil {
		return // 无快照(首次启动 / 机器重启)— 正常
	}
	var states []keypool.KeyState
	if err := json.Unmarshal(data, &states); err != nil {
		logger.Warn("key state snapshot parse failed", zap.Error(err))
		return
	}
	restored := 0
	// snapshot 的 ProviderName = pool 名(vendor),pools map 里有该注册名
	for _, st := range states {
		pool, ok := pools[st.ProviderName]
		if !ok {
			continue
		}
		pool.ApplySnapshot([]keypool.KeyState{st})
		restored++
	}
	logger.Info("key state snapshot restored",
		zap.String("path", path), zap.Int("keys", restored))
}

// registerRoutes 注册路由
func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"version": "0.5.0-p5",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	r.GET("/readyz", func(c *gin.Context) {
		if s.db == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_not_initialized"})
			return
		}
		sqlDB, err := s.db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_handle_error", "error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
		defer cancel()
		if err := sqlDB.PingContext(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// P8: /metrics(Prometheus 格式)
	r.GET("/metrics", gin.WrapH(s.metricsC.Handler()))

	// P12: 管理 API
	gkInfos := make([]handler.GatewayKeyInfo, 0, len(s.cfg.Auth.Keys))
	for _, k := range s.cfg.Auth.Keys {
		gkInfos = append(gkInfos, handler.GatewayKeyInfo{
			Name:          k.Name,
			AllowedModels: k.AllowedModels,
			RPM:           k.RateLimit.RPM,
			TPM:           k.RateLimit.TPM,
		})
	}
	admin := handler.NewAdmin(
		s.manager,
		provider.Default(),
		s.pools,
		s.router,
		s.usageR,
		toRouterAliases(s.cfg.Routing.Aliases, s.cfg.Routing.Chains),
		gkInfos,
		s.accessR,                          // P67: 接入日志 Recorder(可能为 no-op)
		s.quotaM,                           // P68 / P-quota-balance: quota 恢复 worker(可能为 nil)
		auth.NewMimoQuotaCookieStore(s.db), // P-mimo-quota: cookie 持久化(单行)
		// P-mimo-quota 解耦:处理器不 import provider/mimo,由 server(顶层编排者)
		// 把 vendor 专属校验/注入闭包传入 — 与 keyStatusLookup/quotaMarkFunc 同模式
		mimo.ValidateQuotaCookie,          // MimoQuotaValidate
		mimo.SetQuotaCookie,               // MimoQuotaSet
		database.NewRouteOrderStore(s.db), // P-route-order: Level 2/3 排序改写仓库(可 nil)
		func() { // P-route-order: PUT provider 顺序后热更新 router 的 Level 2 排序
			s.reloadProviderOrder()
		},
		func(provider string) { // P-route-order: PUT key 顺序后重载该 provider 的 pool(Level 3)
			s.ReloadProviderPool(provider)
		},
		// P-fingerprint: 设备指纹归一化查询/热切换闭包(handler 不依赖 server 状态)
		func() (bool, string) {
			if s.fpEnabled == nil {
				return false, ""
			}
			return s.fpEnabled.Load(), s.fpSnapshot.DeviceID
		},
		func(enabled bool) {
			if s.fpEnabled != nil {
				s.fpEnabled.Store(enabled)
				s.logger.Info("fingerprint sanitize toggled", zap.Bool("enabled", enabled))
			}
		},
		// P-inflight: 活跃请求快照只读闭包(handler 通过它读 proxy 写入的活跃请求)
		func() []*inflight.Snapshot { return s.inflightR.Snapshot() },
		// P-model-sync: 模型管理页三端点依赖。
		//   ModelStore = database.ProviderModelStore(UpsertModels/SavePricing/All,有写方法),
		//   与 ModelReload 读回内存用的 s.modelStoreAdapter(provider.ModelStore,只读)是两回事:
		//   sync 落库必须用 dbStore(提供 UpsertModels),manager 读回内存用 adapter。
		database.NewProviderModelStore(s.db),
		func(ctx context.Context, vendor string) ([]string, error) {
			return provider.SyncVendorModels(ctx, s.manager, vendor, database.NewProviderModelStore(s.db))
		},
		func(ctx context.Context) ([]provider.VendorSyncResult, error) {
			return provider.SyncAllVendorModels(ctx, s.manager, database.NewProviderModelStore(s.db))
		},
		func() error {
			return s.manager.LoadModelsFromStore(context.Background(), s.modelStoreAdapter)
		},
		// P-relay-station: 中转站管理 Store
		database.NewRelayStationStore(s.db),
		// P-relay-station: 中转站热重载函数
		func() error {
			if err := relay.ReloadFromDatabase(s.db, s.manager); err != nil {
				return err
			}
			// P-relay-independent: 重载后重建中转站的 KeyPool
			registry := provider.Default()
			for name := range s.manager.GetAll() {
				if registry.IsRelay(name) {
					s.ReloadProviderPool(name)
				}
			}
			return nil
		},
		// P-relay-cascade: 删站时按面清 provider_api_keys(和 relay 的 key 同步用同一张表)
		auth.NewProviderKeyStore(s.db),
	)
	// P-provider-vendor: 让 admin 动态读最新 pools —— ReloadProviderPool 会整体替换
	// s.pools(新 map)但不动 NewAdmin 拍下的旧 map 引用,导致「运行中加 key」的厂商
	// 在 /providers 显示陈旧零值。注入带锁读最新 s.pools 的闭包修正(见 poolFor/poolSnapshot)。
	admin.PoolLookup = s.poolFor
	admin.PoolsSnapshot = s.poolSnapshot

	// P-admin-auth: 管理端点鉴权(若启用)
	adminGroup := r.Group("/api/v1")
	if s.adminAuthM != nil {
		adminGroup.Use(middleware.AdminAuthMiddleware(s.adminAuthM))
	}

	// 登录/登出端点始终注册(即使未启用认证,也应返回明确错误而非 404)
	authHandler := handler.NewAdminAuthHandler(s.adminAuthM)
	authHandler.Register(r.Group("/api/v1"))

	admin.Register(adminGroup)

	// P16: Gateway Keys CRUD handler
	// 注意:CRUD 端点本身不要求 auth.enabled,这样即使没启用 auth 也能管理 keys
	// Authenticator 用一个 noop wrapper,把 Reload 调用变 no-op
	noopReload := func(keys []auth.GatewayKey) {
		if s.auth != nil {
			s.auth.Reload(keys)
		}
	}
	keysHandler := auth.NewKeysHandler(s.db, noopReload)
	keysHandler.Register(adminGroup)

	// P30: Provider API keys 管理(给已插件化的 Provider 加上游 LLM key)
	pkHandler := auth.NewProviderKeysHandler(s.db, s.ReloadProviderPool)
	// P68: 注入 status lookup,让 list endpoint 返回 key 运行时状态
	pkHandler.SetKeyStatusLookup(func(providerName, keyID string) string {
		pool, ok := s.poolFor(providerName)
		if !ok {
			return ""
		}
		for _, k := range pool.KeyPtrs() {
			if k.ID == keyID {
				return string(k.Status)
			}
		}
		return ""
	})
	// P-quota-balance: 注入 live key lookup,让 list endpoint 返回 Remaining / LastPolledAt
	pkHandler.SetPoolLookup(func(providerName, keyID string) (*keypool.Key, bool) {
		pool, ok := s.poolFor(providerName)
		if !ok {
			return nil, false
		}
		for _, k := range pool.KeyPtrs() {
			if k.ID == keyID {
				return k, true
			}
		}
		return nil, false
	})
	// P68: 注入 quota mark func(手动把 key 标 QUOTA_EXCEEDED)
	pkHandler.SetQuotaMarkFunc(func(providerName, keyID string) {
		pool, ok := s.poolFor(providerName)
		if !ok {
			return
		}
		for _, k := range pool.KeyPtrs() {
			if k.ID == keyID {
				pool.ReportQuotaExceeded(k)
				return
			}
		}
	})
	pkHandler.RegisterOn(adminGroup)

	// P5: 真代理接入
	// 注册具体协议路径 + NoRoute 兜底(覆盖其他 /v1/* 子路径)
	// P7: 当 auth.enabled=true 时,代理端点前挂 Auth + RateLimit 中间件
	proxyHandlers := []gin.HandlerFunc{}
	if s.auth != nil {
		proxyHandlers = append(proxyHandlers,
			middleware.AuthMiddleware(s.auth),
			middleware.RateLimitMiddleware(s.auth),
		)
	}
	proxyHandlers = append(proxyHandlers, s.engine.HandleRequest)

	r.POST("/v1/chat/completions", proxyHandlers...)
	r.POST("/v1/messages", proxyHandlers...)
	r.POST("/v1/completions", proxyHandlers...)
	// P-responses: OpenAI Responses API(Codex 客户端;Codex 的 base_url 直接
	// 打 /responses,不带 /v1 前缀,所以要注册两个路径)
	r.POST("/responses", proxyHandlers...)
	r.POST("/v1/responses", proxyHandlers...)
	// 流式请求也走 HandleRequest,Engine 内部从 body.stream 判断
	// 没匹配到的路径兜底(例如 /v1/embeddings 之类)
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && len(c.Request.URL.Path) > 4 && c.Request.URL.Path[:4] == "/v1/" {
			if s.auth != nil {
				middleware.AuthMiddleware(s.auth)(c)
				if c.IsAborted() {
					return
				}
				middleware.RateLimitMiddleware(s.auth)(c)
				if c.IsAborted() {
					return
				}
			}
			s.engine.HandleRequest(c)
			return
		}
		// P-web-static(方案 B):Go 进程直接托管前端构建产物,不依赖 nginx。
		// 已注册路由(/api、/v1、/healthz、/readyz、/admin、/metrics)天然不经过这里
		if s.webStatic(c) {
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"type": "not_found", "message": "no route for " + c.Request.URL.Path,
		}})
	})
}

// webStatic 托管前端构建产物(P-web-static 方案 B:Go 进程直接托管,无 nginx)。
//
// 挂在 NoRoute 兜底:只处理「gin 未注册的未知路径」。语义:
//   - static_dir 未配置 → 返回 false,调用方维持原 404 JSON 行为
//   - 只接管 GET/HEAD(其他方法返回 false,交给 404 JSON — 避免把 API 错误吞成页面)
//   - URL 含 ".." → 404(路径穿越防护;Clean 后其实已安全,双保险)
//   - 文件命中 → 返回文件(gin 自动识别 content-type)
//   - 未命中 → 返回 index.html(vue-router history 模式 SPA fallback)
func (s *Server) webStatic(c *gin.Context) bool {
	dir := s.cfg.Server.StaticDir
	if dir == "" {
		return false
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		return false
	}
	if strings.Contains(c.Request.URL.Path, "..") {
		c.AbortWithStatus(http.StatusNotFound) // AbortWithStatus 立即写 header(c.Status 是惰性的,无 body 时不会 flush)
		return true
	}
	// 归一化:以 / 开头 + Clean,保证 Join 结果仍在 staticDir 内
	p := path.Clean("/" + c.Request.URL.Path)
	fp := filepath.Join(dir, p)
	if rel, err := filepath.Rel(dir, fp); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		c.AbortWithStatus(http.StatusNotFound)
		return true
	}
	if st, err := os.Stat(fp); err == nil && !st.IsDir() {
		c.File(fp)
		return true
	}
	c.File(filepath.Join(dir, "index.html"))
	return true
}

var _ = database.Provider{} // keep database import alive

// authTokenRecorder 把 auth.Authenticator 适配到 proxy.TokenUsageRecorder
type authTokenRecorder struct {
	a *auth.Authenticator
}

func newAuthTokenRecorder(a *auth.Authenticator) *authTokenRecorder {
	if a == nil {
		return nil
	}
	return &authTokenRecorder{a: a}
}

func (r *authTokenRecorder) RecordUsage(keyID string, tokens int64) {
	r.a.RecordTokens(keyID, tokens)
}

// Reload 热重载 — 替换 Aliases / Auth Keys / Pricing
// 注意:Provider 实例 / KeyPool / Circuit Breaker 不在此函数内重载
// (它们的 Reload 已在 Manager 上,但涉及 HTTP 客户端重建,留给后续阶段)
func (s *Server) Reload(newCfg *config.Config) {
	if newCfg == nil {
		return
	}
	// P-reload-scfg: 同步 s.cfg = newCfg —— 否则 key-CRUD 触发的 ReloadProviderPool
	// (947/966/1012)会用旧快照(CoolingDuration/KeyRotation)重建 pool,与已 apply 的
	// quotaM.Reload 等 live 字段分歧。newCfg 已由 config.Watch 加载+validate。
	// 注意:Server/host/port/timeout/static_dir 等"需重启"字段读的是 New 时构好的
	// http.Server/s.cfg,此处赋值不影响它们(仍是重启生效,由下方 Warn 提示)。
	s.cfg = newCfg
	// Router aliases
	s.router.ReloadAliases(toRouterAliases(newCfg.Routing.Aliases, newCfg.Routing.Chains))
	// P-catch-all: 兜底路由与 aliases 同频热重载
	s.router.ReloadCatchAll(toRouterCatchAll(newCfg.Routing.CatchAll, newCfg.Routing.Chains))
	// 路由调度策略(default_strategy)同频热重载(此前会静默保留旧值)
	s.router.ReloadStrategy(newCfg.Routing.DefaultStrategy)

	// Manager 定价表(cost) — 不需要重建 Provider 实例,只刷 billingSource/responsesAPI
	s.manager.ReloadPricing(toManagerConfigForReload(newCfg, s.pools))
	// Task 6:pricing/defaultModels 改由 DB provider_models 刷(不再读 config models 段)。
	// 若这里漏掉,热重载后 pricing 仍是旧值(计费不刷新)。
	_ = s.manager.LoadModelsFromStore(context.Background(), s.modelStoreAdapter)

	// Authenticator — P51: 重载时必须从 DB 重新加载,不能只用 config keys
	// 否则通过 API 添加的 key 会在 config 热重载后失效
	if s.auth != nil && newCfg.Auth.Enabled {
		dbKeys, err := auth.LoadFromDB(context.Background(), s.db)
		if err != nil {
			s.logger.Warn("reload keys from DB failed", zap.Error(err))
		} else {
			s.auth.Reload(dbKeys)
		}
	}

	// P68: quota restore — 把新 cfg 传给 Manager,worker 会按 quota_enabled 启停
	if s.quotaM != nil {
		s.quotaM.Reload(quotacheck.ManagerConfig{
			Enabled:           newCfg.KeyPool.QuotaEnabled,
			ProbeInitialDelay: newCfg.KeyPool.QuotaProbeInitialDelay,
			ProbeMaxBackoff:   newCfg.KeyPool.QuotaProbeMaxBackoff,
			ProbeJitterPct:    newCfg.KeyPool.QuotaProbeJitterPct,
			PollInterval:      newCfg.KeyPool.QuotaPollInterval,
			WarnThresholdPct:  newCfg.KeyPool.QuotaWarnThresholdPct,
			PollJitterPct:     newCfg.KeyPool.QuotaPollJitterPct,
			HTTPTimeout:       newCfg.KeyPool.QuotaHTTPTimeout,
			UserAgent:         newCfg.KeyPool.QuotaUserAgent,
		})
	}

	s.logger.Info("config reloaded",
		zap.Int("aliases", len(newCfg.Routing.Aliases)),
		zap.Int("auth_keys", len(newCfg.Auth.Keys)),
	)
	// P-reload-restart-required: 明确提示哪些字段热重载不生效,需重启。
	// 避免 operator 改这些字段后以为 reload 已应用(silent partial apply)。
	s.logger.Warn("config hot-reload is partial — 需重启才生效的字段: database / server(host,port,timeouts,static_dir,access_log.*) / usage(batch_size,flush_interval) / providers(instance,pool,endpoint,timeout)")
}

// ReloadProviderPool P35: 从 DB 重建指定 provider 的 Pool,注入到 Provider
// providerName 为空时全量重建
// ProviderKeysHandler.Create/Delete 后会调这个
func (s *Server) ReloadProviderPool(providerName string) {
	sched := keypool.NewScheduler(s.cfg.KeyPool.KeyRotation)
	store := auth.NewProviderKeyStore(s.db)
	ctx := context.Background()

	if providerName != "" {
		vendor := provider.Default().VendorFor(providerName)
		// 找同 vendor 的所有已加载注册名
		var names []string
		for name := range s.manager.GetAll() {
			if provider.Default().VendorFor(name) == vendor {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			names = []string{providerName}
		}
		// 建一次 pool(vendor 名),重指该 vendor 的所有注册名。
		// poolCfg 与 startup 同一来源(toPoolCfg)—— 保留 BreakerFactory + probe
		// 配额模式,不让重载丢失熔断/回退成 poll 永久死 key
		poolCfg := toPoolCfg(s.cfg, vendor, s.logger)
		pool := buildOnePool(ctx, vendor, sched, poolCfg, store, s.logger, loadKeyOrder(s.db, vendor))
		// 低耦合修复:构建新 map(copy + 该 vendor 更新)后整表原子替换,
		// 不再就地写 s.pools —— 消除与 quotacheck poll 的 Get()(RLock 拷贝)
		// 并发 map 读写的进程崩溃。旧 map 永不就地变,读方持旧快照也安全。
		newPools := make(map[string]*keypool.Pool, len(s.pools))
		for k, v := range s.pools {
			newPools[k] = v
		}
		for i, name := range names {
			newPools[name] = pool
			// SetPool 已在 Provider 接口,直接调用(编译期强制,不再 type-assert)
			if pv, ok := s.manager.Get(name); ok {
				pv.SetPool(pool)
			}
			// P-provider-vendor: 共享 pool 每 vendor 只注入一次 quota callback —
			// 多个注册名指向同一 pool,每个名字都调会把 callback 绑定两次且
			// QUOTA_EXCEEDED key 双份入堆(双倍探测/提前 DISABLE)
			if s.quotaM != nil && i == 0 {
				s.quotaM.ReinjectCallback(name, pool)
			}
		}
		// 整表原子替换:server / router / quotacheck 三者都指到新 map
		s.poolsMu.Lock()
		s.pools = newPools
		s.poolsMu.Unlock()
		if s.router != nil {
			s.router.SetPools(newPools)
		}
		if s.quotaM != nil {
			s.quotaM.Pools().SwapPools(newPools)
		}
		s.logger.Info("provider pool reloaded", zap.String("vendor", vendor), zap.Int("keys", pool.Size()), zap.Strings("names", names))
		return
	}
	// 全量重建 — 按 vendor 分组,每组建一次 pool 重指该 vendor 所有注册名
	newPools := make(map[string]*keypool.Pool)
	vendorPools := make(map[string]*keypool.Pool) // 局部去重:vendor → pool
	// P-provider-vendor: 共享 pool 的 quota callback 每 pool 只注入一次(见单分支注释)
	injectedPools := make(map[*keypool.Pool]bool)
	for name, p := range s.cfg.Providers {
		if !p.Enabled {
			continue
		}
		vendor := provider.Default().VendorFor(name)
		pool, ok := vendorPools[vendor]
		if !ok {
			// 与 startup 同一配置来源(toPoolCfg)—— 保留熔断 + probe 配额模式
			pool = buildOnePool(ctx, vendor, sched, toPoolCfg(s.cfg, vendor, s.logger), store, s.logger, loadKeyOrder(s.db, vendor))
			vendorPools[vendor] = pool
		}
		newPools[name] = pool
		if pv, ok := s.manager.Get(name); ok {
			pv.SetPool(pool) // SetPool 在 Provider 接口,编译期强制
		}
		if s.quotaM != nil && !injectedPools[pool] {
			injectedPools[pool] = true
			s.quotaM.ReinjectCallback(name, pool)
		}
	}
	// 整表原子替换(不再就地写 s.pools,防并发 map 崩溃)
	s.poolsMu.Lock()
	s.pools = newPools
	s.poolsMu.Unlock()
	if s.router != nil {
		s.router.SetPools(newPools)
	}
	if s.quotaM != nil {
		s.quotaM.Pools().SwapPools(newPools)
	}
	s.logger.Info("all provider pools reloaded", zap.Int("providers", len(newPools)))
}

// fingerprintSanitizer 构造设备指纹归一化闭包。闭包捕获 fpEnabled(atomic 开关)
// 与 fpSnapshot(启动 Capture 一次),每次请求读一次开关:off 时原样返回(透传),on 时归一。
// 这样 PATCH /api/v1/fingerprint 翻转 fpEnabled 即热切换,无需重启、无需重捕快照。
func fingerprintSanitizer(enabled *atomic.Bool, snap fingerprint.Snapshot) func([]byte) []byte {
	return func(body []byte) []byte {
		if !enabled.Load() {
			return body
		}
		return fingerprint.Sanitize(body, snap)
	}
}
