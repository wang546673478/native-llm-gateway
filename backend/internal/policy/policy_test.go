package policy

import (
	"testing"
)

func cs() []ProviderRoute {
	return []ProviderRoute{
		{Name: "p1", Model: "m1", Priority: 5, Weight: 10},
		{Name: "p2", Model: "m2", Priority: 1, Weight: 90},
		{Name: "p3", Model: "m3", Priority: 3, Weight: 50},
	}
}

func TestPriorityPolicy_OrdersByPriority(t *testing.T) {
	out, err := NewPriorityPolicy().Order(cs())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p2", "p3", "p1"}
	for i, r := range out {
		if r.Name != want[i] {
			t.Errorf("pos %d: got %s, want %s", i, r.Name, want[i])
		}
	}
}

func TestWeightPolicy_PutsOneOnTop(t *testing.T) {
	// 多次跑,验证确实随机选了不同项
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		out, err := NewWeightPolicy().Order(cs())
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 {
			t.Fatalf("len(out) = %d, want 3", len(out))
		}
		seen[out[0].Name] = true
	}
	// 100 次随机至少覆盖 2 个 provider(p2 权重 90 应该几乎必出)
	if len(seen) < 2 {
		t.Errorf("random distribution too narrow: saw %v", seen)
	}
	if !seen["p2"] {
		t.Error("p2 (weight=90) should appear at least once in 100 trials")
	}
}

func TestCostPolicy_OrdersByWeight(t *testing.T) {
	// P4 阶段 CostPolicy 用 Weight 字段当 cost proxy
	out, err := NewCostPolicy().Order(cs())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p1", "p3", "p2"} // weight 升序
	for i, r := range out {
		if r.Name != want[i] {
			t.Errorf("pos %d: got %s, want %s", i, r.Name, want[i])
		}
	}
}

func TestHealthPolicy_PreservesOrder(t *testing.T) {
	in := cs()
	out, err := NewHealthPolicy().Order(in)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range out {
		if r.Name != in[i].Name {
			t.Errorf("pos %d: got %s, want %s", i, r.Name, in[i].Name)
		}
	}
}

func TestPriorityPolicy_EmptyReturnsErr(t *testing.T) {
	_, err := NewPriorityPolicy().Order(nil)
	if err == nil {
		t.Error("expected ErrNoCandidate on empty input")
	}
}
