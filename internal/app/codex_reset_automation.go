package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/aggregate"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

var errResetScheduleChanged = errors.New("reset schedule changed; preview again")
var errResetScheduleNoWork = errors.New("no due reset schedule work")

// CodexResetScheduleChange describes one user edit. Preparing never saves or consumes.
type CodexResetScheduleChange struct {
	Action     string
	Reference  string
	CreditID   string
	EntryID    string
	At         *time.Time
	FollowMode bool
	Mode       codex.ResetAutomationMode
	Replace    bool
}

type CodexResetSchedulePlan struct {
	State                codex.ResetAutomationState
	ConfirmationRequired bool
	expectedRevision     uint64
	state                codex.ResetAutomationState
}

func (a *CodexResetApp) PrepareSchedule(ctx context.Context, change CodexResetScheduleChange) (CodexResetSchedulePlan, error) {
	state, err := a.Schedule.Load()
	if err != nil {
		return CodexResetSchedulePlan{}, err
	}
	revision := state.Revision
	now := a.now()
	confirmation := false
	invalidate := false
	switch change.Action {
	case "accept":
		for _, e := range state.Entries {
			if !e.Terminal() && e.State != codex.ResetAutomationRunning && e.State != codex.ResetAutomationRetrying && !change.Replace {
				return CodexResetSchedulePlan{}, errors.New("schedule already has pending entries; use --replace")
			}
		}
		recommendation, err := a.Recommend(ctx)
		if err != nil || !recommendation.Complete {
			return CodexResetSchedulePlan{}, errors.Join(errors.New("complete fresh recommendation required"), err)
		}
		snapshot, err := a.snapshot(ctx)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		entries := make([]codex.ResetAutomationEntry, 0)
		for _, item := range recommendation.Items {
			if item.Status != aggregate.ResetScheduled && item.Status != aggregate.ResetDueNow {
				continue
			}
			var matches []codex.ResetAccount
			for _, account := range snapshot.Accounts {
				if (item.AccountID != "" && account.AccountID == item.AccountID) || (item.AccountID == "" && account.Email == item.AccountEmail) {
					matches = append(matches, account)
				}
			}
			if len(matches) != 1 {
				return CodexResetSchedulePlan{}, errors.New("recommendation account identity changed")
			}
			account := matches[0]
			pending, err := a.Attempts.Pending(account.AccountKey)
			if err != nil {
				return CodexResetSchedulePlan{}, err
			}
			if len(pending) > 0 {
				continue
			} // Unresolved accounts cannot authorise another credit.
			busy := false
			for _, e := range state.Entries {
				if e.AccountKey == account.AccountKey && (e.State == codex.ResetAutomationRunning || e.State == codex.ResetAutomationRetrying) {
					busy = true
				}
			}
			if busy {
				continue
			}
			inventory, err := a.Backend.ListCredits(ctx, account)
			if err != nil {
				return CodexResetSchedulePlan{}, err
			}
			selection, err := codex.SelectResetCredit(now, inventory.Credits, nil, item.CreditID)
			if err != nil {
				return CodexResetSchedulePlan{}, err
			}
			if err := validateAutomationTime(&item.UseAt, selection.Credit.ExpiresAt); err != nil {
				return CodexResetSchedulePlan{}, err
			}
			entry, err := newResetAutomationEntry(account, selection.Credit, now)
			if err != nil {
				return CodexResetSchedulePlan{}, err
			}
			entry.At = resetTimePointer(item.UseAt)
			entry.UseBy = resetTimePointer(item.UseBy)
			entry.Source = "recommendation"
			entry.Confidence = string(item.Confidence)
			entries = append(entries, entry)
		}
		if len(entries) == 0 {
			return CodexResetSchedulePlan{}, errors.New("recommendation has no actionable unclaimed credits")
		}
		existed := state.Revision != 0
		for i := range state.Entries {
			e := &state.Entries[i]
			if !e.Terminal() && e.State != codex.ResetAutomationRunning && e.State != codex.ResetAutomationRetrying {
				e.State = codex.ResetAutomationCancelled
				e.UpdatedAt = now
			}
		}
		state.Entries = append(state.Entries, entries...)
		if !existed {
			state.Enabled = true
		}
		if change.Mode != "" {
			state.Mode = change.Mode
		}
		state.PlanValid = true
		state.PlanError = ""
		state.LastReplanAt = resetTimePointer(now)
		state.NextReplanAt = resetTimePointer(now.Add(5 * time.Minute))
		confirmation = true
		// The accepted portfolio can differ from the advisory portfolio (for example,
		// an unresolved credit was excluded). Adaptive plans must use its exact set.
		invalidate = state.Mode == codex.ResetAutomationAdaptive
	case "add":
		snapshot, err := a.snapshot(ctx)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		account, err := snapshot.ResolveReference(change.Reference)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		inventory, err := a.Backend.ListCredits(ctx, account)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		pending, err := a.Attempts.Pending(account.AccountKey)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		if len(pending) > 0 {
			return CodexResetSchedulePlan{}, errors.New("account has an unresolved reset attempt")
		}
		available := inventory.Credits[:0:0]
		for _, credit := range inventory.Credits {
			duplicate := false
			for _, e := range state.Entries {
				if e.AccountKey == account.AccountKey && e.CreditID == credit.ID && !e.Terminal() {
					duplicate = true
				}
			}
			if !duplicate {
				available = append(available, credit)
			}
		}
		selection, err := codex.SelectResetCredit(now, available, nil, change.CreditID)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		if err := validateManualAutomationTime(change.At, selection.Credit.ExpiresAt, now); err != nil {
			return CodexResetSchedulePlan{}, err
		}
		entry, err := newResetAutomationEntry(account, selection.Credit, now)
		if err != nil {
			return CodexResetSchedulePlan{}, err
		}
		entry.At = resetTimePointer(*change.At)
		entry.Pinned = true
		entry.Source = "manual"
		state.Entries = append(state.Entries, entry)
		confirmation = state.Enabled
		invalidate = true
	case "set":
		entry := state.Entry(change.EntryID)
		if entry == nil || entry.Terminal() || entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying {
			return CodexResetSchedulePlan{}, errors.New("entry cannot be edited")
		}
		if (change.At == nil) == !change.FollowMode {
			return CodexResetSchedulePlan{}, errors.New("choose exactly one of --at or --follow-mode")
		}
		if change.FollowMode {
			entry.Pinned = false
			if state.Mode == codex.ResetAutomationAdaptive {
				entry.At = nil
			}
		} else {
			if err := validateManualAutomationTime(change.At, entry.ExpiresAt, now); err != nil {
				return CodexResetSchedulePlan{}, err
			}
			entry.At = resetTimePointer(*change.At)
			entry.Pinned = true
		}
		entry.State = codex.ResetAutomationPending
		entry.LastError = ""
		entry.UpdatedAt = now
		entry.NextAttemptAt = nil
		confirmation = state.Enabled
		invalidate = true
	case "mode":
		if change.Mode != codex.ResetAutomationFixed && change.Mode != codex.ResetAutomationAdaptive {
			return CodexResetSchedulePlan{}, errors.New("mode must be fixed or adaptive")
		}
		if state.Mode == codex.ResetAutomationAdaptive && change.Mode == codex.ResetAutomationFixed {
			state.PlanError = ""
			for i := range state.Entries {
				e := &state.Entries[i]
				if e.Terminal() || e.Pinned || e.State == codex.ResetAutomationRunning || e.State == codex.ResetAutomationRetrying {
					continue
				}
				if !state.PlanValid {
					e.At = nil
				}
				if e.At == nil {
					e.State = codex.ResetAutomationBlocked
					e.LastError = "time_required"
				} else {
					e.State = codex.ResetAutomationPending
					e.LastError = ""
				}
			}
		}
		state.Mode = change.Mode
		confirmation = state.Enabled
		invalidate = true
	case "enable":
		state.Enabled = true
		confirmation = true
		invalidate = true
	case "disable":
		state.Enabled = false
	case "remove", "cancel":
		found := change.Action == "cancel"
		for i := range state.Entries {
			entry := &state.Entries[i]
			if change.Action == "remove" && entry.ID != change.EntryID {
				continue
			}
			found = true
			if !entry.Terminal() && entry.State != codex.ResetAutomationRunning && entry.State != codex.ResetAutomationRetrying {
				entry.State = codex.ResetAutomationCancelled
				entry.UpdatedAt = now
			}
		}
		if !found {
			return CodexResetSchedulePlan{}, errors.New("entry not found")
		}
		if change.Action == "cancel" {
			state.Enabled = false
		}
		invalidate = true
	default:
		return CodexResetSchedulePlan{}, errors.New("unknown schedule action")
	}
	if state.Mode != codex.ResetAutomationFixed && state.Mode != codex.ResetAutomationAdaptive {
		return CodexResetSchedulePlan{}, errors.New("mode must be fixed or adaptive")
	}
	if invalidate {
		state.PlanValid = false
		state.NextReplanAt = nil
	}
	if invalidate && state.Enabled && state.Mode == codex.ResetAutomationAdaptive && change.Action != "remove" && change.Action != "cancel" {
		a.replanAutomationState(ctx, &state)
	}
	return CodexResetSchedulePlan{State: state, ConfirmationRequired: confirmation, expectedRevision: revision, state: state}, nil
}

