package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

type zcodeClaimTestDoer func(*http.Request) (*http.Response, error)

func (f zcodeClaimTestDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func zcodeClaimResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const zcodeClaimOfferFixture = `{"code":0,"data":{"server_time":1800000000,"language":"en-US","deliveries":[{"campaign_id":"gift-campaign","priority":2,"resource_position":"popup","popup":{"layout":"v1","title":{"format":"plaintext","content":"Start gift"},"description":{"format":"plaintext","content":"Daily tokens"},"hero":{"type":"image","args":{"model":"GLM-5.3","amountValue":"3000000","amountUnit":"Tokens"},"image":{"default":{"src":"https://example.invalid/offer.png","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},"buttons":[{"text":{"format":"plaintext","content":"Claim"},"action":{"type":"claim_zcode_plan","args":{"plan_id":"gift-plan"}}}]}}]}}`
const zcodeClaimNoOffers = `{"code":0,"data":{"server_time":1800000000,"language":"en-US","deliveries":[]}}`
const zcodeClaimConfigFixture = `{"code":0,"data":{"configs":{"captcha":{"enabled":true,"region":"sgp","prefix":"fixture-prefix","sceneId":"fixture-scene","skip_model_request":true}}}}`
const zcodeClaimAcceptedFixture = `{"code":0,"data":{"server_time":1800000000,"plan":{"plan_id":"gift-plan","user_plan_id":"owned-gift","status":"active"}}}`

func zcodeClaimAccount(id, label string) zaiprov.Account {
	device := "11111111-1111-4111-8111-111111111111"
	if id == "two" {
		device = "22222222-2222-4222-8222-222222222222"
	}
	return zaiprov.Account{ID: id, Label: label, OAuth: &zcode.LoginSession{ID: id, UserID: "user-" + id, ZCodeJWT: "private-jwt-" + id, DeviceMid: device}}
}

func zcodeClaimDeps(t *testing.T, doer zcodeClaimTestDoer) (zcodeClaimsDependencies, *bytes.Buffer, *bytes.Buffer, *fsutil.MemFS) {
	t.Helper()
	out, diagnostic, fsys := &bytes.Buffer{}, &bytes.Buffer{}, fsutil.NewMemFS()
	return zcodeClaimsDependencies{
		Accounts: func() ([]zaiprov.Account, error) {
			return []zaiprov.Account{zcodeClaimAccount("one", "primary"), zcodeClaimAccount("two", "secondary")}, nil
		},
		Client: zcode.Client{HTTP: doer, Version: "wrong-version", DeviceMid: "wrong-device"}, FS: fsys, StateDir: func() (string, error) { return "/state/claims", nil },
		In: strings.NewReader("n\n"), Out: out, ErrOut: diagnostic, Now: func() time.Time { return time.Unix(1800000000, 0) },
		Verify: func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
			// Fixture callback only: this does not assert vendor validity or human completion.
			return zcode.VerificationProof{Param: "private-sdk-proof", Region: "sgp"}, nil
		},
	}, out, diagnostic, fsys
}

func TestZCodeClaimListReadsOnlySelectedAccountOffers(t *testing.T) {
	requests := 0
	deps, out, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/api/v1/marketing/touch" || r.Header.Get("Authorization") != "Bearer private-jwt-two" || r.Header.Get("X-Device-Mid") != "22222222-2222-4222-8222-222222222222" || r.Header.Get("X-ZCode-App-Version") != "3.17.2" {
			t.Fatal("list crossed account or protocol boundaries")
		}
		return zcodeClaimResponse(zcodeClaimOfferFixture), nil
	})
	deps.FS, deps.StateDir = nil, nil
	deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
		t.Fatal("list started verification")
		return zcode.VerificationProof{}, nil
	}
	err := runZAIZCodeClaimsCommand(context.Background(), "list <reference>", ZAIZCodeClaimsCmd{List: ZAIZCodeClaimListCmd{Reference: "secondary"}}, true, deps)
	if err != nil || requests != 1 || !strings.Contains(out.String(), `"account_id":"two"`) || !strings.Contains(out.String(), `"amountValue":"3000000"`) || strings.Contains(out.String(), "private-") {
		t.Fatalf("incorrect or exposed list result: %v", err)
	}
}

func TestZCodeClaimRequiresCurrentOfferConfirmationAndVerification(t *testing.T) {
	for _, stage := range []string{"declined", "no-offer", "verification-cancelled"} {
		t.Run(stage, func(t *testing.T) {
			posts, verifications := 0, 0
			deps, _, diagnostic, fsys := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					t.Fatal("unapproved claim dispatched")
				}
				if r.URL.Path == "/api/v1/client/configs" {
					return zcodeClaimResponse(zcodeClaimConfigFixture), nil
				}
				if stage == "no-offer" {
					return zcodeClaimResponse(zcodeClaimNoOffers), nil
				}
				return zcodeClaimResponse(zcodeClaimOfferFixture), nil
			})
			deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
				verifications++
				return zcode.VerificationProof{}, context.Canceled
			}
			opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: stage == "verification-cancelled"}}
			err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps)
			if err == nil || posts != 0 || stage != "verification-cancelled" && verifications != 0 {
				t.Fatal("claim advanced past failed prerequisite")
			}
			if stage == "declined" && (!strings.Contains(diagnostic.String(), "3000000") || !strings.Contains(diagnostic.String(), "GLM-5.3")) {
				t.Fatal("confirmation omitted actual server benefit details")
			}
			if _, err := fsys.ReadFile(zcodeClaimAttemptPath("/state/claims", "one", "gift-plan")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsubmitted claim wrote pending intent")
			}
		})
	}
}

