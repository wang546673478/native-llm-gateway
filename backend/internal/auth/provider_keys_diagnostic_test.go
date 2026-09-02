package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func newProviderKeyDiagnosticRouter(t *testing.T, h *ProviderKeysHandler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterOn(r.Group("/api/v1"))
	return r
}

func postProviderKeyDiagnostic(t *testing.T, r http.Handler, providerName, keyID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/providers/"+providerName+"/api-keys/"+keyID+"/diagnose",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestProviderKeysDiagnose_ValidMetadataOnly(t *testing.T) {
	db := newProviderKeyTestDB(t)
	row := dbpkg.ProviderAPIKey{
		ProviderName: "diag-provider", Name: "primary", KeyHash: "placeholder-key-value",
		Enabled: dbpkg.BoolPtr(true), BillingSource: "api",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}

	h := NewProviderKeysHandler(db, nil)
	var gotProvider, gotKey string
	var gotReq provider.KeyDiagnosticRequest
	var gotDeadline bool
	h.SetKeyDiagnosticFunc(func(ctx context.Context, providerName, keyID string, req provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
		gotProvider, gotKey, gotReq = providerName, keyID, req
		_, gotDeadline = ctx.Deadline()
		return &provider.KeyDiagnosticResult{
			ProviderName: "spoofed-provider",
			KeyID:        "spoofed-key",
			Protocol:     provider.ProtocolAnthropic,
			StatusCode:   http.StatusForbidden,
			ErrorType:    provider.ErrorTypeAuth,
			Message:      "diagnostic body placeholder-key-value",
			Reachable:    true,
			LatencyMs:    17,
			FirstByteMs:  5,
		}, nil
	})

	rec := postProviderKeyDiagnostic(t, newProviderKeyDiagnosticRouter(t, h), row.ProviderName,
		"1", `{"protocol":"anthropic","path":"/v1/messages","model":"claude-opus-5"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, _ := out["provider_name"].(string); got != row.ProviderName {
		t.Errorf("provider_name = %q, want %q", got, row.ProviderName)
	}
	if got, _ := out["key_id"].(string); got != "1" {
		t.Errorf("key_id = %q, want 1", got)
	}
	if got, _ := out["status_code"].(float64); got != http.StatusForbidden {
		t.Errorf("status_code = %v, want %d", got, http.StatusForbidden)
	}
	if got, _ := out["error_type"].(string); got != string(provider.ErrorTypeAuth) {
		t.Errorf("error_type = %q, want %q", got, provider.ErrorTypeAuth)
	}
	if got, _ := out["reachable"].(bool); !got {
		t.Error("reachable = false, want true")
	}
	if _, ok := out["message"]; ok {
		t.Errorf("diagnostic message must be omitted from metadata response: %v", out["message"])
	}
	responseBytes := rec.Body.Bytes()
	for _, forbidden := range []string{"placeholder-key-value", "headers", "raw_error"} {
		if bytes.Contains(responseBytes, []byte(forbidden)) {
			t.Errorf("response contains forbidden diagnostic value %q: %s", forbidden, responseBytes)
		}
	}
	if gotProvider != row.ProviderName || gotKey != "1" {
		t.Fatalf("callback identity = %q/%q, want %q/1", gotProvider, gotKey, row.ProviderName)
	}
	if gotReq.Protocol != provider.ProtocolAnthropic || gotReq.Path != "/v1/messages" || gotReq.Model != "claude-opus-5" {
		t.Fatalf("callback request = %+v", gotReq)
	}
	if !gotDeadline {
		t.Error("diagnostic callback context has no deadline")
	}
}

func TestProviderKeysDiagnose_OwnershipAndDisabled(t *testing.T) {
	db := newProviderKeyTestDB(t)
	rows := []dbpkg.ProviderAPIKey{
		{ProviderName: "owner-a", Name: "a", KeyHash: "key-a", Enabled: dbpkg.BoolPtr(true), BillingSource: "api"},
		{ProviderName: "owner-b", Name: "b", KeyHash: "key-b", Enabled: dbpkg.BoolPtr(false), BillingSource: "api"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed key %d: %v", i, err)
		}
	}
	h := NewProviderKeysHandler(db, nil)
	var calls int
	h.SetKeyDiagnosticFunc(func(context.Context, string, string, provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
		calls++
		return &provider.KeyDiagnosticResult{StatusCode: http.StatusOK, Reachable: true}, nil
	})
	r := newProviderKeyDiagnosticRouter(t, h)

	// ID 1 belongs to owner-a; naming owner-b must not let the caller probe it.
	rec := postProviderKeyDiagnostic(t, r, "owner-b", "1", `{"model":"m"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-provider status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// ID 2 exists but is disabled and must not reach the callback/upstream.
	rec = postProviderKeyDiagnostic(t, r, "owner-b", "2", `{"model":"m"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disabled status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("callback calls = %d, want 0 for rejected keys", calls)
	}
}

func TestProviderKeysDiagnose_CallbackErrorIsGeneric(t *testing.T) {
	db := newProviderKeyTestDB(t)
	row := dbpkg.ProviderAPIKey{
		ProviderName: "diag-errors", Name: "k", KeyHash: "stored-key-value",
		Enabled: dbpkg.BoolPtr(true), BillingSource: "api",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	h := NewProviderKeysHandler(db, nil)
	h.SetKeyDiagnosticFunc(func(context.Context, string, string, provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
		return nil, errors.New("upstream detail stored-key-value")
	})
	rec := postProviderKeyDiagnostic(t, newProviderKeyDiagnosticRouter(t, h), row.ProviderName, "1", `{"model":"m"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("stored-key-value")) || bytes.Contains(rec.Body.Bytes(), []byte("upstream detail")) {
		t.Fatalf("callback error leaked: %s", rec.Body.String())
	}
}

func TestProviderKeysDiagnose_RequiresCallbackAndValidInput(t *testing.T) {
	db := newProviderKeyTestDB(t)
	row := dbpkg.ProviderAPIKey{
		ProviderName: "diag-validation", Name: "k", KeyHash: "validation-key",
		Enabled: dbpkg.BoolPtr(true), BillingSource: "api",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	h := NewProviderKeysHandler(db, nil)
	r := newProviderKeyDiagnosticRouter(t, h)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "missing model", body: `{"protocol":"anthropic"}`, want: http.StatusBadRequest},
		{name: "bad protocol", body: `{"protocol":"bogus","model":"m"}`, want: http.StatusBadRequest},
		{name: "bad path", body: `{"path":"/invalid","model":"m"}`, want: http.StatusBadRequest},
		{name: "callback absent", body: `{"model":"m"}`, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postProviderKeyDiagnostic(t, r, row.ProviderName, "1", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, body = %s; want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestProviderKeysDiagnose_TwoKeysRemainIndependent(t *testing.T) {
	db := newProviderKeyTestDB(t)
	rows := []dbpkg.ProviderAPIKey{
		{ProviderName: "two-key-provider", Name: "first", KeyHash: "first-key", Enabled: dbpkg.BoolPtr(true), BillingSource: "api"},
		{ProviderName: "two-key-provider", Name: "second", KeyHash: "second-key", Enabled: dbpkg.BoolPtr(true), BillingSource: "api"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed key %d: %v", i, err)
		}
	}
	h := NewProviderKeysHandler(db, nil)
	var mu sync.Mutex
	var seen []string
	h.SetKeyDiagnosticFunc(func(_ context.Context, providerName, keyID string, _ provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
		if providerName != "two-key-provider" {
			t.Errorf("provider = %q", providerName)
		}
		mu.Lock()
		seen = append(seen, keyID)
		mu.Unlock()
		return &provider.KeyDiagnosticResult{
			ProviderName: providerName,
			KeyID:        keyID,
			Protocol:     provider.ProtocolAnthropic,
			StatusCode:   http.StatusOK,
			Reachable:    true,
		}, nil
	})
	r := newProviderKeyDiagnosticRouter(t, h)
	for _, id := range []string{"1", "2"} {
		rec := postProviderKeyDiagnostic(t, r, "two-key-provider", id, `{"protocol":"anthropic","model":"claude-opus-5"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("key %s status = %d, body = %s", id, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "1" || seen[1] != "2" {
		t.Fatalf("diagnosed key IDs = %v, want [1 2]", seen)
	}
	// The handler only reads DB rows; diagnostics do not update key records.
	var after []dbpkg.ProviderAPIKey
	if err := db.Order("id ASC").Find(&after).Error; err != nil {
		t.Fatalf("read keys: %v", err)
	}
	for _, row := range after {
		if !dbpkg.IsEnabled(row.Enabled) || row.KeyHash == "" {
			t.Fatalf("key row changed unexpectedly: %+v", row)
		}
	}
}

// Compile-time check keeps the test callback signature aligned with the narrow API.
var _ KeyDiagnosticFunc = func(context.Context, string, string, provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
	return nil, nil
}
