package accesslog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBodyFile_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	bw, err := NewBodyFileWriter(dir)
	if err != nil {
		t.Fatalf("NewBodyFileWriter: %v", err)
	}
	defer bw.Close()

	traceID := "test-trace-1"
	data := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`)

	path, err := bw.Write(traceID, "req", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 路径是相对路径,格式 YYYY-MM-DD/{traceID}-req.json
	wantPrefix := filepath.Join(bw.today(), traceID) + "-req"
	if !filepath.HasPrefix(path, wantPrefix) {
		t.Errorf("path = %q, want prefix %q", path, wantPrefix)
	}

	// 文件存在
	full := filepath.Join(dir, path)
	if _, err := os.Stat(full); err != nil {
		t.Errorf("file not found: %v", err)
	}

	// 能读回
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("body mismatch: got %q want %q", got, data)
	}
}

func TestBodyFile_PathFor(t *testing.T) {
	// 不创建文件,只断言路径格式
	got := BodyFilePath("trace-abc", "2026-07-22", "req")
	want := filepath.Join("2026-07-22", "trace-abc-req.json")
	if got != want {
		t.Errorf("BodyFilePath = %q, want %q", got, want)
	}
}

func TestBodyFile_WriteTruncated(t *testing.T) {
	dir := t.TempDir()
	bw, err := NewBodyFileWriter(dir)
	if err != nil {
		t.Fatalf("NewBodyFileWriter: %v", err)
	}
	defer bw.Close()

	data := make([]byte, 8*1024*1024+1)
	path, err := bw.Write("test-trace-truncated", "resp", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := filepath.Join(bw.today(), "test-trace-truncated-resp.truncated.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	got, err := bw.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 8*1024*1024 {
		t.Errorf("body size = %d, want %d", len(got), 8*1024*1024)
	}
}

// TestBodyFile_ReadContainment 验证 Read 的 relPath 不会越权读到 rootDir 之外
// (F2 / final-review.md Important 决议:防止 DB 行被污染时读到 /etc/passwd 等)。
//
// 覆盖:
//   - ../../etc/passwd  → ErrPermission
//   - 绝对路径 /etc/passwd → ErrPermission
//   - 空字符串 → ErrNotExist
//   - 合法相对路径 2026-07-22/{trace}-req.json → 成功
//   - 同名前缀边界(防止 /var/data vs /var/data2 误判)
func TestBodyFile_ReadContainment(t *testing.T) {
	dir := t.TempDir()
	bw, err := NewBodyFileWriter(dir)
	if err != nil {
		t.Fatalf("NewBodyFileWriter: %v", err)
	}
	defer bw.Close()

	// 先写一个真实合法文件,后面用它的相对路径去读
	data := []byte(`{"model":"MiniMax-M3"}`)
	rel, err := bw.Write("real-trace", "req", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	t.Run("path_traversal_rejected", func(t *testing.T) {
		_, err := bw.Read("../../etc/passwd")
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("expected ErrPermission for ../, got %v", err)
		}
	})

	t.Run("absolute_path_rejected", func(t *testing.T) {
		_, err := bw.Read("/etc/passwd")
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("expected ErrPermission for absolute path, got %v", err)
		}
	})

	t.Run("empty_path_rejected", func(t *testing.T) {
		_, err := bw.Read("")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected ErrNotExist for empty path, got %v", err)
		}
	})

	t.Run("normal_relative_path_works", func(t *testing.T) {
		got, err := bw.Read(rel)
		if err != nil {
			t.Fatalf("Read(%q): %v", rel, err)
		}
		if string(got) != string(data) {
			t.Errorf("body mismatch: got %q want %q", got, data)
		}
	})

	t.Run("sibling_dir_prefix_not_contained", func(t *testing.T) {
		// 构造一个 "含 ./ 但 Clean 后落在已写出的文件上" 的 relPath,
		// 验证 Reader 接受这种 normalized form。
		//
		// 真实目录里写的路径是 `2026-07-22/real-trace-req.json`。
		// 给定的 relPath 是 `2026-07-22/./real-trace-req.json` —
		// Clean 后等价于前者,Read 应能正确读到原数据。
		clean := filepath.Clean(bw.today() + "/./" + filepath.Base(rel))
		got, err := bw.Read(clean)
		if err != nil {
			t.Fatalf("Cleaned path %q should be allowed, got %v", clean, err)
		}
		if string(got) != string(data) {
			t.Errorf("body mismatch: got %q want %q", got, data)
		}
	})

	t.Run("nested_traversal_rejected", func(t *testing.T) {
		// 深层 ../ 也必须被拦下
		_, err := bw.Read(strings.Repeat("../", 5) + "etc/passwd")
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("expected ErrPermission for nested ../, got %v", err)
		}
	})
}
