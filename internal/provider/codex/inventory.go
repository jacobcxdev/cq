package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type AccountKey string
type CandidateID string
type LineageID string
type Revision string

type SelectionExclusion struct {
	AccountKey  AccountKey
	CandidateID CandidateID
}

type CredentialSource uint8

const (
	SourceSystem CredentialSource = iota + 1
	SourceManaged
)

type AccountIdentity struct {
	AccountID string
	UserID    string
	Email     string
	PlanType  string
	RecordKey string
}

type CredentialCandidate struct {
	Ref             CandidateRef
	Revision        Revision
	Source          CredentialSource
	Credential      CodexAccount
	AccessExpiresAt time.Time
	CQAuthored      bool
}

type LogicalAccount struct {
	Key        AccountKey
	Identity   AccountIdentity
	Candidates []CredentialCandidate
	Active     bool
	Routable   bool
	Unstable   bool
}

type InventoryIntentKind string

const (
	IntentAssociate InventoryIntentKind = "associate"
	IntentAdopt     InventoryIntentKind = "adopt"
)

type InventoryIntent struct {
	Kind       InventoryIntentKind
	AccountKey AccountKey
	Candidates []CandidateID
}

type Inventory struct {
	Accounts []LogicalAccount
	Intents  []InventoryIntent
}

type rawCandidate struct {
	account    CodexAccount
	data       []byte
	source     CredentialSource
	sourceName string
	cqAuthored bool
}

// DiscoverInventory builds one generation-local read model without writing.
func DiscoverInventory(fs fsutil.FileSystem) Inventory {
	home, err := fs.UserHomeDir()
	if err != nil {
		return Inventory{}
	}
	var raw []rawCandidate
	systemPath := filepath.Join(home, ".codex", "auth.json")
	if candidate, ok := readRawCandidate(fs, systemPath, SourceSystem, "system"); ok {
		candidate.account.IsActive = true
		raw = append(raw, candidate)
	}
	accountsDir := filepath.Join(home, ".codex", "accounts")
	entries, _ := fs.ReadDir(accountsDir)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".auth.json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(accountsDir, name)
		if candidate, ok := readRawCandidate(fs, path, SourceManaged, name); ok {
			raw = append(raw, candidate)
		}
	}

	var inventory Inventory
	for _, candidate := range raw {
		matches := compatibleLogicalAccounts(inventory.Accounts, candidate.account)
		index := -1
		if len(matches) == 1 {
			index = matches[0]
		}
		if index < 0 {
			identity := identityFromAccount(candidate.account)
			key := generationAccountKey(identity, candidate.source, candidate.sourceName)
			inventory.Accounts = append(inventory.Accounts, LogicalAccount{
				Key: key, Identity: identity, Unstable: true,
			})
			index = len(inventory.Accounts) - 1
		}
		logical := &inventory.Accounts[index]
		enrichIdentity(&logical.Identity, candidate.account)
		candidateID := generationCandidateID(candidate.source, candidate.account, candidate.sourceName)
		ref := CandidateRef{AccountKey: logical.Key, CandidateID: candidateID, path: candidate.account.FilePath}
		credential := candidate.account
		credential.AccountKey = logical.Key
		credential.CandidateID = candidateID
		credential.Revision = credentialRevision(candidate.data)
		logical.Candidates = append(logical.Candidates, CredentialCandidate{
			Ref: ref, Revision: credential.Revision, Source: candidate.source,
			Credential: credential, AccessExpiresAt: unixMilliTime(credential.ExpiresAt),
			CQAuthored: candidate.cqAuthored,
		})
		logical.Active = logical.Active || candidate.source == SourceSystem
		logical.Routable = logical.Routable || (credential.AccountID != "" && credential.AccessToken != "")
	}

	for i := range inventory.Accounts {
		logical := &inventory.Accounts[i]
		sortCandidates(logical.Candidates, "", time.Now())
		ids := make([]CandidateID, len(logical.Candidates))
		hasSystem, hasManaged := false, false
		for j, candidate := range logical.Candidates {
			ids[j] = candidate.Ref.CandidateID
			hasSystem = hasSystem || candidate.Source == SourceSystem
			hasManaged = hasManaged || candidate.Source == SourceManaged
		}
		if len(ids) > 1 {
			inventory.Intents = append(inventory.Intents, InventoryIntent{Kind: IntentAssociate, AccountKey: logical.Key, Candidates: ids})
		}
		if hasSystem && !hasManaged && logical.Routable {
			inventory.Intents = append(inventory.Intents, InventoryIntent{Kind: IntentAdopt, AccountKey: logical.Key, Candidates: ids})
		}
	}
	sort.SliceStable(inventory.Accounts, func(i, j int) bool {
		if inventory.Accounts[i].Active != inventory.Accounts[j].Active {
			return inventory.Accounts[i].Active
		}
		return inventory.Accounts[i].Key < inventory.Accounts[j].Key
	})
	return inventory
}