func (a *CodexResetApp) ApplySchedule(ctx context.Context, plan CodexResetSchedulePlan) (codex.ResetAutomationState, error) {
	return a.Schedule.Update(ctx, func(state *codex.ResetAutomationState) error {
		if state.Revision != plan.expectedRevision {
			return errResetScheduleChanged
		}
		*state = plan.state
		return nil
	})
}

func newResetAutomationEntry(account codex.ResetAccount, credit codex.ResetCredit, now time.Time) (codex.ResetAutomationEntry, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return codex.ResetAutomationEntry{}, err
	}
	return codex.ResetAutomationEntry{ID: hex.EncodeToString(id[:]), AccountKey: account.AccountKey, AccountID: account.AccountID, Email: account.Email, CreditID: credit.ID, ExpiresAt: credit.ExpiresAt, State: codex.ResetAutomationPending, UpdatedAt: now}, nil
}
func resetTimePointer(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}
func validateAutomationTime(at, expiry *time.Time) error {
	if at == nil || at.IsZero() {
		return errors.New("valid --at timestamp required")
	}
	if expiry != nil && !at.Before(*expiry) {
		return errors.New("scheduled time must precede credit expiry")
	}
	return nil
}

func validateManualAutomationTime(at, expiry *time.Time, now time.Time) error {
	if err := validateAutomationTime(at, expiry); err != nil {
		return err
	}
	if at.Before(now) {
		return errors.New("manual reset time must not be in the past")
	}
	return nil
}

