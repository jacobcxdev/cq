package codex

import "strings"

// AccountReferenceErrorCode classifies a reference that cannot be resolved to
// one stable logical account.
type AccountReferenceErrorCode string

const (
	AccountReferenceEmpty     AccountReferenceErrorCode = "empty"
	AccountReferenceMissing   AccountReferenceErrorCode = "missing"
	AccountReferenceAmbiguous AccountReferenceErrorCode = "ambiguous"
	AccountReferenceUnstable  AccountReferenceErrorCode = "unstable"
)

// AccountReferenceError deliberately excludes the supplied reference and all
// account metadata so callers can report it without exposing identifiers.
type AccountReferenceError struct {
	Code AccountReferenceErrorCode
}

func (e *AccountReferenceError) Error() string {
	switch e.Code {
	case AccountReferenceEmpty:
		return "account reference is empty"
	case AccountReferenceMissing:
		return "account reference does not resolve"
	case AccountReferenceAmbiguous:
		return "account reference is ambiguous"
	case AccountReferenceUnstable:
		return "account reference resolves to an unstable account"
	default:
		return "account reference is invalid"
	}
}

type accountAliasRow struct {
	Alias      string
	AccountKey AccountKey
}

// AccountAliasIndex is a read-only, non-credential view of CQ aliases. Its
// contents are intentionally opaque outside this package; consumers can only
// use them to resolve a reference against an inventory generation.
type AccountAliasIndex struct {
	rows []accountAliasRow
}

// AccountAliasIndex reads well-formed alias-to-key rows from registry.json.
// It does not project active state or modify the registry.
func (r Registry) AccountAliasIndex() (AccountAliasIndex, error) {
	doc, err := r.read()
	if err != nil {
		return AccountAliasIndex{}, err
	}

	var index AccountAliasIndex
	accounts, _ := doc["accounts"].([]any)
	for _, raw := range accounts {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		alias, aliasOK := record["alias"].(string)
		accountKey, keyOK := record["account_key"].(string)
		alias = strings.TrimSpace(alias)
		if !aliasOK || !keyOK || alias == "" || accountKey == "" || strings.TrimSpace(accountKey) != accountKey {
			continue
		}
		index.rows = append(index.rows, accountAliasRow{
			Alias:      alias,
			AccountKey: AccountKey(accountKey),
		})
	}
	return index, nil
}

// ResolveAccountReference resolves one stable account without exposing account
// metadata. An exact opaque key takes precedence over email and CQ alias
// matches. Routability is deliberately not a resolution requirement.
func ResolveAccountReference(
	inventory Inventory,
	aliases AccountAliasIndex,
	reference string,
) (AccountKey, error) {
	trimmedReference := strings.TrimSpace(reference)
	if trimmedReference == "" {
		return "", &AccountReferenceError{Code: AccountReferenceEmpty}
	}

	exactMatches := make([]bool, len(inventory.Accounts))
	for i := range inventory.Accounts {
		if inventory.Accounts[i].Key == AccountKey(reference) {
			exactMatches[i] = true
		}
	}
	if matchedAccountCount(exactMatches) != 0 {
		return resolveMatchedAccount(inventory.Accounts, exactMatches)
	}

	metadataMatches := make([]bool, len(inventory.Accounts))
	for i := range inventory.Accounts {
		account := &inventory.Accounts[i]
		if account.Key == "" {
			continue
		}
		email := strings.TrimSpace(account.Identity.Email)
		if email != "" && strings.EqualFold(email, trimmedReference) {
			metadataMatches[i] = true
		}
	}
	for _, row := range aliases.rows {
		if !strings.EqualFold(row.Alias, trimmedReference) {
			continue
		}
		for i := range inventory.Accounts {
			if inventory.Accounts[i].Key == row.AccountKey {
				metadataMatches[i] = true
			}
		}
	}
	return resolveMatchedAccount(inventory.Accounts, metadataMatches)
}

// AccountKeyState returns only the state needed to report whether a persisted
// opaque key still resolves and can currently route. Duplicate keys are not
// resolved because they do not identify one logical account.
func AccountKeyState(inventory Inventory, key AccountKey) (resolved, routable, unstable bool) {
	if key == "" {
		return false, false, false
	}

	match := -1
	for i := range inventory.Accounts {
		if inventory.Accounts[i].Key != key {
			continue
		}
		if match != -1 {
			return false, false, false
		}
		match = i
	}
	if match == -1 {
		return false, false, false
	}
	account := inventory.Accounts[match]
	return true, account.Routable, account.Unstable
}

func matchedAccountCount(matches []bool) int {
	count := 0
	for _, match := range matches {
		if match {
			count++
		}
	}
	return count
}

func resolveMatchedAccount(accounts []LogicalAccount, matches []bool) (AccountKey, error) {
	match := -1
	for i, matched := range matches {
		if !matched {
			continue
		}
		if match != -1 {
			return "", &AccountReferenceError{Code: AccountReferenceAmbiguous}
		}
		match = i
	}
	if match == -1 {
		return "", &AccountReferenceError{Code: AccountReferenceMissing}
	}
	if accounts[match].Unstable {
		return "", &AccountReferenceError{Code: AccountReferenceUnstable}
	}
	return accounts[match].Key, nil
}
