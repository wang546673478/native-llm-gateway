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

// MaxBodyBytes 单条 body 文件 8MB 上限(spec §3.3 / F12)
// 任何调用方需要判断 body 大小上限或截断阈值时,都应使用此常量,
// 不要在内部再定义新 const。
const MaxBodyBytes = 8 * 1024 * 1024

// Write 写入 body 文件并返回相对于 rootDir 的路径。
// 单个文件最多保存 8 MB；超出部分丢弃，并在文件名中标记 truncated。
func (b *BodyFileWriter) Write(traceID, kind string, data []byte) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	truncated := len(data) > MaxBodyBytes
	if truncated {
		data = data[:MaxBodyBytes]
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
//
// 安全性(relPath containment,F2/F-defense-in-depth):
//   - 拒绝空 relPath
//   - 拒绝绝对路径(filepath.IsAbs)
//   - filepath.Clean 规范化后再 join,然后验证最终结果仍在 rootDir 内
//
// 这样即使将来某条 DB 行的 req_body_path 被污染为 "../etc/passwd",
// 也会被此处拦下,不会越权读到 rootDir 之外的文件。
func (b *BodyFileWriter) Read(relPath string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.checkContainment(relPath); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(b.rootDir, filepath.Clean(relPath)))
}

// checkContainment 校验 relPath 经 Clean 后仍位于 rootDir 之内。
//
// 设计要点:
//   - relPath 必须非空、非绝对路径
//   - filepath.Clean 消除 "../" 等相对穿越
//   - 最终绝对路径必须以 rootDir + PathSeparator 开头(避免
//     /var/data 与 /var/data2 这种"看起来包含但实际不同"的情况)
func (b *BodyFileWriter) checkContainment(relPath string) error {
	if relPath == "" {
		return os.ErrNotExist
	}
	if filepath.IsAbs(relPath) {
		return os.ErrPermission
	}
	cleanRel := filepath.Clean(relPath)
	absRoot, err := filepath.Abs(b.rootDir)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(filepath.Join(b.rootDir, cleanRel))
	if err != nil {
		return err
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) {
		return os.ErrPermission
	}
	return nil
}

// RootDir 返回 rootDir。
func (b *BodyFileWriter) RootDir() string { return b.rootDir }

// Close 释放资源（预留，目前无内部状态）。
func (b *BodyFileWriter) Close() error { return nil }

// IsTruncated 判断相对路径对应的 body 文件是否是 truncated 写入。
//
// BodyFileWriter.Write 在写入超过 8 MiB 的 body 时会在文件名后追加
// ".truncated.json" 后缀(F1/F12 决议 — truncation marker 仅在文件名里,
// 不进 DB 列,也不在 AccessEntry struct 里)。这是 canonical 的 truncation
// 表示 — 调用方拿到 relPath 后用本函数判断是否被截断。
func IsTruncated(relPath string) bool {
	return strings.HasSuffix(relPath, ".truncated.json")
}