func TestZCodeClaimWritesPendingBeforePOSTAndSuppressesAcceptedReplay(t *testing.T) {
	posts, verifications, queries := 0, 0, 0
	var fsys *fsutil.MemFS
	deps, out, _, mem := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/client/configs" {
			return zcodeClaimResponse(zcodeClaimConfigFixture), nil
		}
		if r.Method == "GET" {
			queries++
			return zcodeClaimResponse(zcodeClaimOfferFixture), nil
		}
		posts++
		data, err := fsys.ReadFile(zcodeClaimAttemptPath("/state/claims", "two", "gift-plan"))
		var pending zcodeClaimAttempt
		if err != nil || json.Unmarshal(data, &pending) != nil || pending.State != "pending" || pending.AccountID != "two" || verifications != 1 || queries != 2 {
			t.Fatal("POST preceded verified durable intent")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["plan_id"] != "gift-plan" || r.Header.Get("Authorization") != "Bearer private-jwt-two" || r.Header.Get("X-Device-Mid") != "22222222-2222-4222-8222-222222222222" || r.Header.Get("X-Aliyun-Captcha-Verify-Param") != "private-sdk-proof" || r.Header.Get("X-Aliyun-Captcha-Verify-Region") != "sgp" {
			t.Fatal("claim changed account, plan or proof boundary")
		}
		return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
	})
	fsys = mem
	deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
		verifications++
		return zcode.VerificationProof{Param: "private-sdk-proof", Region: "sgp"}, nil
	}
	opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "secondary", PlanID: "gift-plan", Yes: true}}
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, true, deps); err != nil {
		t.Fatal(err)
	}
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, true, deps); err != nil || posts != 1 || verifications != 1 || queries != 2 {
		t.Fatal("accepted claim submitted again")
	}
	if !strings.Contains(out.String(), `"already_accepted":true`) {
		t.Fatal("accepted receipt not reported")
	}
	data, _ := fsys.ReadFile(zcodeClaimAttemptPath("/state/claims", "two", "gift-plan"))
	if strings.Contains(string(data), "private-") {
		t.Fatal("journal persisted proof or credentials")
	}
	for _, path := range []string{"/state/claims", zcodeClaimAttemptPath("/state/claims", "two", "gift-plan")} {
		info, err := fsys.Stat(path)
		want := os.FileMode(0o600)
		if path == "/state/claims" {
			want = 0o700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("claim state permissions not private")
		}
	}
}

func TestZCodeClaimPendingNeverRetriesAndAccountsAreIndependent(t *testing.T) {
	posts, verifications := 0, 0
	deps, _, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/client/configs" {
			return zcodeClaimResponse(zcodeClaimConfigFixture), nil
		}
		if r.Method == "GET" {
			return zcodeClaimResponse(zcodeClaimOfferFixture), nil
		}
		posts++
		if r.Header.Get("Authorization") == "Bearer private-jwt-one" {
			return nil, errors.New("private-transport-details")
		}
		return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
	})
	deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
		verifications++
		return zcode.VerificationProof{Param: "private-sdk-proof", Region: "sgp"}, nil
	}
	opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("ambiguous error missing or leaked")
	}
	for _, newAttempt := range []bool{false, true} {
		opts.Claim.New = newAttempt
		err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps)
		if err == nil || !strings.Contains(err.Error(), "balance") || posts != 1 || verifications != 1 {
			t.Fatal("uncertain claim retried")
		}
	}
	opts.Claim.Reference, opts.Claim.New = "secondary", false
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err != nil || posts != 2 || verifications != 2 {
		t.Fatal("pending claim crossed account boundary")
	}
}

func TestZCodeClaimNewRequiresFreshOfferAndConfirmation(t *testing.T) {
	posts, queries, available := 0, 0, true
	deps, _, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/client/configs" {
			return zcodeClaimResponse(zcodeClaimConfigFixture), nil
		}
		if r.Method == "GET" {
			queries++
			if !available {
				return zcodeClaimResponse(zcodeClaimNoOffers), nil
			}
			return zcodeClaimResponse(zcodeClaimOfferFixture), nil
		}
		posts++
		return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
	})
	opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err != nil {
		t.Fatal(err)
	}
	opts.Claim.New, available = true, false
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err == nil || posts != 1 {
		t.Fatal("new claimed absent offer")
	}
	available, opts.Claim.Yes = true, false
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err == nil || posts != 1 {
		t.Fatal("new bypassed confirmation")
	}
	opts.Claim.Yes = true
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err != nil || posts != 2 || queries < 5 {
		t.Fatal("fresh new claim failed")
	}
}

