package modelregistry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

// Explicit, read-only discovery from the real Start balance and public model
// configuration. The selected account copy changes route only in memory.
func TestZCodeLiveStartCatalogue(t *testing.T) {
	if os.Getenv("CQ_ZCODE_CATALOGUE_LIVE") != "1" {
		t.Skip("explicit real Start catalogue qualification")
	}
	dir := os.Getenv("CQ_ZCODE_LIVE_CONFIG")
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		t.Fatal("select an isolated OAuth profile")
	}
	accounts, err := zaiprov.ReadAccounts(fsutil.OSFileSystem{}, func(string) string { return "" }, dir)
	if err != nil || len(accounts) == 0 || accounts[0].OAuth == nil {
		t.Fatal("own OAuth login required")
	}
	account := accounts[0]
	account.Transport = "zcode-start"
	client := httputil.NewClient(20*time.Second, "zcode-catalogue-qualification")
	provider := zaiprov.NewAccounts(client, func() ([]zaiprov.Account, error) { return []zaiprov.Account{account}, nil })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	source := &ZCodeStartSource{Catalogue: &zcode.ModelCatalogue{HTTP: client, Version: zcode.ClientVersion}, Quota: func(ctx context.Context) ([]quota.Result, error) { return provider.Fetch(ctx, time.Now()) }}
	result, err := source.Fetch(ctx)
	if err != nil || len(result.Entries) == 0 {
		t.Fatal("real Start model discovery failed")
	}
	projected := CodexModelsResponse(Snapshot{Entries: result.Entries, CodexRawByID: result.CodexRawByID})
	for _, raw := range projected.Models {
		var model struct {
			ID      string `json:"slug"`
			Context int    `json:"context_window"`
			Max     int    `json:"max_output_tokens"`
			Compact int64  `json:"auto_compact_token_limit"`
		}
		if json.Unmarshal(raw, &model) != nil || model.Context <= 0 || model.Max <= 0 {
			t.Fatal("invalid real Start model projection")
		}
		if model.ID == "glm-5.3" && model.Compact != int64(model.Context)*95/100*70/100 {
			t.Fatal("Start source lost GLM-5.3 compaction policy")
		}
		t.Logf("model=%s context=%d max_output=%d auto_compact=%d", model.ID, model.Context, model.Max, model.Compact)
	}
}
