package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Explicit, read-only billing evidence. Aggregates cannot establish which
// coefficient was applied to one request; this check never infers a discount.
func TestZCodeLiveBillingSnapshot(t *testing.T) {
	if os.Getenv("CQ_ZCODE_BILLING_LIVE") != "1" {
		t.Skip("set CQ_ZCODE_BILLING_LIVE=1 to read real billing aggregates")
	}
	login, _ := zcodeLiveLogin(t)
	client := httputil.NewClient(30*time.Second, "zcode-billing-qualification")
	tokens := &ProjectTokenClient{HTTP: client, Version: ClientVersion}
	defer tokens.Clear()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	credential, err := tokens.Resolve(ctx, login)
	if err != nil {
		t.Fatal("resolve own OAuth project token")
	}
	now := time.Now().In(time.FixedZone("UTC+8", 8*60*60))
	day := now.Format("2006-01-02")
	endpoint := "https://api.z.ai/api/monitor/credit-usage/usage-detail?" + url.Values{
		"type": {"1"}, "usageType": {"MODEL"}, "startTime": {day + " 00:00:00"}, "endTime": {day + " 23:59:59"},
	}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", credential)
	SetSourceHeaders(req.Header, ClientVersion)
	response, err := client.Do(req)
	if err != nil || response == nil || response.Body == nil {
		t.Fatal("billing read failed")
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("billing response unavailable: status=%d", response.StatusCode)
	}
	var result struct {
		Code int `json:"code"`
		Data struct {
			Summary struct {
				TotalCredits struct {
					Value json.RawMessage `json:"value"`
				} `json:"totalCredits"`
			} `json:"summary"`
			ModelUsage struct {
				Models []struct {
					Code            string            `json:"modelCode"`
					Tokens          json.RawMessage   `json:"totalTokens"`
					Credits         json.RawMessage   `json:"totalCredits"`
					CreditsUsage    []json.RawMessage `json:"totalCreditsUsage"`
					TokensUsage     []json.RawMessage `json:"totalTokensUsage"`
					UncachedCredits []json.RawMessage `json:"uncachedInputCreditsUsage"`
					UncachedTokens  []json.RawMessage `json:"uncachedInputTokensUsage"`
					OutputCredits   []json.RawMessage `json:"outputCreditsUsage"`
					OutputTokens    []json.RawMessage `json:"outputTokensUsage"`
				} `json:"modelDataList"`
			} `json:"modelUsage"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code != 200 && result.Code != 0 {
		t.Fatal("billing envelope invalid")
	}
	logNumber := func(label string, raw json.RawMessage) {
		var number json.Number
		if len(raw) > 0 && json.Unmarshal(raw, &number) == nil && number != "" {
			t.Logf("%s=%s", label, number)
		}
	}
	t.Logf("status=%d date=%s models=%d", response.StatusCode, day, len(result.Data.ModelUsage.Models))
	logNumber("total_credits", result.Data.Summary.TotalCredits.Value)
	for _, model := range result.Data.ModelUsage.Models {
		// Only known public model codes and numeric totals reach diagnostics.
		if model.Code == "glm-5.3-flash" || model.Code == "GLM-5.3-Flash" || model.Code == "glm-5.3" || model.Code == "GLM-5.3" {
			t.Logf("model=%s", model.Code)
			logNumber("tokens", model.Tokens)
			logNumber("credits", model.Credits)
			for _, series := range []struct {
				label  string
				values []json.RawMessage
			}{
				{"credits_series", model.CreditsUsage}, {"tokens_series", model.TokensUsage},
				{"uncached_credits", model.UncachedCredits}, {"uncached_tokens", model.UncachedTokens},
				{"output_credits", model.OutputCredits}, {"output_tokens", model.OutputTokens},
			} {
				for _, value := range series.values {
					logNumber(series.label, value)
				}
			}
		}
	}
}