// replanAutomationState changes only accepted, unresolved-free pending entries.
// The caller fences persistence against the revision that supplied these inputs.
func (a *CodexResetApp) replanAutomationState(ctx context.Context, state *codex.ResetAutomationState) {
	now := a.now()
	unresolved := make(map[codex.AccountKey]bool)
	for _, entry := range state.Entries {
		if entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying {
			unresolved[entry.AccountKey] = true
		}
		if entry.Terminal() || a.Attempts == nil {
			continue
		}
		pending, err := a.Attempts.Pending(entry.AccountKey)
		if err != nil {
			state.PlanValid = false
			state.PlanError = "attempts_unavailable"
			state.LastReplanAt = resetTimePointer(now)
			state.NextReplanAt = resetTimePointer(now.Add(5 * time.Minute))
			for i := range state.Entries {
				e := &state.Entries[i]
				if !e.Terminal() && !e.Pinned && e.State != codex.ResetAutomationRunning && e.State != codex.ResetAutomationRetrying {
					e.State = codex.ResetAutomationBlocked
					e.LastError = state.PlanError
				}
			}
			return
		}
		if len(pending) > 0 {
			unresolved[entry.AccountKey] = true
		}
	}

	constraints := &aggregate.ResetScheduleConstraints{}
	for _, entry := range state.Entries {
		if unresolved[entry.AccountKey] || entry.Terminal() || entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying {
			continue
		}
		if entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
			continue
		}
		constraints.AllowedCredits = append(constraints.AllowedCredits, aggregate.ResetScheduleCreditRef{AccountKey: string(entry.AccountKey), CreditID: entry.CreditID})
		if entry.Pinned && entry.At != nil {
			constraints.FixedUses = append(constraints.FixedUses, aggregate.ResetScheduleFixedUse{AccountKey: string(entry.AccountKey), CreditID: entry.CreditID, At: *entry.At})
		}
	}
	recommendation, err := a.recommendConstrained(ctx, true, constraints)
	state.LastReplanAt = resetTimePointer(now)
	state.NextReplanAt = resetTimePointer(now.Add(5 * time.Minute))
	state.PlanValid = err == nil && recommendation.Complete
	state.PlanError = ""
	if !state.PlanValid {
		state.PlanError = "recommendation_incomplete"
	}
	for i := range state.Entries {
		entry := &state.Entries[i]
		if unresolved[entry.AccountKey] || entry.Terminal() || entry.Pinned || entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying {
			continue
		}
		if !state.PlanValid {
			entry.State = codex.ResetAutomationBlocked
			entry.LastError = state.PlanError
			continue
		}
		entry.At = nil
		entry.UseBy = nil
		entry.State = codex.ResetAutomationPending
		entry.LastError = "not_yet_useful"
		for _, item := range recommendation.Items {
			if item.CreditID == entry.CreditID && ((entry.AccountID != "" && item.AccountID == entry.AccountID) || (entry.AccountID == "" && item.AccountEmail == entry.Email)) {
				entry.Confidence = string(item.Confidence)
				if item.Status == aggregate.ResetScheduled || item.Status == aggregate.ResetDueNow {
					entry.At = resetTimePointer(item.UseAt)
					entry.UseBy = resetTimePointer(item.UseBy)
					entry.LastError = ""
				}
				break
			}
		}
		entry.UpdatedAt = now
	}
}

