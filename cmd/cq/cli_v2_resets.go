package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/history"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ResetCredit struct {
	ID          string                      `json:"id"`
	ResetType   codexprov.ResetType         `json:"reset_type"`
	Status      codexprov.ResetCreditStatus `json:"status"`
	GrantedAt   time.Time                   `json:"granted_at"`
	ExpiresAt   *time.Time                  `json:"expires_at"`
	Title       *string                     `json:"title"`
	Description *string                     `json:"description"`
	Supported   bool                        `json:"supported"`
}
type ResetInventoryError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	EntryIndex *int   `json:"entry_index"`
}
type ResetAccountInventory struct {
	AccountReference *string               `json:"account_reference"`
	AccountID        *string               `json:"account_id"`
	Email            *string               `json:"email"`
	Credits          []ResetCredit         `json:"credits"`
	AvailableCount   *int                  `json:"available_count"`
	Errors           []ResetInventoryError `json:"errors"`
}
type ResetRestoredWindow struct {
	Name       quota.WindowName `json:"name"`
	Percentage float64          `json:"percentage"`
}
type ResetScheduleItem struct {
	AccountReference  string                            `json:"account_reference"`
	AccountEmail      *string                           `json:"account_email"`
	AccountID         *string                           `json:"account_id"`
	CreditID          string                            `json:"credit_id"`
	UseAt             *time.Time                        `json:"use_at"`
	UseBy             *time.Time                        `json:"use_by"`
	Status            aggregate.ResetScheduleStatus     `json:"status"`
	Confidence        aggregate.ResetScheduleConfidence `json:"confidence"`
	RestoredPct       []ResetRestoredWindow             `json:"restored_pct"`
	AvoidedGapSeconds int64                             `json:"avoided_gap_seconds"`
	ReasonCodes       []aggregate.ResetScheduleReason   `json:"reason_codes"`
}
type ResetScheduleBlocker struct {
	Code             string  `json:"code"`
	AccountReference *string `json:"account_reference"`
	AccountEmail     *string `json:"account_email"`
	AccountID        *string `json:"account_id"`
}
type ResetSchedule struct {
	GeneratedAt time.Time                         `json:"generated_at"`
	Horizon     time.Time                         `json:"horizon"`
	Complete    bool                              `json:"complete"`
	Exact       bool                              `json:"exact"`
	Confidence  aggregate.ResetScheduleConfidence `json:"confidence"`
	Items       []ResetScheduleItem               `json:"items"`
	Objective   codexResetScheduleObjectiveJSON   `json:"objective"`
	Blockers    []ResetScheduleBlocker            `json:"blockers"`
}

// T27 installs these leaves into the executable's canonical dispatcher.
func lookupV2ResetInspection(path string) (cli.Handler, bool) {
	switch path {
	case "codex reset list", "codex reset recommend":
		return handleV2ResetInspection, true
	}
	return nil, false
}

type v2ResetDependencies struct {
	app   *app.CodexResetApp
	close func() error
}
type v2ResetFactory func(context.Context, bool) (v2ResetDependencies, error)

func handleV2ResetInspection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2ResetInspectionWithFactory(ctx, inv, session, newV2ResetDependencies)
}
func handleV2ResetInspectionWithFactory(parent context.Context, inv cli.Invocation, session *cli.Session, factory v2ResetFactory) cli.Outcome {
	timeout, _ := time.ParseDuration(inv.Options["timeout"][0])
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type observation struct {
		deps v2ResetDependencies
		err  error
	}
	done := make(chan observation)
	if ctx.Err() == nil {
		go func() {
			r := observation{}
			defer func() {
				if recover() != nil {
					r.err = errors.New("reset dependencies unavailable")
				}
				select {
				case done <- r:
				case <-ctx.Done():
					if r.deps.close != nil {
						_ = closeV2ResetDependencies(r.deps.close)
					}
				}
			}()
			r.deps, r.err = factory(ctx, inv.Path == "codex reset recommend")
		}()
	}
	var observed observation
	select {
	case observed = <-done:
	case <-ctx.Done():
		return v2ResetFailure(ctx.Err())
	}
	if observed.err != nil {
		if observed.deps.close != nil {
			go func() { _ = closeV2ResetDependencies(observed.deps.close) }()
		}
		return v2ResetFailure(observed.err)
	}
	outcome := handleV2ResetInspectionWithApp(ctx, inv, session, observed.deps.app)
	if observed.deps.close != nil {
		closed := make(chan error, 1)
		go func() { closed <- closeV2ResetDependencies(observed.deps.close) }()
		select {
		case err := <-closed:
			if err != nil && outcome.ExitCode == 0 {
				failure := v2ResetFailure(err)
				outcome.ExitCode, outcome.Errors = failure.ExitCode, failure.Errors
			}
		case <-ctx.Done():
			failure := v2ResetFailure(ctx.Err())
			outcome.ExitCode, outcome.Errors = failure.ExitCode, failure.Errors
		}
	}
	return outcome
}

