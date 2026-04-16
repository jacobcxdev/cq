package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexSelector_PrefersActive(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "inactive@test.com", AccessToken: "tok-1", IsActive: false},
			{Email: "active@test.com", AccessToken: "tok-2", IsActive: true, AccountID: "acct-2"},
		}
	}, nil)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "active@test.com" {
		t.Errorf("email = %q, want active@test.com", acct.Email)
	}
	if acct.AccessToken != "tok-2" {
		t.Errorf("token = %q, want tok-2", acct.AccessToken)
	}
}

func TestCodexSelector_FallsBackToFirstWithToken(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "", IsActive: false},
			{Email: "b@test.com", AccessToken: "tok-b", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_NoAccounts(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return nil
	}, nil)

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for no accounts")
	}
}

func TestCodexSelector_NoValidTokens(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "", IsActive: true},
		}
	}, nil)

	_, err := sel.Select(context.Background())
	if err == nil {
		t.Fatal("expected error for no valid tokens")
	}
}

func TestCodexSelector_ReturnsCopy(t *testing.T) {
	accounts := []codex.CodexAccount{
		{Email: "a@test.com", AccessToken: "tok", IsActive: true},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return accounts
	}, nil)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Mutate returned account — should not affect source.
	acct.AccessToken = "mutated"
	if accounts[0].AccessToken == "mutated" {
		t.Error("selector returned reference instead of copy")
	}
}

func TestCodexSelector_ExcludeByEmail(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.Background(), "a@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeByAccountID(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", AccountID: "acct-1", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", AccountID: "acct-2", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeByRecordKey(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", RecordKey: "uid1::acct1", IsActive: true},
			{Email: "b@test.com", AccessToken: "tok-b", RecordKey: "uid2::acct2", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.Background(), "uid1::acct1")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "b@test.com" {
		t.Errorf("email = %q, want b@test.com", acct.Email)
	}
}

func TestCodexSelector_ExcludeAll(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "a@test.com", AccessToken: "tok-a", IsActive: true},
		}
	}, nil)

	_, err := sel.Select(context.Background(), "a@test.com")
	if err == nil {
		t.Fatal("expected error when all accounts are excluded")
	}
}

func TestCodexSelector_SkipsExhaustedAccounts(t *testing.T) {
	now := time.Now()
	quotaReader := stubQuotaReader{
		"dead": {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}}, FetchedAt: now},
		"live": {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 42}}}, FetchedAt: now},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountID: "dead", Email: "dead@test.com", AccessToken: "t1", IsActive: true},
			{AccountID: "live", Email: "live@test.com", AccessToken: "t2"},
		}
	}, quotaReader)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if acct == nil || acct.Email != "live@test.com" {
		t.Fatalf("got %+v, want live@test.com", acct)
	}
}

func TestCodexSelector_DoesNotSwitchWhenAllAccountsExhausted(t *testing.T) {
	now := time.Now()
	quotaReader := stubQuotaReader{
		"dead-a": {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}}, FetchedAt: now},
		"dead-b": {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}}, FetchedAt: now},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountID: "dead-a", Email: "a@test.com", AccessToken: "t1", IsActive: true},
			{AccountID: "dead-b", Email: "b@test.com", AccessToken: "t2"},
		}
	}, quotaReader)

	acct, err := sel.Select(context.Background())
	if err == nil || acct != nil {
		t.Fatalf("expected no eligible accounts, got acct=%v err=%v", acct, err)
	}
}

func TestCodexSelector_SelectsAccountWithNoWindowData(t *testing.T) {
	// An account whose quota snapshot has no windows returns MinRemainingPct()==-1.
	// This means "no data yet", not "exhausted". The selector must treat it as
	// eligible, not skip it.
	quotaReader := stubQuotaReader{
		"acct": {Result: quota.Result{Windows: nil}},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountID: "acct", Email: "a@test.com", AccessToken: "tok", IsActive: true},
		}
	}, quotaReader)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("Select error: %v (account with no window data should be eligible)", err)
	}
	if acct == nil || acct.Email != "a@test.com" {
		t.Fatalf("got %+v, want a@test.com", acct)
	}
}

// TestCodexSelector_StaleZeroPercentSnapshotIsEligible verifies that a stale
// zero-percent quota snapshot is NOT treated as confirmed-exhausted: the account
// must still be eligible for selection because the data is too old to trust.
func TestCodexSelector_StaleZeroPercentSnapshotIsEligible(t *testing.T) {
	// Snapshot with 0% remaining, but FetchedAt is 10 minutes ago (stale).
	staleTime := time.Now().Add(-10 * time.Minute)
	quotaReader := stubQuotaReader{
		"acct": {
			Result:    quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}},
			FetchedAt: staleTime,
		},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountID: "acct", Email: "a@test.com", AccessToken: "tok", IsActive: true},
		}
	}, quotaReader)

	acct, err := sel.Select(context.Background())
	if err != nil {
		t.Fatalf("Select error: %v (stale zero-pct snapshot should be treated as eligible)", err)
	}
	if acct == nil || acct.Email != "a@test.com" {
		t.Fatalf("got %+v, want a@test.com", acct)
	}
}

func TestCodexSelector_PrefersProAccountForSparkModel(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
			{Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "pro@test.com" {
		t.Fatalf("email = %q, want pro@test.com", acct.Email)
	}
}

func TestCodexSelector_PrefersProAccountForSparkVariant(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
			{Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark-high"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "pro@test.com" {
		t.Fatalf("email = %q, want pro@test.com", acct.Email)
	}
}

func TestCodexSelector_PrefersProAccountForSparkWithOneMSuffix(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
			{Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark[1m]"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "pro@test.com" {
		t.Fatalf("email = %q, want pro@test.com", acct.Email)
	}
}

func TestCodexSelector_UsesNonProAccountForNonSparkModel(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
			{Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "plus@test.com" {
		t.Fatalf("email = %q, want plus@test.com", acct.Email)
	}
}

func TestCodexSelector_KeepsActiveProForSparkModel(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: true},
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: false},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "pro@test.com" {
		t.Fatalf("email = %q, want pro@test.com", acct.Email)
	}
}

func TestCodexSelector_FallsBackToNonProWhenNoProAvailableForSparkModel(t *testing.T) {
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
		}
	}, nil)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "plus@test.com" {
		t.Fatalf("email = %q, want plus@test.com", acct.Email)
	}
}

func TestCodexSelector_FallsBackToNonProWhenProHasNoQuotaForSparkModel(t *testing.T) {
	now := time.Now()
	quotaReader := stubQuotaReader{
		"plus": {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 42}}}, FetchedAt: now},
		"pro":  {Result: quota.Result{Windows: map[quota.WindowName]quota.Window{quota.Window5Hour: {RemainingPct: 0}}}, FetchedAt: now},
	}
	sel := NewCodexSelector(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountID: "plus", Email: "plus@test.com", AccessToken: "tok-plus", PlanType: "plus", IsActive: true},
			{AccountID: "pro", Email: "pro@test.com", AccessToken: "tok-pro", PlanType: "pro", IsActive: false},
		}
	}, quotaReader)

	acct, err := sel.Select(context.WithValue(context.Background(), codexModelContextKey{}, "gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if acct.Email != "plus@test.com" {
		t.Fatalf("email = %q, want plus@test.com", acct.Email)
	}
}
