// Package minimax — quota balance polling
// P-quota-balance: GET https://www.minimaxi.com/v1/token_plan/remains
// Authorization: Bearer <subscription_key>
// Response schema is not officially documented; we accept several field names.
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

type miniMaxBalancer struct {
	client *http.Client
}

func newMiniMaxBalancer() *miniMaxBalancer {
	return &miniMaxBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *miniMaxBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	// Use the configured provider endpoint if non-empty, else hit the canonical host.
	endpoint := strings.TrimRight(baseURL, "/")
	if endpoint == "" {
		endpoint = "https://www.minimaxi.com"
	}
	url := endpoint + "/v1/token_plan/remains"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{HasQuota: false, Source: "minimax:/v1/token_plan/remains"}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Subscription-key mismatch — treat as auth failure; caller will DISABLE.
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
		}, fmt.Errorf("minimax quota auth failed: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
		}, fmt.Errorf("minimax quota http %d", resp.StatusCode)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{HasQuota: false, Source: "minimax:/v1/token_plan/remains"}, err
	}
	hasQuota, raw := extractAvailable(parsed)
	return quotacheck.Balance{
		Raw:      raw,
		HasQuota: hasQuota,
		Source:   "minimax:/v1/token_plan/remains",
	}, nil
}

// extractAvailable returns (hasQuota, rawValue) given an untyped JSON object.
// Tries several field names the MiniMax docs reference or that real responses
// have used; falls back to HasQuota=false when nothing meaningful is found.
func extractAvailable(m map[string]json.RawMessage) (hasQuota bool, raw float64) {
	candidates := []string{"quota_remaining", "remains", "balance", "available"}
	for _, k := range candidates {
		v, ok := m[k]
		if !ok {
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err == nil {
			return f > 0, f
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if f2, err := strconv.ParseFloat(s, 64); err == nil {
				return f2 > 0, f2
			}
		}
	}
	return false, 0
}

func init() {
	b := newMiniMaxBalancer()
	quotacheck.RegisterBalancer("minimax", b)
	quotacheck.RegisterBalancer("minimax-openai", b)
}
