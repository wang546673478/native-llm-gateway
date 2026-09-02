package router

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// TestRouterPoolUsesVendorFallbackAndCopyOnWrite locks the same pool
// resolution contract used by RouteIterator.Next and by proxy key feedback.
// A face-only update must not mutate a map snapshot retained by an in-flight
// iterator.
func TestRouterPoolUsesVendorFallbackAndCopyOnWrite(t *testing.T) {
	r, openFace, anthFace, shared := newSharedMultiRelayRouter(t, "openai")
	if got := r.Pool(openFace); got != shared {
		t.Fatalf("Pool(%q) = %p, want shared vendor pool %p", openFace, got, shared)
	}
	if got := r.Pool(anthFace); got != shared {
		t.Fatalf("Pool(%q) = %p, want shared vendor pool %p", anthFace, got, shared)
	}

	oldMap := r.pools
	replacement := keypool.NewPool(openFace, nil, nil, keypool.Config{})
	r.SetPool(openFace, replacement)
	if got := r.Pool(openFace); got != replacement {
		t.Fatalf("Pool(%q) after SetPool = %p, want replacement %p", openFace, got, replacement)
	}
	if got := r.Pool(anthFace); got != shared {
		t.Fatalf("Pool(%q) after face update = %p, want shared vendor pool %p", anthFace, got, shared)
	}

	// Mutating the old map after SetPool must not affect Router readers.
	oldMap["stale-after-copy"] = replacement
	if got := r.Pool("stale-after-copy"); got != nil {
		t.Fatalf("Router retained caller map after copy-on-write: got %p", got)
	}
}
