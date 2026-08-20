package inflight

import (
	"testing"
	"time"
)

func TestPutSnapshotAndDelete(t *testing.T) {
	r := NewRegistry()

	r.Put(&Snapshot{TraceID: "t1", StartedAt: time.Now().UTC(), Model: "m1", IsStream: true})

	if got := r.Snapshot(); len(got) != 1 {
		t.Fatalf("after Put, Snapshot len = %d, want 1", len(got))
	} else if got[0].TraceID != "t1" || got[0].Model != "m1" {
		t.Fatalf("Snapshot[0] = %+v, want trace_id=t1 model=m1", got[0])
	}

	r.Delete("t1")
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("after Delete, Snapshot len = %d, want 0", len(got))
	}
}

func TestSetProviderUpdatesExisting(t *testing.T) {
	r := NewRegistry()
	r.Put(&Snapshot{TraceID: "t1", StartedAt: time.Now().UTC()})

	r.SetProvider("t1", "deepseek")
	if got := r.Snapshot(); len(got) != 1 || got[0].ProviderName != "deepseek" {
		t.Fatalf("after SetProvider, got %+v, want provider=deepseek", got)
	}

	// failover 切 provider
	r.SetProvider("t1", "minimax")
	if got := r.Snapshot(); got[0].ProviderName != "minimax" {
		t.Fatalf("after failover SetProvider, provider = %q, want minimax", got[0].ProviderName)
	}
}

func TestSetProviderUnknownTraceIsNoop(t *testing.T) {
	r := NewRegistry()
	r.SetProvider("ghost", "deepseek") // 不该 panic,不该插入
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("SetProvider on unknown trace inserted an entry: %+v", got)
	}
}

func TestSnapshotSortedByStartedAt(t *testing.T) {
	r := NewRegistry()
	base := time.Now().UTC()
	r.Put(&Snapshot{TraceID: "later", StartedAt: base.Add(2 * time.Second)})
	r.Put(&Snapshot{TraceID: "earlier", StartedAt: base})

	got := r.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(got))
	}
	if got[0].TraceID != "earlier" || got[1].TraceID != "later" {
		t.Fatalf("Snapshot not sorted by StartedAt: got [%s, %s], want [earlier, later]",
			got[0].TraceID, got[1].TraceID)
	}
}
