package anthropic_compatible

import (
	"encoding/json"
	"testing"
)

// TestParseAnthropicUsage_Model P65: 验证 Anthropic 响应顶层 model 字段被抽到 Usage.Model
func TestParseAnthropicUsage_Model(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"type": "message",
		"model": "MiniMax-M3",
		"role": "assistant",
		"content": [{"type": "text", "text": "hi"}],
		"usage": {
			"input_tokens": 20,
			"output_tokens": 8,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 5
		}
	}`)
	u := parseAnthropicUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "MiniMax-M3" {
		t.Errorf("Model = %q, want MiniMax-M3", u.Model)
	}
	if u.PromptTokens != 20 {
		t.Errorf("PromptTokens = %d, want 20", u.PromptTokens)
	}
	if u.CompletionTokens != 8 {
		t.Errorf("CompletionTokens = %d, want 8", u.CompletionTokens)
	}
	if u.CacheReadTokens != 5 {
		t.Errorf("CacheReadTokens = %d, want 5", u.CacheReadTokens)
	}
	expectedTotal := 20 + 8 + 0 + 5
	if u.TotalTokens != expectedTotal {
		t.Errorf("TotalTokens = %d, want %d", u.TotalTokens, expectedTotal)
	}
}

// TestParseAnthropicUsage_MissingModel P65: 响应无 model 字段时 Usage.Model 为空
func TestParseAnthropicUsage_MissingModel(t *testing.T) {
	body := []byte(`{"id":"msg_02","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	u := parseAnthropicUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "" {
		t.Errorf("Model = %q, want empty", u.Model)
	}
}

// TestExtractAnthropicStreamUsage_Model P65: 验证流式 message_start 抽 model
func TestExtractAnthropicStreamUsage_Model(t *testing.T) {
	// message_start 事件:event: + data: 行
	event := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"MiniMax-M3\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n")

	var input, output, cacheCreate, cacheRead int
	var model string
	extractAnthropicStreamUsage(event, &input, &output, &cacheCreate, &cacheRead, &model)

	if model != "MiniMax-M3" {
		t.Errorf("model = %q, want MiniMax-M3", model)
	}
	if input != 10 {
		t.Errorf("input = %d, want 10", input)
	}
}

// TestExtractAnthropicStreamUsage_MessageDelta P65: 验证 message_delta 抽 output_tokens
func TestExtractAnthropicStreamUsage_MessageDelta(t *testing.T) {
	event := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n")

	var input, output, cacheCreate, cacheRead int
	var model string
	extractAnthropicStreamUsage(event, &input, &output, &cacheCreate, &cacheRead, &model)

	if output != 15 {
		t.Errorf("output = %d, want 15", output)
	}
	// message_delta 没有 message.model,model 应该保持原值
	if model != "" {
		t.Errorf("model = %q, want empty (message_delta 不抽 model)", model)
	}
}

// silence unused imports for parallel test
var _ = json.Unmarshal
