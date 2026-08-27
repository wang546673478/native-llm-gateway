// Package provider — SyncVendorModels 单元测试
package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"go.uber.org/zap"
)

// fakeModelsProvider 最小 Provider 实现(有可配 protocol / models / ListModels 结果)
type fakeModelsProvider struct {
	name     string
	protocol Protocol
	models   []string
	listErr  error
}

func (p *fakeModelsProvider) Name() string       { return p.name }
func (p *fakeModelsProvider) Protocol() Protocol { return p.protocol }
func (p *fakeModelsProvider) SendRequest(ctx context.Context, req *Request) (*Response, error) {
	return nil, nil
}
func (p *fakeModelsProvider) SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error) {
	return nil, nil, nil
}
func (p *fakeModelsProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeModelsProvider) ListModels(ctx context.Context) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.models, nil
}
func (p *fakeModelsProvider) SetPool(*keypool.Pool) {}
func (p *fakeModelsProvider) Close() error          { return nil }

// fakeModelSyncStore 记录 UpsertModels 收到的参数用于断言
type fakeModelSyncStore struct {
	vendor   string
	modelIDs []string
	err      error
	calls    int
	// faceCalls 记录每个面各自落库的归属(P-model-face),face → modelIDs
	faceCalls map[string][]string
}

func (s *fakeModelSyncStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	s.calls++
	s.vendor = vendor
	s.modelIDs = modelIDs
	return s.err
}

func (s *fakeModelSyncStore) ReplaceFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	if s.faceCalls == nil {
		s.faceCalls = make(map[string][]string)
	}
	s.faceCalls[face] = modelIDs
	return nil
}

func (s *fakeModelSyncStore) AddFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	if s.faceCalls == nil {
		s.faceCalls = make(map[string][]string)
	}
	existing := s.faceCalls[face]
	for _, id := range modelIDs {
		found := false
		for _, ex := range existing {
			if ex == id {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, id)
		}
	}
	s.faceCalls[face] = existing
	return nil
}

func (s *fakeModelSyncStore) CountVendorModels(ctx context.Context, vendor string) (int, error) {
	if vendor == s.vendor {
		return len(s.modelIDs), nil
	}
	return 0, nil
}

