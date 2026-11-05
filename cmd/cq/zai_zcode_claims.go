package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

type ZAIZCodeClaimsCmd struct {
	List  ZAIZCodeClaimListCmd `cmd:"" help:"List currently offered Z.ai token plans without claiming"`
	Claim ZAIZCodeClaimCmd     `cmd:"" help:"Claim one currently offered plan after confirmation and browser verification"`
}

type ZAIZCodeClaimListCmd struct {
	Reference string `arg:"" help:"OAuth account ID or unique label"`
}

type ZAIZCodeClaimCmd struct {
	Reference string `arg:"" help:"OAuth account ID or unique label"`
	PlanID    string `arg:"" help:"Plan ID from the account's current offers"`
	Yes       bool   `help:"Confirm the selected offer without an interactive prompt"`
	New       bool   `help:"Confirm a new claim after a recorded accepted result"`
}

type zcodeClaimsDependencies struct {
	Accounts func() ([]zaiprov.Account, error)
	Client   zcode.Client
	FS       fsutil.FileSystem
	StateDir func() (string, error)
	In       io.Reader
	Out      io.Writer
	ErrOut   io.Writer
	Now      func() time.Time
	OpenURL  func(context.Context, string) error
	Verify   func(context.Context, zcode.VerificationConfig, func(context.Context, string) error) (zcode.VerificationProof, error)
}

// This journal contains public receipt fields only. In particular, it never
// stores the OAuth session or the short-lived, single-use verification proof.
type zcodeClaimAttempt struct {
	SchemaVersion int                `json:"schema_version"`
	AccountID     string             `json:"account_id"`
	UserID        string             `json:"user_id"`
	PlanID        string             `json:"plan_id"`
	State         string             `json:"state"`
	StartedAt     time.Time          `json:"started_at"`
	AcceptedAt    *time.Time         `json:"accepted_at,omitempty"`
	Result        *zcode.ClaimResult `json:"result,omitempty"`
}

