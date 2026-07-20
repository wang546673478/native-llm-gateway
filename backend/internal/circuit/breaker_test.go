package circuit

import (
	"testing"
	"time"
)

func TestBreaker_StartsClosed(t *testing.T) {
	b := New("test", Config{FailureThreshold: 3, FailureWindow: time.Second})
	if b.State() != StateClosed {
		t.Errorf("initial state = %s, want CLOSED", b.State())
	}
	if !b.Allow() {
		t.Error("CLOSED should allow requests")
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := New("test", Config{FailureThreshold: 3, FailureWindow: time.Second, OpenTimeout: time.Hour})
	for i := 0; i < 3; i++ {
		b.RecordFailure("server_error")
	}
	if b.State() != StateOpen {
		t.Errorf("after 3 failures: state = %s, want OPEN", b.State())
	}
	if b.Allow() {
		t.Error("OPEN should reject requests within OpenTimeout")
	}
}

func TestBreaker_OpensToHalfOpenAfterTimeout(t *testing.T) {
	b := New("test", Config{FailureThreshold: 2, FailureWindow: time.Second, OpenTimeout: 10 * time.Millisecond, HalfOpenRequests: 1})
	b.RecordFailure("server_error")
	b.RecordFailure("server_error")
	if b.State() != StateOpen {
		t.Fatalf("expected OPEN, got %s", b.State())
	}
	time.Sleep(20 * time.Millisecond)
	// 下一次 Allow 应该把状态切到 HALF_OPEN
	if !b.Allow() {
		t.Fatal("after OpenTimeout, should allow 1 probe request")
	}
	if b.State() != StateHalfOpen {
		t.Errorf("state after Allow = %s, want HALF_OPEN", b.State())
	}
}

func TestBreaker_HalfOpenSuccessClosesBreaker(t *testing.T) {
	b := New("test", Config{FailureThreshold: 2, FailureWindow: time.Second, OpenTimeout: 5 * time.Millisecond, HalfOpenRequests: 1})
	b.RecordFailure("server_error")
	b.RecordFailure("server_error")
	time.Sleep(10 * time.Millisecond)
	b.Allow()                // 触发 OPEN → HALF_OPEN
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Errorf("after half-open success: state = %s, want CLOSED", b.State())
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	b := New("test", Config{FailureThreshold: 2, FailureWindow: time.Second, OpenTimeout: 5 * time.Millisecond, HalfOpenRequests: 1})
	b.RecordFailure("server_error")
	b.RecordFailure("server_error")
	time.Sleep(10 * time.Millisecond)
	b.Allow() // OPEN → HALF_OPEN
	b.RecordFailure("server_error")
	if b.State() != StateOpen {
		t.Errorf("after half-open failure: state = %s, want OPEN", b.State())
	}
}

func TestBreaker_ExcludedErrorNotCounted(t *testing.T) {
	b := New("test", Config{FailureThreshold: 2, FailureWindow: time.Second, OpenTimeout: time.Hour})
	// rate_limit 不应该计数
	for i := 0; i < 10; i++ {
		b.RecordFailure("rate_limit")
	}
	if b.State() != StateClosed {
		t.Errorf("rate_limit should not count toward opening, got state %s", b.State())
	}
}

func TestBreaker_ConnectionErrorCounted(t *testing.T) {
	b := New("test", Config{FailureThreshold: 2, FailureWindow: time.Second, OpenTimeout: time.Hour})
	b.RecordFailure("connection")
	b.RecordFailure("connection")
	if b.State() != StateOpen {
		t.Errorf("connection should count, got state %s", b.State())
	}
}

func TestBreaker_SlidingWindowExpiresOldFailures(t *testing.T) {
	b := New("test", Config{FailureThreshold: 3, FailureWindow: 50 * time.Millisecond, OpenTimeout: time.Hour})
	b.RecordFailure("server_error")
	b.RecordFailure("server_error")
	time.Sleep(100 * time.Millisecond)
	// 这条新失败进来时,旧的两条已经过期,计数应只有 1
	b.RecordFailure("server_error")
	if b.State() != StateClosed {
		t.Errorf("after old failures expire, state = %s, want CLOSED", b.State())
	}
}

func TestBreaker_HalfOpenRespectsLimit(t *testing.T) {
	b := New("test", Config{FailureThreshold: 1, FailureWindow: time.Second, OpenTimeout: 5 * time.Millisecond, HalfOpenRequests: 2})
	b.RecordFailure("server_error")
	time.Sleep(10 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("first probe should be allowed")
	}
	if !b.Allow() {
		t.Fatal("second probe should be allowed (HalfOpenRequests=2)")
	}
	if b.Allow() {
		t.Error("third probe should be denied (over HalfOpenRequests)")
	}
}

func TestBreaker_Reset(t *testing.T) {
	b := New("test", Config{FailureThreshold: 1, FailureWindow: time.Second, OpenTimeout: time.Hour})
	b.RecordFailure("server_error")
	if b.State() != StateOpen {
		t.Fatal("setup: should be OPEN")
	}
	b.Reset()
	if b.State() != StateClosed {
		t.Errorf("after Reset: state = %s, want CLOSED", b.State())
	}
	if !b.Allow() {
		t.Error("after Reset should allow requests")
	}
}

func TestBreaker_Stats(t *testing.T) {
	b := New("test", Config{FailureThreshold: 5, FailureWindow: time.Second, OpenTimeout: time.Hour})
	b.RecordFailure("server_error")
	b.RecordFailure("server_error")
	s := b.Stats()
	if s.State != StateClosed {
		t.Errorf("stats.State = %s, want CLOSED", s.State)
	}
	if s.FailuresInWindow != 2 {
		t.Errorf("stats.FailuresInWindow = %d, want 2", s.FailuresInWindow)
	}
}