func TestSyncVendorModels_FindsOpenAIFace(t *testing.T) {
	openai := &fakeModelsProvider{
		name:     "deepseek",
		protocol: ProtocolOpenAI,
		models:   []string{"deepseek-chat", "deepseek-reasoner"},
	}
	anthropic := &fakeModelsProvider{
		name:     "deepseek-anthropic",
		protocol: ProtocolAnthropic,
		models:   []string{"deepseek-chat"},
	}
	other := &fakeModelsProvider{
		name:     "qwen",
		protocol: ProtocolOpenAI,
		models:   []string{"qwen-max"},
	}
	// deepseek 有两个面(openai + anthropic),qwen 属于另一 vendor。
	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("deepseek", func(cfg ProviderConfig) (Provider, error) {
		return openai, nil
	}, ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("deepseek-anthropic", func(cfg ProviderConfig) (Provider, error) {
		return anthropic, nil
	}, ProtocolAnthropic, "deepseek")
	reg.RegisterWithProtocolVendor("qwen", func(cfg ProviderConfig) (Provider, error) {
		return other, nil
	}, ProtocolOpenAI, "qwen")
	mgr.SetForTesting("deepseek", openai)
	mgr.SetForTesting("deepseek-anthropic", anthropic)
	mgr.SetForTesting("qwen", other)

	store := &fakeModelSyncStore{}
	ids, err := SyncVendorModels(context.Background(), mgr, "deepseek", store)
	if err != nil {
		t.Fatalf("SyncVendorModels: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"deepseek-chat", "deepseek-reasoner"}) {
		t.Errorf("ids = %v, want [deepseek-chat deepseek-reasoner]", ids)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
	if store.vendor != "deepseek" {
		t.Errorf("store.vendor = %q, want %q", store.vendor, "deepseek")
	}
	if !reflect.DeepEqual(store.modelIDs, []string{"deepseek-chat", "deepseek-reasoner"}) {
		t.Errorf("store.modelIDs = %v, want [deepseek-chat deepseek-reasoner]", store.modelIDs)
	}
}

// TestSyncVendorModels_AnthropicOnlyFace P-model-sync: 多面聚合后,只有 anthropic 面、
// 但该面 ListModels 能返回模型的 vendor 也能同步(旧逻辑只认 openai 面,会漏)。
// 这是 rightapi claude 渠道(anthropic 面有自定义 ListModels)需要的语义。
func TestSyncVendorModels_AnthropicOnlyFace(t *testing.T) {
	anthropicOnly := &fakeModelsProvider{
		name:     "glm-anthropic",
		protocol: ProtocolAnthropic,
		models:   []string{"glm-4"},
	}
	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("glm-anthropic", func(cfg ProviderConfig) (Provider, error) {
		return anthropicOnly, nil
	}, ProtocolAnthropic, "glm")
	mgr.SetForTesting("glm-anthropic", anthropicOnly)

	store := &fakeModelSyncStore{}
	ids, err := SyncVendorModels(context.Background(), mgr, "glm", store)
	if err != nil {
		t.Fatalf("SyncVendorModels: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"glm-4"}) {
		t.Errorf("ids = %v, want [glm-4]", ids)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
}

// TestSyncVendorModels_AllFacesFail P-model-sync: 所有面 ListModels 都失败(或被跳过)→
// 报「无可用模型列表面」通用错误,且不 upsert。
func TestSyncVendorModels_AllFacesFail(t *testing.T) {
	boom := errors.New("upstream list failed")
	openai := &fakeModelsProvider{
		name:     "minimax",
		protocol: ProtocolOpenAI,
		models:   []string{"abab-6.5"},
		listErr:  boom,
	}
	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("minimax", func(cfg ProviderConfig) (Provider, error) {
		return openai, nil
	}, ProtocolOpenAI, "minimax")
	mgr.SetForTesting("minimax", openai)

	store := &fakeModelSyncStore{}
	_, err := SyncVendorModels(context.Background(), mgr, "minimax", store)
	if err == nil {
		t.Fatal("expected error when every face fails, got nil")
	}
	if store.calls != 0 {
		t.Errorf("store calls = %d, want 0 (should not upsert when all faces fail)", store.calls)
	}
}

// TestSyncVendorModels_MergesFaces P-model-sync: 同一 vendor 多个面各自有模型列表时,
// 合并去重到同一 vendor 名下(如 rightapi 的 codex[gpt-*] + claude[claude-*])。
func TestSyncVendorModels_MergesFaces(t *testing.T) {
	codexFace := &fakeModelsProvider{name: "rc-codex", protocol: ProtocolOpenAI, models: []string{"gpt-5.4", "gpt-5.5"}}
	claudeFace := &fakeModelsProvider{name: "rc-claude", protocol: ProtocolAnthropic, models: []string{"claude-opus-5", "gpt-5.5"}} // gpt-5.5 与 codex 重复,去重
	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("rc-codex", func(cfg ProviderConfig) (Provider, error) { return codexFace, nil }, ProtocolOpenAI, "rightapi")
	reg.RegisterWithProtocolVendor("rc-claude", func(cfg ProviderConfig) (Provider, error) { return claudeFace, nil }, ProtocolAnthropic, "rightapi")
	mgr.SetForTesting("rc-codex", codexFace)
	mgr.SetForTesting("rc-claude", claudeFace)

	store := &fakeModelSyncStore{}
	ids, err := SyncVendorModels(context.Background(), mgr, "rightapi", store)
	if err != nil {
		t.Fatalf("SyncVendorModels: %v", err)
	}
	// 验证返回的模型集合（不依赖顺序，因为 map 遍历顺序随机）
	want := map[string]bool{"gpt-5.4": true, "gpt-5.5": true, "claude-opus-5": true}
	if len(ids) != len(want) {
		t.Errorf("len(ids) = %d, want %d", len(ids), len(want))
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected model %q in ids", id)
		}
	}
	// 验证去重生效（gpt-5.5 在两个面都有，但只应出现一次）
	seen := make(map[string]int)
	for _, id := range ids {
		seen[id]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("model %q appears %d times, want 1 (should be deduped)", id, count)
		}
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1 (single upsert for merged list)", store.calls)
	}
	// P-model-face: 合并进定价表的同时,每个面各自的归属必须单独落库 ——
	// 否则中转站两个面共享合并清单,codex 面会拿到 claude 模型发给自己的端点 → 404。
	// 注意 gpt-5.5 同时属于两个面(去重只发生在 vendor 级合并,不影响面归属)。
	if got := store.faceCalls["rc-codex"]; !reflect.DeepEqual(got, []string{"gpt-5.4", "gpt-5.5"}) {
		t.Errorf("faceCalls[rc-codex] = %v, want [gpt-5.4 gpt-5.5]", got)
	}
	if got := store.faceCalls["rc-claude"]; !reflect.DeepEqual(got, []string{"claude-opus-5", "gpt-5.5"}) {
		t.Errorf("faceCalls[rc-claude] = %v, want [claude-opus-5 gpt-5.5]", got)
	}
}

// TestSyncVendorModels_FailedFaceKeepsItsAttribution P-model-face:某面 ListModels
// 失败/NotSupported 时**不动它已有的归属** —— 只有成功的面才 ReplaceFaceModels。
// 若失败也清空归属,该面会掉进 vendor 级 fallback 拿到全厂商模型(含别的面的),
// 等于把本次修的 bug 在上游抖动时重新引入。
func TestSyncVendorModels_FailedFaceKeepsItsAttribution(t *testing.T) {
	okFace := &fakeModelsProvider{name: "rc-codex", protocol: ProtocolOpenAI, models: []string{"gpt-5.4"}}
	failFace := &fakeModelsProvider{name: "rc-claude", protocol: ProtocolAnthropic, listErr: errors.New("upstream 503")}
	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("rc-codex", func(cfg ProviderConfig) (Provider, error) { return okFace, nil }, ProtocolOpenAI, "rightapi")
	reg.RegisterWithProtocolVendor("rc-claude", func(cfg ProviderConfig) (Provider, error) { return failFace, nil }, ProtocolAnthropic, "rightapi")
	mgr.SetForTesting("rc-codex", okFace)
	mgr.SetForTesting("rc-claude", failFace)

	store := &fakeModelSyncStore{}
	if _, err := SyncVendorModels(context.Background(), mgr, "rightapi", store); err != nil {
		t.Fatalf("SyncVendorModels: %v", err)
	}
	if _, ok := store.faceCalls["rc-codex"]; !ok {
		t.Error("成功的面必须落归属:faceCalls 缺 rc-codex")
	}
	if _, ok := store.faceCalls["rc-claude"]; ok {
		t.Error("失败的面不能落归属(会清空已有归属):faceCalls 不该有 rc-claude")
	}
}

// fakeCountingSyncStore 每个 vendor 独立计数,验证"全部同步"逐个落库。
type fakeCountingSyncStore struct {
	byVendor map[string][]string
	errByVendor map[string]error
}

func (s *fakeCountingSyncStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	if s.byVendor == nil {
		s.byVendor = make(map[string][]string)
	}
	s.byVendor[vendor] = modelIDs
	if s.errByVendor != nil {
		return s.errByVendor[vendor]
	}
	return nil
}

func (s *fakeCountingSyncStore) ReplaceFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	return nil
}

func (s *fakeCountingSyncStore) AddFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	return nil
}

func (s *fakeCountingSyncStore) CountVendorModels(ctx context.Context, vendor string) (int, error) {
	if s.byVendor == nil {
		return 0, nil
	}
	return len(s.byVendor[vendor]), nil
}

func TestSyncAllVendorModels_IteratesEveryVendor(t *testing.T) {
	deepseekOpenai := &fakeModelsProvider{name: "deepseek", protocol: ProtocolOpenAI, models: []string{"ds-c", "ds-r"}}
	minimaxOpenai := &fakeModelsProvider{name: "minimax-openai", protocol: ProtocolOpenAI, models: []string{"m3"}}
	mimoOpenai := &fakeModelsProvider{name: "mimo", protocol: ProtocolOpenAI, models: []string{"mimo-v2.5"}}

	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("deepseek", func(cfg ProviderConfig) (Provider, error) { return deepseekOpenai, nil }, ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("minimax-openai", func(cfg ProviderConfig) (Provider, error) { return minimaxOpenai, nil }, ProtocolOpenAI, "minimax")
	reg.RegisterWithProtocolVendor("mimo", func(cfg ProviderConfig) (Provider, error) { return mimoOpenai, nil }, ProtocolOpenAI, "mimo")
	mgr.SetForTesting("deepseek", deepseekOpenai)
	mgr.SetForTesting("minimax-openai", minimaxOpenai)
	mgr.SetForTesting("mimo", mimoOpenai)

	store := &fakeCountingSyncStore{}
	results, err := SyncAllVendorModels(context.Background(), mgr, store)
	if err != nil {
		t.Fatalf("SyncAllVendorModels: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (deepseek/minimax/mimo)", len(results))
	}
	// 结果按 vendor 名排序
	wantVendors := []string{"deepseek", "mimo", "minimax"}
	for i, r := range results {
		if r.Vendor != wantVendors[i] {
			t.Errorf("results[%d].Vendor = %q, want %q", i, r.Vendor, wantVendors[i])
		}
		if r.Error != "" {
			t.Errorf("results[%d] error = %q, want empty", i, r.Error)
		}
	}
	if len(store.byVendor) != 3 {
		t.Errorf("upserted %d vendors, want 3", len(store.byVendor))
	}
}

func TestSyncAllVendorModels_SingleVendorFailureDoesNotAbort(t *testing.T) {
	deepseekOpenai := &fakeModelsProvider{name: "deepseek", protocol: ProtocolOpenAI, models: []string{"ds-c"}}
	boom := errors.New("mimo list failed")
	mimoOpenai := &fakeModelsProvider{name: "mimo", protocol: ProtocolOpenAI, listErr: boom}

	reg := NewRegistry()
	mgr := NewManager(reg, zap.NewNop())
	reg.RegisterWithProtocolVendor("deepseek", func(cfg ProviderConfig) (Provider, error) { return deepseekOpenai, nil }, ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("mimo", func(cfg ProviderConfig) (Provider, error) { return mimoOpenai, nil }, ProtocolOpenAI, "mimo")
	mgr.SetForTesting("deepseek", deepseekOpenai)
	mgr.SetForTesting("mimo", mimoOpenai)

	store := &fakeCountingSyncStore{}
	results, err := SyncAllVendorModels(context.Background(), mgr, store)
	if err != nil {
		t.Fatalf("SyncAllVendorModels: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	// deepseek 成功,mimo 失败且不中断 deepseek 的落库
	if results[0].Vendor != "deepseek" || results[0].SyncedModels != 1 || results[0].Error != "" {
		t.Errorf("deepseek result = %+v, want success with 1 model", results[0])
	}
	if results[1].Vendor != "mimo" || results[1].Error == "" {
		t.Errorf("mimo result = %+v, want error recorded", results[1])
	}
	if store.byVendor["deepseek"] == nil {
		t.Errorf("deepseek should still be upserted despite mimo failure")
	}
}
