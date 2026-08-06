package quotacheck

import (
	"context"
	"errors"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

type fakeQuotaBalancer struct {
	hasQuota bool
	err      error
}

func (f *fakeQuotaBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (Balance, error) {
	if f.err != nil {
		return Balance{}, f.err
	}
	return Balance{Raw: 100, HasQuota: f.hasQuota, Kind: "percent"}, nil
}

func TestCheckQuota(t *testing.T) {
	k := &keypool.Key{ID: "7", ProviderName: "minimax", Name: "key-1"}

	t.Run("已注册且有余量 → true", func(t *testing.T) {
		RegisterBalancer("test-quota-ok", &fakeQuotaBalancer{hasQuota: true})
		got, err := CheckQuota(context.Background(), "test-quota-ok", "https://x.example", k)
		if err != nil || !got {
			t.Fatalf("want (true, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("已注册且耗尽 → false", func(t *testing.T) {
		RegisterBalancer("test-quota-out", &fakeQuotaBalancer{hasQuota: false})
		got, err := CheckQuota(context.Background(), "test-quota-out", "https://x.example", k)
		if err != nil || got {
			t.Fatalf("want (false, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("未注册 → (true, nil) 未知按未耗尽", func(t *testing.T) {
		got, err := CheckQuota(context.Background(), "no-such-provider", "https://x.example", k)
		if err != nil || !got {
			t.Fatalf("want (true, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("FetchBalance 出错 → 返回错误", func(t *testing.T) {
		RegisterBalancer("test-quota-err", &fakeQuotaBalancer{err: errors.New("boom")})
		_, err := CheckQuota(context.Background(), "test-quota-err", "https://x.example", k)
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}
