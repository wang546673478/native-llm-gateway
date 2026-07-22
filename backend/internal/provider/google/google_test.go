package google

import "testing"

// TestParseGoogleUsage_ModelVersion P65: 验证 Google 响应顶层 modelVersion 字段被抽到 Usage.Model
func TestParseGoogleUsage_ModelVersion(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"parts": [{"text": "hi"}], "role": "model"}}],
		"modelVersion": "gemini-2.0-flash",
		"usageMetadata": {
			"promptTokenCount": 12,
			"candidatesTokenCount": 6,
			"totalTokenCount": 18
		}
	}`)
	u := parseGoogleUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "gemini-2.0-flash" {
		t.Errorf("Model = %q, want gemini-2.0-flash", u.Model)
	}
	if u.PromptTokens != 12 {
		t.Errorf("PromptTokens = %d, want 12", u.PromptTokens)
	}
	if u.CompletionTokens != 6 {
		t.Errorf("CompletionTokens = %d, want 6", u.CompletionTokens)
	}
	if u.TotalTokens != 18 {
		t.Errorf("TotalTokens = %d, want 18", u.TotalTokens)
	}
}

// TestParseGoogleUsage_MissingModelVersion P65: 无 modelVersion 时 Usage.Model 为空
func TestParseGoogleUsage_MissingModelVersion(t *testing.T) {
	body := []byte(`{"candidates":[],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	u := parseGoogleUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "" {
		t.Errorf("Model = %q, want empty", u.Model)
	}
}
