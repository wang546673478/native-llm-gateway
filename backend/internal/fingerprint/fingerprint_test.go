package fingerprint

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实 Claude Code 请求里指纹的典型结构(local access log 2026-08-18 采样)
// 用于构造测试夹具。字段名/位置与实测 body 一致:
//   - metadata.user_id 是 JSON 字符串,内含 device_id/account_uuid/session_id
//   - system[].text 里 # Environment 块含 Platform/Shell/OS Version/working dir
func claudeCodeBody(deviceID, platform, shell, osVersion, workdir string) []byte {
	uid, _ := json.Marshal(map[string]string{
		"device_id":    deviceID,
		"account_uuid": "",
		"session_id":   "sess-123",
	})
	sys := []map[string]any{
		{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.233.14b; cc_entrypoint=cli;"},
		{"type": "text", "text": "You are Claude Code."},
		{"type": "text", "text": "# Environment\n" +
			" - Primary working directory: " + workdir + "\n" +
			" - Platform: " + platform + "\n" +
			" - Shell: " + shell + "\n" +
			" - OS Version: " + osVersion + "\n"},
	}
	body := map[string]any{
		"model":    "claude-opus-5",
		"metadata": map[string]any{"user_id": string(uid)},
		"system":   sys,
		"messages": []map[string]any{
			{"role": "user", "content": "please read linux files under " + workdir},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func snapForTest() Snapshot {
	return Snapshot{
		DeviceID:  "canonical-device",
		Platform:  "linux",
		Shell:     "bash",
		OSVersion: "6.8.0-canonical",
	}
}

func TestSanitizeReplacesDeviceID(t *testing.T) {
	body := claudeCodeBody("real-device-hash", "darwin", "zsh", "Darwin 24.0.0", "/Users/zhangsan/proj")
	out := Sanitize(body, snapForTest())

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("sanitized body not valid json: %v", err)
	}
	meta, _ := m["metadata"].(map[string]any)
	uidStr, _ := meta["user_id"].(string)
	var uid map[string]string
	_ = json.Unmarshal([]byte(uidStr), &uid)
	if uid["device_id"] != "canonical-device" {
		t.Errorf("device_id = %q, want canonical-device", uid["device_id"])
	}
	// 其它 user_id 子字段不动
	if uid["session_id"] != "sess-123" {
		t.Errorf("session_id should remain, got %q", uid["session_id"])
	}
}

func TestSanitizeReplacesEnvironmentBlock(t *testing.T) {
	body := claudeCodeBody("real", "darwin", "zsh", "Darwin 24.0.0", "/Users/zhangsan/proj")
	out := Sanitize(body, snapForTest())

	var m map[string]any
	_ = json.Unmarshal(out, &m)
	sys := m["system"].([]any)
	envText := ""
	for _, s := range sys {
		block := s.(map[string]any)
		if strings.Contains(block["text"].(string), "# Environment") {
			envText = block["text"].(string)
		}
	}
	if envText == "" {
		t.Fatal("no # Environment block found")
	}
	for _, want := range []string{
		"Platform: linux",
		"Shell: bash",
		"OS Version: 6.8.0-canonical",
	} {
		if !strings.Contains(envText, want) {
			t.Errorf("environment block missing %q, got:\n%s", want, envText)
		}
	}
}

// 核心无副作用断言:Primary working directory 必须保持原样,不能被归一化
func TestSanitizeDoesNotTouchWorkingDirectory(t *testing.T) {
	workdir := "/Users/zhangsan/proj"
	body := claudeCodeBody("real", "darwin", "zsh", "Darwin 24.0.0", workdir)
	out := Sanitize(body, snapForTest())

	if !strings.Contains(string(out), workdir) {
		t.Errorf("working directory must remain untouched, but it disappeared")
	}
	// 且 messages 里恰好出现的 linux 一词不被环境块替换污染
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	first := msgs[0].(map[string]any)
	if !strings.Contains(first["content"].(string), "read linux files") {
		t.Errorf("message content polluted, got %q", first["content"])
	}
}

func TestSanitizeInvalidJSONReturnsOriginal(t *testing.T) {
	bad := []byte("not-json{{{")
	if string(Sanitize(bad, snapForTest())) != string(bad) {
		t.Error("invalid json should be returned unchanged")
	}
}

func TestSanitizeNoMetadataReturnsUnchanged(t *testing.T) {
	body := []byte(`{"model":"x","messages":[]}`)
	out := Sanitize(body, snapForTest())
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["metadata"]; ok {
		t.Error("metadata should not be added if absent")
	}
}

func TestSanitizeEmptyDeviceIDDoesNotInject(t *testing.T) {
	// metadata.user_id 无 device_id 字段
	uid, _ := json.Marshal(map[string]string{"session_id": "s"})
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"user_id": string(uid)},
	})
	out := Sanitize(body, snapForTest())
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	meta := m["metadata"].(map[string]any)
	uidStr := meta["user_id"].(string)
	var got map[string]string
	_ = json.Unmarshal([]byte(uidStr), &got)
	if _, has := got["device_id"]; has {
		t.Error("should not inject device_id when absent")
	}
}
