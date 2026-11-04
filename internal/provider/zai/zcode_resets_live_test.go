package zai

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

// Read-only qualification of the personal inventory using an OAuth business
// login. It does not issue or use reset cards.
func TestZCodeLiveResetInventory(t *testing.T) {
	if os.Getenv("CQ_ZCODE_RESETS_LIVE") != "1" {
		t.Skip("set CQ_ZCODE_RESETS_LIVE=1 to read own reset inventory")
	}
	dir := os.Getenv("CQ_ZCODE_LIVE_CONFIG")
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		t.Fatal("select an existing isolated OAuth profile")
	}
	accounts, err := ReadAccounts(fsutil.OSFileSystem{}, func(string) string { return "" }, dir)
	if err != nil || len(accounts) == 0 || accounts[0].OAuth == nil {
		t.Fatal("own OAuth account required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := ResetClient{HTTP: zcodeResetLiveHTTP{httputil.NewClient(25*time.Second, "zcode-reset-qualification")}}
	inventory, err := client.List(ctx, accounts[0].OAuth.BusinessJWT, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("oauth_business_inventory=ok cards=%d", len(inventory.Credits))
}

type zcodeResetLiveHTTP struct{ httputil.Doer }

func (c zcodeResetLiveHTTP) Do(req *http.Request) (*http.Response, error) {
	zcode.SetSourceHeaders(req.Header, zcode.ClientVersion)
	return c.Doer.Do(req)
}
