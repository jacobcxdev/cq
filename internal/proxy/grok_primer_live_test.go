package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

// This opt-in check qualifies one native primer request, not five-hour timer
// activation. The selected saved subscription must have positive included quota.
func TestGrokLivePrimerRequest(t *testing.T) {
	if os.Getenv("CQ_GROK_PRIME_REQUEST_LIVE") != "1" {
		t.Skip("explicit live primer request opt-in required")
	}
	config := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if !filepath.IsAbs(config) {
		t.Fatal("absolute CQ_GROK_LIVE_CONFIG required")
	}
	authority := grok.NewAuthority(grok.Store{FS: fsutil.OSFileSystem{}, ConfigDir: config}, httputil.NewClient(30*time.Second, "cq-primer-qualification"), nil)
	now := time.Now()
	results, err := grok.New(authority).Fetch(t.Context(), now)
	if err != nil || len(results) == 0 || !results[0].IsUsable() || results[0].MinRemainingPct() <= 0 {
		t.Fatal("positive included subscription quota required")
	}
	id := grok.AccountID(results[0].AccountID)
	models, err := grok.FetchModels(t.Context(), authority, id)
	if err != nil {
		t.Fatal("account model catalogue unavailable")
	}
	model, err := grokPrimerModel(models)
	if err != nil {
		t.Fatal(err)
	}
	primer, err := NewGrokPrimer(fsutil.NewMemFS(), "/state", authority)
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	_, err = primer.withJournal(t.Context(), now, time.Minute, func() (time.Time, error) {
		return primer.dispatch(t.Context(), now, string(id), 5*time.Hour, time.Minute, func() (string, error) {
			state, err := primer.send(t.Context(), id, model)
			if err != nil || state != "admitted" {
				return state, errors.New("live native primer request was not admitted")
			}
			admitted = true
			return state, nil
		}, func() (int64, error) { return 0, errors.New("no live five-hour activation contract") })
	})
	if !admitted || err == nil || primer.Health(true).Accounts[string(id)].State != "admitted" {
		t.Fatal("native request admission or honest unverified state missing", err)
	}
	t.Log("one native primer request admitted; five-hour activation remains unverified")
}
