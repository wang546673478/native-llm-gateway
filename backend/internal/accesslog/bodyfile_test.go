package accesslog

import (
	"os"
	"path/filepath"
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