// StepSchedule performs at most one reset. Its cross-process lock also protects
// restart recovery; schedule edits use a separate short-lived store lock.
func (a *CodexResetApp) StepSchedule(ctx context.Context) error {
	state, err := a.Schedule.Load()
	if err != nil {
		return err
	}
	if err := a.cleanupScheduleAttempts(state); err != nil {
		return err
	}
	if !state.Enabled {
		return nil
	}
	hasWork := false
	for _, e := range state.Entries {
		if !e.Terminal() {
			hasWork = true
			break
		}
	}
	if !hasWork {
		return a.cleanupScheduleAttempts(state)
	}

	lock, err := a.Schedule.AcquireExecutor()
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return err
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err = a.Schedule.Load()
	if err != nil {
		return err
	}
	if !state.Enabled {
		return nil
	}
	if err := a.cleanupScheduleAttempts(state); err != nil {
		return err
	}

	now := a.now()
	needsReplan := state.NextReplanAt == nil || !state.NextReplanAt.After(now)
	if state.PlanValid {
		for _, entry := range state.Entries {
			if !entry.Terminal() && !entry.Pinned && entry.State != codex.ResetAutomationRunning && entry.State != codex.ResetAutomationRetrying && entry.At != nil && !entry.At.After(now) {
				needsReplan = true
			}
		}
	}
	if state.Mode == codex.ResetAutomationAdaptive && needsReplan {
		revision := state.Revision
		a.replanAutomationState(ctx, &state)
		state, err = a.Schedule.Update(ctx, func(current *codex.ResetAutomationState) error {
			if current.Revision != revision || !current.Enabled || current.Mode != codex.ResetAutomationAdaptive {
				return errResetScheduleChanged
			}
			*current = state
			return nil
		})
		if errors.Is(err, errResetScheduleChanged) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	now = a.now() // A fresh due-now plan was generated after the initial clock read.
	entries := append([]codex.ResetAutomationEntry(nil), state.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].At == nil {
			return false
		}
		if entries[j].At == nil {
			return true
		}
		return entries[i].At.Before(*entries[j].At)
	})
	for _, entry := range entries {
		if entry.Terminal() {
			continue
		}
		retry := entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying
		if a.Attempts == nil {
			return errors.New("reset attempt storage unavailable")
		}
		pending, err := a.Attempts.Pending(entry.AccountKey)
		if err != nil {
			return err
		}
		for _, attempt := range pending {
			if attempt.CreditID == entry.CreditID {
				retry = true
			}
		}
		if entry.NextAttemptAt != nil && entry.NextAttemptAt.After(now) {
			continue
		}
		if !retry && entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
			return a.finishAutomationEntry(ctx, state.Revision, entry.ID, codex.ResetAutomationMissed, "credit_expired")
		}
		if !retry && (entry.At == nil || entry.At.After(now)) {
			continue
		}
		if !retry && state.Mode == codex.ResetAutomationAdaptive && !entry.Pinned && !state.PlanValid {
			continue
		}
		return a.executeAutomationEntry(ctx, state, entry)
	}
	return nil
}

