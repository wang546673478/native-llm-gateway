package accesslog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BodyFilePath 构造 body 文件相对路径。
// 调用方拿到路径后拼上 rootDir 得到绝对路径。
func BodyFilePath(traceID, date, kind string) string {
	return filepath.Join(date, traceID+"-"+kind+".json")
}

// BodyFileWriter 管理 body 文件的写入和读取。
// 文件按 UTC 日期分目录，并通过单个互斥锁串行化文件操作。
type BodyFileWriter struct {
	rootDir string
	mu      sync.Mutex
}

// NewBodyFileWriter 构造 writer；若 rootDir 不存在则创建。
func NewBodyFileWriter(rootDir string) (*BodyFileWriter, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, err
	}
	return &BodyFileWriter{rootDir: rootDir}, nil
}

// today 返回 YYYY-MM-DD（UTC）。
func (b *BodyFileWriter) today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// Write 写入 body 文件并返回相对于 rootDir 的路径。
// 单个文件最多保存 8 MB；超出部分丢弃，并在文件名中标记 truncated。
func (b *BodyFileWriter) Write(traceID, kind string, data []byte) (string, error) {
	const maxBodyBytes = 8 * 1024 * 1024

	b.mu.Lock()
	defer b.mu.Unlock()

	truncated := len(data) > maxBodyBytes
	if truncated {
		data = data[:maxBodyBytes]
	}

	date := b.today()
	relPath := BodyFilePath(traceID, date, kind)
	if truncated {
		relPath = strings.TrimSuffix(relPath, ".json") + ".truncated.json"
	}

	absPath := filepath.Join(b.rootDir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(absPath, data, 0o644); err != nil {
		return "", err
	}
	return relPath, nil
}

// Read 读取相对于 rootDir 的 body 文件内容。
func (b *BodyFileWriter) Read(relPath string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return os.ReadFile(filepath.Join(b.rootDir, relPath))
}

// RootDir 返回 rootDir。
func (b *BodyFileWriter) RootDir() string { return b.rootDir }

// Close 释放资源（预留，目前无内部状态）。
func (b *BodyFileWriter) Close() error { return nil }