func readRawCandidate(fs fsutil.FileSystem, path string, source CredentialSource, sourceName string) (rawCandidate, bool) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return rawCandidate{}, false
	}
	account, ok := parseAccountData(data, path)
	if !ok {
		return rawCandidate{}, false
	}
	var doc struct {
		CQ struct {
			Provenance string `json:"provenance"`
		} `json:"_cq"`
	}
	_ = json.Unmarshal(data, &doc)
	return rawCandidate{
		account: account, data: data, source: source, sourceName: sourceName,
		cqAuthored: doc.CQ.Provenance == "cq_oauth",
	}, true
}

func identityFromAccount(account CodexAccount) AccountIdentity {
	return AccountIdentity{
		AccountID: account.AccountID, UserID: account.UserID, Email: account.Email,
		PlanType: account.PlanType, RecordKey: account.RecordKey,
	}
}

func compatibleLogicalAccounts(accounts []LogicalAccount, candidate CodexAccount) []int {
	var matches []int
	for i, logical := range accounts {
		identity := logical.Identity
		if candidate.AccountID == "" || identity.AccountID == "" || candidate.AccountID != identity.AccountID {
			continue
		}
		if candidate.UserID != "" && identity.UserID != "" && candidate.UserID != identity.UserID {
			continue
		}
		matches = append(matches, i)
	}
	return matches
}

func enrichIdentity(identity *AccountIdentity, candidate CodexAccount) {
	if identity.AccountID == "" {
		identity.AccountID = candidate.AccountID
	}
	if identity.UserID == "" {
		identity.UserID = candidate.UserID
	}
	if identity.Email == "" {
		identity.Email = candidate.Email
	}
	if identity.PlanType == "" {
		identity.PlanType = candidate.PlanType
	}
	if identity.RecordKey == "" {
		identity.RecordKey = candidate.RecordKey
	}
}

func generationAccountKey(identity AccountIdentity, source CredentialSource, sourceName string) AccountKey {
	seed := ""
	if identity.AccountID != "" {
		seed = "account:" + identity.AccountID
		if identity.UserID != "" {
			seed += ":user:" + identity.UserID
		}
	} else {
		seed = "isolated:" + source.String() + ":" + sourceName
	}
	return AccountKey("gen:" + shortHash(seed))
}

func generationCandidateID(source CredentialSource, account CodexAccount, sourceName string) CandidateID {
	identity := account.RecordKey
	if identity == "" {
		identity = account.AccountID
	}
	if identity == "" {
		identity = sourceName
	}
	return CandidateID(source.String() + ":" + shortHash(identity+":"+sourceName))
}

func (s CredentialSource) String() string {
	if s == SourceSystem {
		return "system"
	}
	return "managed"
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func unixMilliTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

// ResolveCandidate returns deterministic candidates in retry order.
func ResolveCandidate(logical LogicalAccount, accepted Revision, now time.Time) []CredentialCandidate {
	candidates := append([]CredentialCandidate(nil), logical.Candidates...)
	sortCandidates(candidates, accepted, now)
	return candidates
}

func sortCandidates(candidates []CredentialCandidate, accepted Revision, now time.Time) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Revision == accepted) != (b.Revision == accepted) {
			return a.Revision == accepted
		}
		classA, classB := expiryClass(a.AccessExpiresAt, now), expiryClass(b.AccessExpiresAt, now)
		if classA != classB {
			return classA < classB
		}
		if a.CQAuthored != b.CQAuthored {
			return a.CQAuthored
		}
		if !a.AccessExpiresAt.Equal(b.AccessExpiresAt) {
			return a.AccessExpiresAt.After(b.AccessExpiresAt)
		}
		if a.Source != b.Source {
			return a.Source == SourceSystem
		}
		return a.Ref.CandidateID < b.Ref.CandidateID
	})
}

func expiryClass(expires time.Time, now time.Time) int {
	if expires.IsZero() {
		return 1
	}
	if expires.After(now) {
		return 0
	}
	return 2
}