func (a *CodexResetApp) cleanupScheduleAttempts(state codex.ResetAutomationState) error {
	if a.Attempts == nil {
		return nil
	}
	for _, entry := range state.Entries {
		if !entry.Terminal() || entry.Outcome == "" {
			continue
		}
		pending, err := a.Attempts.Pending(entry.AccountKey)
		if err != nil {
			return err
		}
		for _, attempt := range pending {
			if attempt.CreditID == entry.CreditID {
				if err := a.Attempts.Remove(entry.AccountKey, entry.CreditID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *CodexResetApp) executeAutomationEntry(ctx context.Context, state codex.ResetAutomationState, entry codex.ResetAutomationEntry) error {
	// A crash between the schedule claim and attempt creation, or an older
	// manual client, can leave a durable claim without its replay record.
	// Rebuild only that claimed credit's deterministic request; never select a replacement.
	if entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying {
		pending, err := a.Attempts.Pending(entry.AccountKey)
		if err != nil {
			return a.blockAutomationEntry(ctx, state.Revision, entry, err)
		}
		found := false
		for _, attempt := range pending {
			if attempt.CreditID == entry.CreditID {
				found = true
			}
		}
		if !found {
			if len(pending) != 0 {
				return a.blockAutomationEntry(ctx, state.Revision, entry, resetAppError("consume_indeterminate", errors.New("account has an unresolved reset attempt")))
			}
			if _, err := a.Attempts.Ensure(entry.AccountKey, entry.CreditID, entry.UpdatedAt); err != nil {
				return a.blockAutomationEntry(ctx, state.Revision, entry, err)
			}
		}
	}
	// Target-only preflight keeps explicit fixed authority independent of forecast failures.
	plan, err := a.prepareUse(ctx, string(entry.AccountKey), entry.CreditID, false)
	if err != nil {
		return a.blockAutomationEntry(ctx, state.Revision, entry, err)
	}
	if plan.account.AccountKey != entry.AccountKey || plan.AccountID != entry.AccountID {
		return a.blockAutomationEntry(ctx, state.Revision, entry, errors.New("account_identity_changed"))
	}
	{
		if len(plan.CurrentWindows) == 0 {
			return a.blockAutomationEntry(ctx, state.Revision, entry, errors.New("usage_windows_invalid"))
		}
		full := true
		for _, window := range plan.CurrentWindows {
			remaining := float64(window.RemainingPct)
			if window.RemainingPctExact != nil {
				remaining = *window.RemainingPctExact
			}
			if !time.Unix(window.ResetAtUnix, 0).After(a.now()) || !finiteResetRemaining(remaining) {
				return a.blockAutomationEntry(ctx, state.Revision, entry, errors.New("usage_windows_invalid"))
			}
			if remaining < 100 {
				full = false
			}
		}
		if full && !plan.selection.Resume {
			if state.Mode == codex.ResetAutomationAdaptive && !entry.Pinned {
				_, err := a.Schedule.Update(ctx, func(current *codex.ResetAutomationState) error {
					e := current.Entry(entry.ID)
					if current.Revision != state.Revision || e == nil {
						return errResetScheduleChanged
					}
					e.At = nil
					e.LastError = "not_yet_useful"
					current.PlanValid = false
					current.NextReplanAt = resetTimePointer(a.now().Add(5 * time.Minute))
					return nil
				})
				if errors.Is(err, errResetScheduleChanged) {
					return nil
				}
				return err
			}
			return a.finishAutomationEntry(ctx, state.Revision, entry.ID, codex.ResetAutomationSkipped, "nothing_to_reset")
		}
	}
	// The claim and durable attempt precede the POST. Cancellation before this
	// lock wins; later edits cannot revoke a request already sent upstream.
	claim := func() error {
		_, err := a.Schedule.Update(ctx, func(current *codex.ResetAutomationState) error {
			e := current.Entry(entry.ID)
			if !current.Enabled || current.Revision != state.Revision || e == nil || e.Terminal() {
				return errResetScheduleNoWork
			}
			if !plan.selection.Resume && plan.Credit.ExpiresAt != nil && !plan.Credit.ExpiresAt.After(a.now()) {
				return errResetScheduleNoWork
			}
			e.State = codex.ResetAutomationRunning
			e.LastError = ""
			e.UpdatedAt = a.now()
			return nil
		})
		return err
	}
	if a.ScheduleClaim != nil {
		err = a.ScheduleClaim(ctx, claim)
	} else {
		err = claim()
	}
	if errors.Is(err, errResetScheduleNoWork) {
		return nil
	}
	if err != nil {
		return err
	}
	result, consumeErr := a.executeUse(ctx, plan, true)
	// Completion merges into current state; it never restores old mode/enabled flags.
	_, saveErr := a.Schedule.Update(context.WithoutCancel(ctx), func(current *codex.ResetAutomationState) error {
		e := current.Entry(entry.ID)
		if e == nil {
			return errors.New("claimed reset entry missing")
		}
		e.UpdatedAt = a.now()
		e.NextAttemptAt = nil
		if consumeErr != nil {
			e.State = codex.ResetAutomationRetrying
			e.LastError = resetAutomationErrorCode(consumeErr)
			e.NextAttemptAt = resetTimePointer(a.now().Add(time.Minute))
			return nil
		}
		applyAutomationUseResult(current, e, result, a.now())
		return nil
	})
	if consumeErr == nil && saveErr == nil {
		if err := a.Attempts.Remove(entry.AccountKey, entry.CreditID); err != nil {
			_, warningErr := a.Schedule.Update(context.WithoutCancel(ctx), func(current *codex.ResetAutomationState) error {
				if e := current.Entry(entry.ID); e != nil {
					e.Warnings = append(e.Warnings, "attempt_cleanup_failed")
				}
				return nil
			})
			return errors.Join(err, warningErr)
		}
	}
	return errors.Join(consumeErr, saveErr)
}

func (a *CodexResetApp) reconcileManualAutomationUse(ctx context.Context, plan CodexResetUsePlan, result CodexResetUseResult) error {
	_, err := a.Schedule.Update(ctx, func(state *codex.ResetAutomationState) error {
		for i := range state.Entries {
			entry := &state.Entries[i]
			if entry.AccountKey == plan.account.AccountKey && entry.CreditID == plan.Credit.ID && !entry.Terminal() {
				applyAutomationUseResult(state, entry, result, a.now())
				return nil
			}
		}
		return errResetScheduleNoWork
	})
	if errors.Is(err, errResetScheduleNoWork) {
		return nil
	}
	return err
}

func applyAutomationUseResult(state *codex.ResetAutomationState, entry *codex.ResetAutomationEntry, result CodexResetUseResult, now time.Time) {
	entry.UpdatedAt = now
	entry.NextAttemptAt = nil
	entry.Outcome = result.Outcome
	entry.LastError = ""
	entry.Warnings = nil
	for _, warning := range result.Warnings {
		entry.Warnings = append(entry.Warnings, warning.Code)
	}
	switch result.Outcome {
	case codex.ConsumeReset, codex.ConsumeAlreadyRedeemed:
		entry.State = codex.ResetAutomationCompleted
	case codex.ConsumeNothingToReset:
		entry.State = codex.ResetAutomationSkipped
	default:
		entry.State = codex.ResetAutomationMissed
		entry.LastError = "no_credit"
	}
	state.PlanValid = false
	state.NextReplanAt = nil
}

func finiteResetRemaining(value float64) bool { return value >= 0 && value <= 100 }

func resetAutomationErrorCode(err error) string {
	var typed *CodexResetError
	if errors.As(err, &typed) {
		return typed.Code
	}
	return "preflight_failed"
}
func (a *CodexResetApp) blockAutomationEntry(ctx context.Context, revision uint64, entry codex.ResetAutomationEntry, cause error) error {
	_, err := a.Schedule.Update(ctx, func(state *codex.ResetAutomationState) error {
		if state.Revision != revision {
			return errResetScheduleChanged
		}
		e := state.Entry(entry.ID)
		if e == nil || e.Terminal() {
			return nil
		}
		if e.State != codex.ResetAutomationRunning && e.State != codex.ResetAutomationRetrying {
			e.State = codex.ResetAutomationBlocked
		}
		e.LastError = resetAutomationErrorCode(cause)
		e.NextAttemptAt = resetTimePointer(a.now().Add(time.Minute))
		e.UpdatedAt = a.now()
		return nil
	})
	if errors.Is(err, errResetScheduleChanged) {
		return nil
	}
	return errors.Join(cause, err)
}
func (a *CodexResetApp) finishAutomationEntry(ctx context.Context, revision uint64, id string, status codex.ResetAutomationEntryState, reason string) error {
	_, err := a.Schedule.Update(ctx, func(state *codex.ResetAutomationState) error {
		if state.Revision != revision {
			return errResetScheduleChanged
		}
		e := state.Entry(id)
		if e == nil || e.Terminal() {
			return nil
		}
		e.State = status
		e.LastError = reason
		e.UpdatedAt = a.now()
		state.PlanValid = false
		state.NextReplanAt = nil
		return nil
	})
	if errors.Is(err, errResetScheduleChanged) {
		return nil
	}
	return err
}
