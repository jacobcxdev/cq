//go:build !windows

package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

func TestZAICompactionLiveSummaryAndReplay(t *testing.T) {
	if os.Getenv("CQ_ZAI_COMPACTION_LIVE") != "1" {
		t.Skip("live Z.ai compaction requires explicit opt-in")
	}
	file, err := os.Open(os.Getenv("CQ_ZAI_LIVE_CREDENTIAL_FILE"))
	if err != nil {
		t.Fatal("open Z.ai compaction credential file")
	}
	accounts, err := readZAILiveAccounts(file, "")
	_ = file.Close()
	if err != nil || len(accounts) == 0 {
		t.Fatal("read Z.ai compaction credentials")
	}
	s := &Server{Config: &Config{LocalToken: "isolated-compaction-owner"}, ZAIAccounts: func() ([]zaiprov.Account, error) { return accounts, nil }}
	marker := "CQ-COMPACT-" + rand.Text()
	initial, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "input": []any{
		map[string]string{"role": "user", "content": "Preserve the assistant's recorded marker for the next turn."},
		map[string]string{"role": "assistant", "content": "Recorded marker: " + marker},
		map[string]string{"type": "compaction_trigger"},
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	compacted, err := s.compactZAI(ctx, http.Header{}, initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted.Output) != 1 {
		t.Fatal("live compaction did not produce one owned state item")
	}
	replay, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "reasoning": map[string]string{"effort": "low"}, "stream": false, "max_output_tokens": 256, "input": []any{
		compacted.Output[0], map[string]string{"role": "user", "content": "Reply with exactly the recorded marker, and no other text. Do not use tools."},
	}})
	replay, changed, err := normaliseCQCompactionInput(replay, s.Config.LocalToken)
	if err != nil || !changed || !strings.Contains(string(replay), marker) {
		t.Fatal("live summary did not retain the assistant-only marker")
	}
	target, _ := s.zaiResponsesURL()
	response, err := s.doZAIRequest(ctx, target, http.Header{}, replay, accounts)
	if err != nil {
		t.Fatal("live compacted continuation failed")
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("live compacted continuation returned an unsuccessful response")
	}
	var result struct {
		Output []struct {
			Type    string
			Content []struct{ Type, Text string }
		}
	}
	if json.Unmarshal(body, &result) != nil {
		t.Fatal("decode live compacted continuation")
	}
	var text strings.Builder
	for _, item := range result.Output {
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
		}
	}
	if strings.TrimSpace(text.String()) != marker {
		t.Fatal("live compacted continuation lost the assistant-only marker")
	}
	t.Log("Real GLM summary and encrypted-state replay retained an assistant-only marker")
}