func closeV2ResetDependencies(close func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("reset cleanup unavailable")
		}
	}()
	return close()
}

// Apply the complete-inventory rules before resolving any reset selector.
type v2ResetInventory struct{ codexprov.CredentialInventory }

func (i v2ResetInventory) List(ctx context.Context) (codexprov.Inventory, error) {
	return (&codexprov.Accounts{Inventory: i.CredentialInventory}).Inspect(ctx)
}

func newV2ResetDependencies(ctx context.Context, recommend bool) (v2ResetDependencies, error) {
	fs := fsutil.OSFileSystem{}
	client := httputil.NewClient(10*time.Second, version)
	return newV2ResetDependenciesWithClient(ctx, recommend, fs, client)
}

func newV2ResetDependenciesWithClient(ctx context.Context, recommend bool, fs fsutil.DurableFileSystem, client httputil.Doer) (v2ResetDependencies, error) {
	control, err := codexprov.OpenDefaultCanonicalCredentialRefreshControl(ctx, fs, client)
	if err != nil {
		return v2ResetDependencies{}, err
	}
	return newV2ResetDependenciesWithControl(recommend, fs, client, control)
}

func newV2ResetDependenciesWithControl(recommend bool, fs fsutil.DurableFileSystem, client httputil.Doer, control *codexprov.CredentialControl) (v2ResetDependencies, error) {
	deps := v2ResetDependencies{close: control.Close}
	store, err := codexprov.NewManagedStore(fs)
	if err != nil {
		return deps, err
	}
	backend := &codexprov.ResetBackend{Inventory: v2ResetInventory{control.CanonicalAdmin()}, Resolver: control, Refresh: control.CanonicalAdmin(), Aliases: func() (codexprov.AccountAliasIndex, error) {
		return (codexprov.Registry{FS: fs, Home: store.Home}).AccountAliasIndex()
	}, Credits: codexprov.ResetCreditClient{HTTP: client}, Now: time.Now}
	deps.app = &app.CodexResetApp{Backend: backend, Clock: systemClock{}}
	if recommend {
		roots, err := userdirs.Default(userdirs.CacheRoot)
		if err != nil {
			return deps, err
		}
		store, err := history.New(fs, roots.Cache)
		if err != nil {
			return deps, err
		}
		deps.app.History = store
		deps.app.Usage, err = codexprov.NewWithCredentialAuthority(client, backend.Inventory, control, control.CanonicalAdmin())
		if err != nil {
			return deps, err
		}
	}
	return deps, nil
}
func handleV2ResetInspectionWithApp(parent context.Context, inv cli.Invocation, _ *cli.Session, a *app.CodexResetApp) (outcome cli.Outcome) {
	timeout, _ := time.ParseDuration(inv.Options["timeout"][0])
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// Workers retain this observer after the command returns. Detachment discards
	// late diagnostics while keeping provider/history legacy stderr paths disabled.
	var mu sync.Mutex
	var warnings []cli.Diagnostic
	detached := false
	ctx = provider.WithObservation(ctx, provider.Observation{Warning: func(code, message string) {
		mu.Lock()
		defer mu.Unlock()
		if !detached && ctx.Err() == nil {
			warnings = append(warnings, cli.Diagnostic{Code: code, Message: message})
		}
	}})
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		detached = true
		sort.Slice(warnings, func(i, j int) bool {
			if warnings[i].Code != warnings[j].Code {
				return warnings[i].Code < warnings[j].Code
			}
			return warnings[i].Message < warnings[j].Message
		})
		for i, warning := range warnings {
			if i == 0 || warning.Code != warnings[i-1].Code || warning.Message != warnings[i-1].Message {
				outcome.Warnings = append(outcome.Warnings, warning)
			}
		}
	}()
	if inv.Path == "codex reset recommend" {
		schedule, err := a.Recommend(ctx)
		dto := v2ResetSchedule(schedule)
		out := cli.Outcome{}
		if err != nil {
			out = v2ResetFailure(err)
		}
		if ctx.Err() != nil {
			out = v2ResetFailure(ctx.Err())
		}
		if err != nil && !dto.Complete && len(dto.Blockers) == 0 {
			dto.Blockers = append(dto.Blockers, ResetScheduleBlocker{Code: "credits_unavailable"})
		}
		out.Data, _ = cli.EncodeData(struct {
			Schedule ResetSchedule `json:"schedule"`
		}{dto})
		var human strings.Builder
		fmt.Fprintf(&human, "Reset recommendation: complete=%t, exact=%t, confidence=%s.\nHorizon: %s.\n", dto.Complete, dto.Exact, dto.Confidence, dto.Horizon.Format(time.RFC3339))
		for _, item := range dto.Items {
			fmt.Fprintf(&human, "%s\t%s\t%s\t%s\t%s\n", cli.HumanValue(item.AccountReference), cli.HumanValue(item.CreditID), item.Status, v2ResetTimeText(item.UseAt, "not scheduled"), v2ResetTimeText(item.UseBy, "no expiry"))
		}
		out.Human = human.String()
		return out
	}
	reference := ""
	if values := inv.Arguments["account"]; len(values) > 0 {
		reference = values[0]
	}
	result, err := a.List(ctx, reference)
	if err != nil && len(result.Accounts) == 0 && ctx.Err() == nil {
		return v2ResetFailure(err)
	}
	rows, complete := v2ResetInventories(result)
	out := cli.Outcome{}
	if !complete {
		out = v2ResetDiagnostic(8, "codex_reset_inventory_partial", "Reset-credit inventory is incomplete.")
	}
	if err != nil && complete {
		out = v2ResetFailure(err)
	}
	if ctx.Err() != nil {
		complete = false
		out = v2ResetFailure(ctx.Err())
	}
	out.Data, _ = cli.EncodeData(struct {
		Accounts []ResetAccountInventory `json:"accounts"`
		Complete bool                    `json:"complete"`
	}{rows, complete})
	var human strings.Builder
	fmt.Fprintf(&human, "Reset inventories: %d; complete=%t.\n", len(rows), complete)
	for _, row := range rows {
		reference := "—"
		if row.AccountReference != nil {
			reference = cli.HumanValue(*row.AccountReference)
		}
		for _, credit := range row.Credits {
			fmt.Fprintf(&human, "%s\t%s\t%s\t%s\n", reference, cli.HumanValue(credit.ID), cli.HumanValue(string(credit.Status)), v2ResetTimeText(credit.ExpiresAt, "never"))
		}
	}
	out.Human = human.String()
	return out
}
func v2ResetDiagnostic(exit int, code, message string) cli.Outcome {
	return cli.Outcome{ExitCode: exit, Errors: []cli.Diagnostic{{Code: code, Message: message}}}
}
func v2ResetFailure(err error) cli.Outcome {
	if errors.Is(err, context.Canceled) {
		return v2ResetDiagnostic(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return v2ResetDiagnostic(7, "codex_reset_timeout", "Codex reset command timed out.")
	}
	var environment *userdirs.EnvironmentError
	if errors.As(err, &environment) {
		return v2ResetDiagnostic(environment.ExitCode, environment.Code, environment.Error())
	}
	var reference *codexprov.AccountReferenceError
	if errors.As(err, &reference) {
		switch reference.Code {
		case codexprov.AccountReferenceEmpty:
			return v2ResetDiagnostic(2, "account_reference_empty", "Account reference must not be empty.")
		case codexprov.AccountReferenceMissing:
			return v2ResetDiagnostic(3, "account_not_found", "Account reference does not resolve.")
		case codexprov.AccountReferenceAmbiguous:
			return v2ResetDiagnostic(6, "account_ambiguous", "Account reference is ambiguous; use an exact account key.")
		default:
			return v2ResetDiagnostic(6, "account_unstable", "Account reference resolves to an unstable account.")
		}
	}
	var appError *app.CodexResetError
	if errors.As(err, &appError) && appError.Code == "recommendation_incomplete" {
		return v2ResetDiagnostic(8, "codex_reset_recommendation_incomplete", "Reset recommendation is incomplete; resolve the reported blockers.")
	}
	var httpError *codexprov.ResetHTTPError
	if errors.As(err, &httpError) {
		if httpError.Status == 401 || httpError.Status == 403 {
			return v2ResetDiagnostic(5, "codex_reset_auth_failed", "Codex reset authentication failed.")
		}
		return v2ResetDiagnostic(1, "codex_reset_upstream_failed", "Codex reset request failed.")
	}
	return v2ResetDiagnostic(4, "codex_reset_credentials_unavailable", "Codex reset credentials are unavailable.")
}
func v2ResetInventories(result app.CodexResetListResult) ([]ResetAccountInventory, bool) {
	rows := make([]ResetAccountInventory, 0, len(result.Accounts))
	complete := true
	for _, account := range result.Accounts {
		row := ResetAccountInventory{AccountReference: v2AccountString(string(account.AccountKey)), AccountID: v2AccountString(account.AccountID), Email: v2AccountString(account.Email), Credits: []ResetCredit{}, Errors: []ResetInventoryError{}}
		if account.Unselectable {
			row.AccountReference = nil
		}
		if account.ReadError == nil {
			count := account.Inventory.AvailableCount
			row.AvailableCount = &count
		}
		for _, credit := range account.Credits {
			row.Credits = append(row.Credits, ResetCredit{ID: credit.ID, ResetType: credit.ResetType, Status: credit.Status, GrantedAt: credit.GrantedAt.UTC(), ExpiresAt: v2ResetTime(credit.ExpiresAt), Title: v2AccountString(credit.Title), Description: v2AccountString(credit.Description), Supported: credit.ResetType == codexprov.ResetTypeCodexRateLimits})
		}
		for _, entry := range account.Inventory.EntryErrors {
			index := entry.Index
			row.Errors = append(row.Errors, v2ResetInventoryError(entry.Code, &index))
		}
		if account.ReadError != nil {
			code := "credits_unavailable"
			var httpError *codexprov.ResetHTTPError
			var inventoryError *codexprov.ResetCreditInventoryError
			if errors.As(account.ReadError, &httpError) && (httpError.Status == 401 || httpError.Status == 403) {
				code = "auth_failed"
			}
			if errors.As(account.ReadError, &inventoryError) {
				code = "invalid_inventory"
				if inventoryError.Code == "invalid_credit_entries" && len(row.Errors) > 0 {
					code = ""
					count := account.Inventory.AvailableCount
					row.AvailableCount = &count
				}
			}
			if code != "" {
				row.Errors = append(row.Errors, v2ResetInventoryError(code, nil))
			}
		}
		if len(row.Errors) > 0 {
			complete = false
		}
		sort.SliceStable(row.Credits, func(i, j int) bool {
			a, b := row.Credits[i], row.Credits[j]
			if a.ExpiresAt == nil && b.ExpiresAt != nil {
				return false
			}
			if a.ExpiresAt != nil && b.ExpiresAt == nil {
				return true
			}
			if a.ExpiresAt != nil && b.ExpiresAt != nil && !a.ExpiresAt.Equal(*b.ExpiresAt) {
				return a.ExpiresAt.Before(*b.ExpiresAt)
			}
			return a.ID < b.ID
		})
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.AccountReference == nil && b.AccountReference != nil {
			return false
		}
		if a.AccountReference != nil && b.AccountReference == nil {
			return true
		}
		if v2ResetString(a.AccountReference) != v2ResetString(b.AccountReference) {
			return v2ResetString(a.AccountReference) < v2ResetString(b.AccountReference)
		}
		if v2ResetString(a.AccountID) != v2ResetString(b.AccountID) {
			return v2ResetString(a.AccountID) < v2ResetString(b.AccountID)
		}
		return v2ResetString(a.Email) < v2ResetString(b.Email)
	})
	return rows, complete
}
func v2ResetInventoryError(code string, index *int) ResetInventoryError {
	message := "Reset-credit entry is invalid."
	switch code {
	case "auth_failed":
		message = "Codex reset authentication failed."
	case "credits_unavailable":
		message = "Reset-credit inventory is unavailable."
	case "invalid_inventory":
		message = "Reset-credit inventory is invalid."
	case "invalid_id":
		message = "Reset-credit ID is invalid."
	case "missing_reset_type":
		message = "Reset-credit type is missing."
	case "missing_status":
		message = "Reset-credit status is missing."
	case "invalid_granted_at":
		message = "Reset-credit grant time is invalid."
	case "invalid_expires_at":
		message = "Reset-credit expiry is invalid."
	default:
		code = "invalid_entry"
	}
	return ResetInventoryError{Code: code, Message: message, EntryIndex: index}
}
func v2ResetSchedule(schedule aggregate.ResetSchedule) ResetSchedule {
	legacy := publicCodexResetSchedule(schedule)
	result := ResetSchedule{GeneratedAt: schedule.GeneratedAt.UTC(), Horizon: schedule.Horizon.UTC(), Complete: schedule.Complete, Exact: schedule.Exact, Confidence: schedule.Confidence, Items: []ResetScheduleItem{}, Objective: legacy.Objective, Blockers: []ResetScheduleBlocker{}}
	if result.Horizon.IsZero() {
		result.Horizon = result.GeneratedAt.Add(7 * 24 * time.Hour)
	}
	if result.Confidence == "" {
		result.Confidence = aggregate.ResetConfidenceLow
	}
	reasons := []aggregate.ResetScheduleReason{aggregate.ResetReasonGapAvoidance, aggregate.ResetReasonExpiryPressure, aggregate.ResetReasonNaturalReset, aggregate.ResetReasonWasteReduction, aggregate.ResetReasonRateFallback}
	for _, item := range schedule.Items {
		row := ResetScheduleItem{AccountReference: item.AccountReference, AccountEmail: v2AccountString(item.AccountEmail), AccountID: v2AccountString(item.AccountID), CreditID: item.CreditID, UseBy: v2ResetTime(item.CreditExpiresAt), Status: item.Status, Confidence: item.Confidence, RestoredPct: []ResetRestoredWindow{}, AvoidedGapSeconds: item.AvoidedGapSec, ReasonCodes: []aggregate.ResetScheduleReason{}}
		if schedule.Complete && !item.UseAt.IsZero() {
			row.UseAt = v2ResetTime(&item.UseAt)
		}
		for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
			if value, ok := item.RestoredPct[name]; ok {
				row.RestoredPct = append(row.RestoredPct, ResetRestoredWindow{name, value})
			}
		}
		for _, reason := range reasons {
			for _, found := range item.ReasonCodes {
				if reason == found {
					row.ReasonCodes = append(row.ReasonCodes, reason)
					break
				}
			}
		}
		result.Items = append(result.Items, row)
	}
	for _, blocker := range schedule.Blockers {
		code := blocker.Code
		switch code {
		case "usage_missing":
			code = "usage_account_missing"
		case "usage_duplicate":
			code = "usage_account_ambiguous"
		case "auth_expired":
			code = "auth_failed"
		case "usage_unavailable", "usage_windows_invalid", "credits_unavailable":
		default:
			code = "inventory_invalid"
		}
		result.Blockers = append(result.Blockers, ResetScheduleBlocker{code, v2AccountString(blocker.AccountReference), v2AccountString(blocker.AccountEmail), v2AccountString(blocker.AccountID)})
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if a.UseAt == nil && b.UseAt != nil {
			return false
		}
		if a.UseAt != nil && b.UseAt == nil {
			return true
		}
		if a.UseAt != nil && b.UseAt != nil && !a.UseAt.Equal(*b.UseAt) {
			return a.UseAt.Before(*b.UseAt)
		}
		if a.AccountReference != b.AccountReference {
			return a.AccountReference < b.AccountReference
		}
		return a.CreditID < b.CreditID
	})
	sort.SliceStable(result.Blockers, func(i, j int) bool {
		a, b := result.Blockers[i], result.Blockers[j]
		if v2ResetString(a.AccountReference) != v2ResetString(b.AccountReference) {
			return v2ResetString(a.AccountReference) < v2ResetString(b.AccountReference)
		}
		return a.Code < b.Code
	})
	return result
}
func v2ResetString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func v2ResetTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
func v2ResetTimeText(value *time.Time, fallback string) string {
	if value == nil {
		return fallback
	}
	return value.UTC().Format(time.RFC3339)
}
