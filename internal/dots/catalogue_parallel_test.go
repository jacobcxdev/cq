package dots

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCatalogueFixtureFetchedSecondaryOwnersConcurrently(t *testing.T) {
	c := catalogueTestClient(t)
	for _, owner := range []string{"system", "one", "two"} {
		if owner != "system" {
			catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, owner+".auth.json"), catalogueTestDocument(owner, c.now().Unix()+3600))
		}
		c.Settings.Dots[owner] = Dot{OwnerAlias: AccountAlias(owner), ProfileID: "profile-" + owner, RootThreadID: "root-" + owner}
	}
	profile := func(owner string) map[string]any {
		return map[string]any{"id": "profile-" + owner, "active_root_thread_id": "root-" + owner, "messaging_room_id": "room-" + owner, "display_name": owner, "status": "active"}
	}
	var mu sync.Mutex
	started := 0
	barrier := make(chan struct{})
	c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		started++
		if started == 2 {
			close(barrier)
		}
		mu.Unlock()
		select {
		case <-barrier:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return catalogueTestResponse(200, map[string]any{"items": []any{profile(r.Header.Get("ChatGPT-Account-Id"))}, "cursor": nil}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := c.Fixture(ctx, nil, []map[string]any{profile("system")})
	if err != nil {
		t.Fatalf("owners waited serially inside shared startup deadline: %v", err)
	}
	if len(got.Profiles) != 3 || got.Profiles[0]["id"] != "profile-system" {
		t.Fatalf("unstable assembled catalogue: %#v", got.Profiles)
	}
}

func TestCatalogueFixtureOwnerFailuresAndPanicsFailedClosed(t *testing.T) {
	for _, variant := range []string{"error", "panic", "cancel"} {
		t.Run(variant, func(t *testing.T) {
			c := catalogueTestClient(t)
			catalogueTestWrite(t, filepath.Join(c.Settings.CQAccounts, "owner.auth.json"), catalogueTestDocument("owner", c.now().Unix()+3600))
			c.Settings.Dots["owner"] = Dot{OwnerAlias: AccountAlias("owner"), ProfileID: "profile-owner", RootThreadID: "root-owner"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
				switch variant {
				case "panic":
					panic("synthetic owner failure")
				case "cancel":
					cancel()
					return nil, r.Context().Err()
				default:
					return catalogueTestResponse(503, map[string]any{}), nil
				}
			})
			if _, err := c.Fixture(ctx, nil, nil); err == nil {
				t.Fatal("owner failure published usable catalogue")
			}
		})
	}
}