func runZAIZCodeClaimsCommand(ctx context.Context, command string, options ZAIZCodeClaimsCmd, jsonOutput bool, deps zcodeClaimsDependencies) error {
	fields := strings.Fields(command)
	if len(fields) == 0 || fields[0] != "list" && fields[0] != "claim" {
		return errors.New("unknown ZCode claims command")
	}
	if deps.Accounts == nil || deps.Out == nil {
		return errors.New("ZCode claims command unavailable")
	}
	reference := options.List.Reference
	if fields[0] == "claim" {
		reference = options.Claim.Reference
	}
	account, err := selectZCodeClaimAccount(deps.Accounts, reference)
	if err != nil {
		return err
	}
	client := zcodeClaimClient(deps.Client, account)
	if fields[0] == "list" {
		offers, err := client.PlanOffers(ctx, account.OAuth.AsSession())
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(deps.Out).Encode(struct {
				AccountID string            `json:"account_id"`
				Label     string            `json:"label,omitempty"`
				Offers    []zcode.PlanOffer `json:"offers"`
			}{account.ID, account.Label, offers})
		}
		if len(offers) == 0 {
			_, err = fmt.Fprintln(deps.Out, "Z.ai has no currently offered token-plan claims for this account.")
			return err
		}
		for _, offer := range offers {
			if err := describeZCodeClaim(deps.Out, account, offer); err != nil {
				return err
			}
		}
		return nil
	}
	if deps.FS == nil || deps.StateDir == nil || deps.Now == nil || deps.ErrOut == nil || strings.TrimSpace(options.Claim.PlanID) == "" {
		return errors.New("ZCode claim storage or plan selection unavailable")
	}
	dir, err := deps.StateDir()
	if err != nil || dir == "" {
		return errors.New("resolve ZCode claim state directory")
	}
	if err := fsutil.EnsureSecureDirectory(deps.FS, dir); err != nil {
		return errors.New("prepare private ZCode claim state")
	}
	path := zcodeClaimAttemptPath(dir, account.ID, options.Claim.PlanID)
	lock, err := fsutil.AcquireExclusiveLock(deps.FS, path+".lock")
	if err != nil {
		return errors.New("ZCode account and plan claim is busy or its state is unavailable")
	}
	defer lock.Close()
	account, err = selectZCodeClaimAccount(deps.Accounts, account.ID)
	if err != nil {
		return err
	}
	client = zcodeClaimClient(deps.Client, account)
	attempt, err := readZCodeClaimAttempt(deps.FS, path, account, options.Claim.PlanID)
	if err != nil {
		return err
	}
	if attempt.State == "pending" {
		return unresolvedZCodeClaim()
	}
	if attempt.State == "accepted" && !options.Claim.New {
		return writeZCodeClaimResult(deps.Out, jsonOutput, attempt, true)
	}
	offer, err := currentZCodeClaimOffer(ctx, client, account, options.Claim.PlanID)
	if err != nil {
		return err
	}
	if err := describeZCodeClaim(deps.ErrOut, account, offer); err != nil {
		return err
	}
	if !options.Claim.Yes {
		if deps.In == nil {
			return errors.New("interactive ZCode claim confirmation unavailable; use --yes for explicit confirmation")
		}
		if _, err := fmt.Fprint(deps.ErrOut, "Claim this offered benefit? [y/N] "); err != nil {
			return errors.New("write ZCode claim confirmation")
		}
		answer, err := bufio.NewReader(io.LimitReader(deps.In, 32)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return errors.New("read ZCode claim confirmation")
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return errors.New("ZCode claim cancelled; no claim submitted")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	config, err := client.CaptchaConfig(ctx)
	if err != nil {
		return err
	}
	if !config.Enabled {
		return errors.New("ZCode claim verification is unavailable")
	}
	verify := deps.Verify
	if verify == nil {
		verify = zcode.Verify
	}
	proof, err := verify(ctx, config, deps.OpenURL)
	if err != nil {
		return err
	}
	defer func() { proof = zcode.VerificationProof{} }()
	if strings.TrimSpace(proof.Param) == "" || proof.Region != config.Region {
		return errors.New("ZCode verification did not return the required result")
	}
	// A label, login, device or offer may change while the user is in the browser.
	// Re-resolve the same account and require the confirmed offer to remain current.
	latest, err := selectZCodeClaimAccount(deps.Accounts, account.ID)
	if err != nil {
		return err
	}
	if latest.OAuth.UserID != account.OAuth.UserID {
		return errors.New("ZCode claim account changed during verification; no claim submitted")
	}
	account, client = latest, zcodeClaimClient(deps.Client, latest)
	current, err := currentZCodeClaimOffer(ctx, client, account, options.Claim.PlanID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(offer, current) {
		return errors.New("Z.ai's offered benefit changed during verification; confirm its current details again")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	attempt = zcodeClaimAttempt{SchemaVersion: 1, AccountID: account.ID, UserID: account.OAuth.UserID, PlanID: offer.PlanID, State: "pending", StartedAt: deps.Now().UTC()}
	if err := saveZCodeClaimAttempt(deps.FS, path, attempt); err != nil {
		return err
	}
	result, err := client.ClaimPlan(ctx, account.OAuth.AsSession(), offer.PlanID, proof)
	if err != nil {
		var operation *zcode.OperationError
		if errors.As(err, &operation) && !operation.Ambiguous {
			attempt.State = "rejected"
			if saveErr := saveZCodeClaimAttempt(deps.FS, path, attempt); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
		return errors.Join(err, unresolvedZCodeClaim())
	}
	acceptedAt := deps.Now().UTC()
	attempt.State, attempt.AcceptedAt, attempt.Result = "accepted", &acceptedAt, &result
	if err := saveZCodeClaimAttempt(deps.FS, path, attempt); err != nil {
		return errors.New("Z.ai accepted the claim but its receipt could not be saved; inspect the account balance before another claim")
	}
	return writeZCodeClaimResult(deps.Out, jsonOutput, attempt, false)
}

func selectZCodeClaimAccount(read func() ([]zaiprov.Account, error), reference string) (zaiprov.Account, error) {
	account, err := selectZCodeResetAccount(read, reference)
	if err != nil {
		return zaiprov.Account{}, errors.New("ZCode OAuth account not found or ambiguous; use its account ID and cq zai login --zcode")
	}
	return account, nil
}

func zcodeClaimClient(client zcode.Client, account zaiprov.Account) zcode.Client {
	client.Version, client.DeviceMid = zcode.ClientVersion, account.OAuth.DeviceMid
	return client
}

func currentZCodeClaimOffer(ctx context.Context, client zcode.Client, account zaiprov.Account, planID string) (zcode.PlanOffer, error) {
	offers, err := client.PlanOffers(ctx, account.OAuth.AsSession())
	if err != nil {
		return zcode.PlanOffer{}, err
	}
	for _, offer := range offers {
		if offer.PlanID == planID {
			return offer, nil
		}
	}
	return zcode.PlanOffer{}, errors.New("that plan is not currently offered by Z.ai for this account; no claim submitted")
}

func zcodeClaimAttemptPath(dir, accountID, planID string) string {
	digest := sha256.Sum256([]byte(accountID + "\x00" + planID))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
}

func readZCodeClaimAttempt(fsys fsutil.FileSystem, path string, account zaiprov.Account, planID string) (zcodeClaimAttempt, error) {
	data, err := fsutil.ReadSecureFile(fsys, path, 2<<20)
	if errors.Is(err, os.ErrNotExist) {
		return zcodeClaimAttempt{}, nil
	}
	if err != nil {
		return zcodeClaimAttempt{}, errors.New("read private ZCode claim intent or receipt")
	}
	var attempt zcodeClaimAttempt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&attempt) != nil || decoder.Decode(new(any)) != io.EOF || attempt.SchemaVersion != 1 || attempt.AccountID != account.ID || attempt.UserID != account.OAuth.UserID || attempt.PlanID != planID || attempt.StartedAt.IsZero() || attempt.State != "pending" && attempt.State != "accepted" && attempt.State != "rejected" || attempt.State == "accepted" && (attempt.AcceptedAt == nil || attempt.AcceptedAt.IsZero() || attempt.Result == nil) || attempt.State != "accepted" && (attempt.AcceptedAt != nil || attempt.Result != nil) {
		return zcodeClaimAttempt{}, errors.New("invalid ZCode claim intent or receipt; inspect the account balance before another claim")
	}
	return attempt, nil
}

func saveZCodeClaimAttempt(fsys fsutil.FileSystem, path string, attempt zcodeClaimAttempt) error {
	data, err := json.Marshal(attempt)
	if err != nil || len(data) > 2<<20 {
		return errors.New("encode ZCode claim intent or receipt")
	}
	if err := fsutil.SecureAtomicWrite(fsys, path, data); err != nil {
		return errors.New("save private ZCode claim intent or receipt")
	}
	return nil
}

func unresolvedZCodeClaim() error {
	return errors.New("previous ZCode claim outcome is unresolved; inspect its account balance before another claim; no automatic retry is supported")
}

func describeZCodeClaim(out io.Writer, account zaiprov.Account, offer zcode.PlanOffer) error {
	if _, err := fmt.Fprintf(out, "Z.ai offer %q (%q) for account %q (%q).\n", offer.Name, offer.PlanID, account.Label, account.ID); err != nil {
		return errors.New("write ZCode offer details")
	}
	if offer.Description != "" {
		if _, err := fmt.Fprintf(out, "  %q\n", offer.Description); err != nil {
			return errors.New("write ZCode offer details")
		}
	}
	if len(offer.Details) > 0 {
		var details bytes.Buffer
		if json.Indent(&details, offer.Details, "  ", "  ") != nil {
			return errors.New("invalid Z.ai offer benefit details")
		}
		if _, err := fmt.Fprintf(out, "  Benefit details from Z.ai:\n  %s\n", details.String()); err != nil {
			return errors.New("write ZCode offer benefit details")
		}
	}
	return nil
}

func writeZCodeClaimResult(out io.Writer, jsonOutput bool, attempt zcodeClaimAttempt, already bool) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(struct {
			AccountID       string             `json:"account_id"`
			PlanID          string             `json:"plan_id"`
			Accepted        bool               `json:"accepted"`
			AlreadyAccepted bool               `json:"already_accepted,omitempty"`
			Result          *zcode.ClaimResult `json:"result"`
		}{attempt.AccountID, attempt.PlanID, true, already, attempt.Result})
	}
	if already {
		_, err := fmt.Fprintf(out, "Z.ai already accepted plan %q for %s; no additional claim submitted.\n", attempt.PlanID, attempt.AccountID)
		return err
	}
	_, err := fmt.Fprintf(out, "Z.ai accepted plan %q for %s.\n", attempt.PlanID, attempt.AccountID)
	return err
}
