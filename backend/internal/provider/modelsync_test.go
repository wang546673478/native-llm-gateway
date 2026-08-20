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
}

func (s *fakeModelSyncStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	s.calls++
	s.vendor = vendor
	s.modelIDs = modelIDs
	return s.err
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
	if !reflect.DeepEqual(ids, []string{"gpt-5.4", "gpt-5.5", "claude-opus-5"}) {
		t.Errorf("ids = %v, want [gpt-5.4 gpt-5.5 claude-opus-5] (merged, deduped)", ids)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1 (single upsert for merged list)", store.calls)
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