func TestZCodeClaimRejectsChangedOfferOrAccountBeforeDispatch(t *testing.T) {
	for _, changed := range []string{"offer", "account"} {
		t.Run(changed, func(t *testing.T) {
			verified, posts := false, 0
			deps, _, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
				}
				if r.URL.Path == "/api/v1/client/configs" {
					return zcodeClaimResponse(zcodeClaimConfigFixture), nil
				}
				body := zcodeClaimOfferFixture
				if changed == "offer" && verified {
					body = strings.ReplaceAll(body, "3000000", "1000000")
				}
				return zcodeClaimResponse(body), nil
			})
			deps.Accounts = func() ([]zaiprov.Account, error) {
				account := zcodeClaimAccount("one", "primary")
				if changed == "account" && verified {
					account.OAuth.UserID = "different-user"
				}
				return []zaiprov.Account{account}, nil
			}
			deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
				verified = true
				return zcode.VerificationProof{Param: "private-proof", Region: "sgp"}, nil
			}
			err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}, false, deps)
			if err == nil || posts != 0 {
				t.Fatal("changed identity or benefit was dispatched")
			}
		})
	}
}

func TestZCodeClaimPathsAndInvalidAccountsAreIsolated(t *testing.T) {
	path := zcodeClaimAttemptPath("/state/claims", "../../account", "../../plan")
	if filepath.Dir(path) != "/state/claims" || strings.Contains(filepath.Base(path), "account") || strings.Contains(filepath.Base(path), "plan") || path == zcodeClaimAttemptPath("/state/claims", "../../account2", "../../plan") {
		t.Fatal("journal path not hash safe or scoped")
	}
	deps, _, _, _ := zcodeClaimDeps(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid account reached network")
		return nil, nil
	})
	deps.Accounts = func() ([]zaiprov.Account, error) {
		return []zaiprov.Account{zcodeClaimAccount("one", "duplicate"), zcodeClaimAccount("two", "duplicate"), {ID: "key-only", APIKey: "private-key"}}, nil
	}
	for _, reference := range []string{"", "missing", "duplicate", "key-only"} {
		err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: reference, PlanID: "gift-plan", Yes: true}}, false, deps)
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("invalid claim account accepted or exposed")
		}
	}
}

func TestZCodeClaimLockRejectsAnotherPendingCaller(t *testing.T) {
	posts, blocked := 0, false
	deps, _, _, _ := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/client/configs" {
			return zcodeClaimResponse(zcodeClaimConfigFixture), nil
		}
		if r.Method == "GET" {
			return zcodeClaimResponse(zcodeClaimOfferFixture), nil
		}
		posts++
		return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
	})
	opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}
	deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
		err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps)
		blocked = err != nil && strings.Contains(err.Error(), "busy")
		return zcode.VerificationProof{Param: "private-proof", Region: "sgp"}, nil
	}
	if err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps); err != nil || !blocked || posts != 1 {
		t.Fatal("same account and plan had concurrent claim owners")
	}
}

func TestZCodeClaimIntentAndReceiptWriteFailuresDoNotAllowRetries(t *testing.T) {
	for _, stage := range []string{"intent", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			posts := 0
			var fsys *fsutil.MemFS
			deps, _, _, mem := zcodeClaimDeps(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/client/configs" {
					return zcodeClaimResponse(zcodeClaimConfigFixture), nil
				}
				if r.Method == "GET" {
					return zcodeClaimResponse(zcodeClaimOfferFixture), nil
				}
				posts++
				if stage == "receipt" {
					if err := fsys.Chmod("/state/claims", 0o755); err != nil {
						t.Fatal(err)
					}
				}
				return zcodeClaimResponse(zcodeClaimAcceptedFixture), nil
			})
			fsys = mem
			deps.Verify = func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error) {
				if stage == "intent" {
					if err := fsys.Chmod("/state/claims", 0o755); err != nil {
						t.Fatal(err)
					}
				}
				return zcode.VerificationProof{Param: "private-proof", Region: "sgp"}, nil
			}
			opts := ZAIZCodeClaimsCmd{Claim: ZAIZCodeClaimCmd{Reference: "primary", PlanID: "gift-plan", Yes: true}}
			err := runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps)
			if err == nil || stage == "intent" && posts != 0 || stage == "receipt" && posts != 1 {
				t.Fatal("journal write failure crossed claim dispatch boundary")
			}
			if stage == "receipt" {
				if err := fsys.Chmod("/state/claims", 0o700); err != nil {
					t.Fatal(err)
				}
				err = runZAIZCodeClaimsCommand(context.Background(), "claim <reference> <plan-id>", opts, false, deps)
				if err == nil || posts != 1 || !strings.Contains(err.Error(), "balance") {
					t.Fatal("accepted but unrecorded result allowed automatic retry")
				}
			}
		})
	}
}
