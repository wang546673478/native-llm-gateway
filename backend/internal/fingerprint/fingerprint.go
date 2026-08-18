// Package fingerprint 归一化发往上游前 body 里的设备级指纹。
//
// 背景:Claude Code 客户端往 Anthropic 发的请求里夹了几处结构化、无功能副作用的
// 设备指纹(metadata.user_id.device_id + system[].text 里 # Environment 块的
// Platform/Shell/OS Version)。多台机器(亲友)经 Gateway 共用一把上游 key 时,
// 这些指纹各不相同,被上游风控判为「多设备共享账号」→ 封号。
//
// 本包只把这些「纯指纹」归一成 Gateway 自己的一套固定值(启动时采集一次真实
// 环境缓存进内存),绝不碰有功能副作用的字段:
//   - Primary working directory:保留原样 —— 它是功能字段,模型靠它定位相对路径/
//     读真实文件,替换会让路径语义错乱。
//   - messages 对话内容 / tools / thinking:x-anthropic-billing-header 保留。
//
// 单一职责:定位 + 替换。不 import proxy/provider 业务包,由 server 注入闭包使用。
package fingerprint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Snapshot Gateway 自己的环境快照,启动时 Capture 一次,只读,替换时直接引用。
type Snapshot struct {
	// DeviceID 统一 device_id。config 配了就固定;否则 Capture 里随机生成一次。
	DeviceID string
	Platform string // runtime.GOOS
	Shell    string // $SHELL,空则 "bash"
	// OSVersion 完整 kernel 版本(uname -r);失败则 fallback runtime.GOOS。
	OSVersion string
}

// Capture 采集一次 Gateway 真实环境 + device_id。
// canonicalDeviceID 非空时用之;空则生成一个稳定的随机值(仅本进程内存,不落盘)。
func Capture(canonicalDeviceID string) Snapshot {
	deviceID := canonicalDeviceID
	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}
	return Snapshot{
		DeviceID:  deviceID,
		Platform:  runtime.GOOS,
		Shell:     shell,
		OSVersion: osVersion(),
	}
}

// osVersion 读 uname -r 拿完整 kernel 版本;失败返回 runtime.GOOS(保守兜底)。
func osVersion() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return runtime.GOOS
	}
	return strings.TrimSpace(string(out))
}

// generateDeviceID 生成一个稳定的 device_id(64 字符 hex)。
// 系统没有 device_id 此值,Gateway 启动时造一次、进程内保持不变即可 —
// 所以只在此生成一次(见 Capture),无需持久化。用 crypto/rand 保证唯一性。
func generateDeviceID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下 crypto/rand 失败,兜底常量(几乎不可能触发)
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// Sanitize 把 body 里的设备指纹归一成 snap 的固定值。
// 只改 metadata.user_id.device_id 与 system[].text 里 # Environment 块三字段,
// 不碰 messages / tools / thinking / working directory。
// body 非法 JSON / 无 fingerprint 字段时原样返回(透传语义不变)。
func Sanitize(body []byte, snap Snapshot) []byte {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}

	if changed := sanitizeMetadata(root, snap.DeviceID); changed {
		// 继续走完整流程
	}
	sanitizeSystem(root, snap)

	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// sanitizeMetadata 替换 metadata.user_id 里的 device_id;返回是否有改。
func sanitizeMetadata(root map[string]any, deviceID string) bool {
	if deviceID == "" {
		return false
	}
	meta, ok := root["metadata"].(map[string]any)
	if !ok {
		return false
	}
	uidStr, ok := meta["user_id"].(string)
	if !ok || uidStr == "" {
		return false
	}
	var uid map[string]any
	if err := json.Unmarshal([]byte(uidStr), &uid); err != nil {
		return false
	}
	if _, has := uid["device_id"]; !has {
		return false
	}
	uid["device_id"] = deviceID
	re, err := json.Marshal(uid)
	if err != nil {
		return false
	}
	meta["user_id"] = string(re)
	return true
}

// sanitizeSystem 替换 system[].text 里 # Environment 块的 Platform/Shell/OS Version。
func sanitizeSystem(root map[string]any, snap Snapshot) {
	sys, ok := root["system"].([]any)
	if !ok {
		return
	}
	for _, item := range sys {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			continue
		}
		block["text"] = sanitizeEnvBlock(text, snap)
	}
}

// sanitizeEnvBlock 替换 # Environment 段内的三行,其余文本(含 working directory)
// 原样保留。
func sanitizeEnvBlock(text string, snap Snapshot) string {
	idx := strings.Index(text, "# Environment")
	if idx < 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = replaceLinePrefix(lines[i], " - Platform:", snap.Platform)
		lines[i] = replaceLinePrefix(lines[i], " - Shell:", snap.Shell)
		lines[i] = replaceLinePrefix(lines[i], " - OS Version:", snap.OSVersion)
	}
	return strings.Join(lines, "\n")
}

// replaceLinePrefix 若行以 prefix 开头,整体替换为 "prefix value",否则原样返回。
func replaceLinePrefix(line, prefix, value string) string {
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	return prefix + " " + value
}
