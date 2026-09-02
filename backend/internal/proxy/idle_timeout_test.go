package proxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestCalculateIdleTimeout 测试动态超时计算函数
func TestCalculateIdleTimeout(t *testing.T) {
	engine := &Engine{}

	tests := []struct {
		name     string
		bodySize int
		expected time.Duration
	}{
		{"small < 100KB", 50 * 1024, 10 * time.Second},
		{"medium 100KB-500KB", 200 * 1024, 15 * time.Second},
		{"large 500KB-1MB", 800 * 1024, 20 * time.Second},
		{"xlarge 1MB-2MB", 1500 * 1024, 30 * time.Second},
		{"xxlarge > 2MB", 3 * 1024 * 1024, 45 * time.Second},
		{"boundary 100KB", 100 * 1024, 15 * time.Second},
		{"boundary 500KB", 500 * 1024, 20 * time.Second},
		{"boundary 1MB", 1024 * 1024, 30 * time.Second},
		{"boundary 2MB", 2 * 1024 * 1024, 45 * time.Second},
		{"tiny 10KB", 10 * 1024, 10 * time.Second},
		{"huge 5MB", 5 * 1024 * 1024, 45 * time.Second},
		{"zero bytes", 0, 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.calculateIdleTimeout(tt.bodySize)
			assert.Equal(t, tt.expected, result,
				"timeout should be %v for body size %d bytes", tt.expected, tt.bodySize)
		})
	}
}

// TestCalculateIdleTimeout_Boundaries 测试所有边界值
func TestCalculateIdleTimeout_Boundaries(t *testing.T) {
	engine := &Engine{}

	boundaries := []struct {
		desc     string
		size     int
		expected time.Duration
	}{
		{"99KB (< 100KB)", 99 * 1024, 10 * time.Second},
		{"100KB (= 100KB)", 100 * 1024, 15 * time.Second},
		{"101KB (> 100KB)", 101 * 1024, 15 * time.Second},
		{"499KB (< 500KB)", 499 * 1024, 15 * time.Second},
		{"500KB (= 500KB)", 500 * 1024, 20 * time.Second},
		{"501KB (> 500KB)", 501 * 1024, 20 * time.Second},
		{"1023KB (< 1MB)", 1023 * 1024, 20 * time.Second},
		{"1MB (= 1MB)", 1024 * 1024, 30 * time.Second},
		{"1025KB (> 1MB)", 1025 * 1024, 30 * time.Second},
		{"2MB-1 (< 2MB)", 2*1024*1024 - 1, 30 * time.Second},
		{"2MB (= 2MB)", 2 * 1024 * 1024, 45 * time.Second},
		{"2MB+1 (> 2MB)", 2*1024*1024 + 1, 45 * time.Second},
	}

	for _, b := range boundaries {
		t.Run(b.desc, func(t *testing.T) {
			result := engine.calculateIdleTimeout(b.size)
			assert.Equal(t, b.expected, result,
				"%s (%d bytes) should have timeout %v", b.desc, b.size, b.expected)
		})
	}
}

// TestCalculateIdleTimeout_Consistency 测试一致性：相同大小总是返回相同超时
func TestCalculateIdleTimeout_Consistency(t *testing.T) {
	engine := &Engine{}

	testSizes := []int{
		50 * 1024,       // 50KB
		100 * 1024,      // 100KB
		500 * 1024,      // 500KB
		1024 * 1024,     // 1MB
		1500 * 1024,     // 1.5MB
		2 * 1024 * 1024, // 2MB
		5 * 1024 * 1024, // 5MB
	}

	for _, size := range testSizes {
		first := engine.calculateIdleTimeout(size)
		// 多次调用应该返回相同结果
		for i := 0; i < 10; i++ {
			result := engine.calculateIdleTimeout(size)
			assert.Equal(t, first, result,
				"calculateIdleTimeout(%d) should always return %v", size, first)
		}
	}
}

// TestCalculateIdleTimeout_Monotonic 测试单调性：更大的请求体 >= 更长的超时
func TestCalculateIdleTimeout_Monotonic(t *testing.T) {
	engine := &Engine{}

	sizes := []int{
		10 * 1024,        // 10KB
		100 * 1024,       // 100KB
		500 * 1024,       // 500KB
		1024 * 1024,      // 1MB
		2 * 1024 * 1024,  // 2MB
		10 * 1024 * 1024, // 10MB
	}

	var prevTimeout time.Duration
	for i, size := range sizes {
		timeout := engine.calculateIdleTimeout(size)
		if i > 0 {
			assert.GreaterOrEqual(t, timeout, prevTimeout,
				"timeout for %d bytes (%v) should be >= timeout for previous size (%v)",
				size, timeout, prevTimeout)
		}
		prevTimeout = timeout
	}
}

// TestCalculateIdleTimeout_ReasonableRange 测试返回值在合理范围内
func TestCalculateIdleTimeout_ReasonableRange(t *testing.T) {
	engine := &Engine{}

	testSizes := []int{
		0,
		1 * 1024,          // 1KB
		100 * 1024,        // 100KB
		1 * 1024 * 1024,   // 1MB
		10 * 1024 * 1024,  // 10MB
		100 * 1024 * 1024, // 100MB (极端情况)
	}

	minTimeout := 5 * time.Second
	maxTimeout := 60 * time.Second

	for _, size := range testSizes {
		timeout := engine.calculateIdleTimeout(size)
		assert.GreaterOrEqual(t, timeout, minTimeout,
			"timeout for %d bytes should be >= %v", size, minTimeout)
		assert.LessOrEqual(t, timeout, maxTimeout,
			"timeout for %d bytes should be <= %v", size, maxTimeout)
	}
}
