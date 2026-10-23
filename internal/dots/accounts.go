package dots

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// Account credentials stay in memory and are never part of persisted JSON.
type Account struct {
	Alias     string          `json:"alias"`
	AccountID string          `json:"account_id"`
	Token     string          `json:"-"`
	ViewerIDs map[string]bool `json:"-"`
	Email     string          `json:"email,omitempty"`
}

type AccountOutcome struct {
	OwnerAlias     string `json:"owner_alias,omitempty"`
	OwnerAccountID string `json:"owner_account_id,omitempty"`
	System         bool   `json:"system"`
	Status         string `json:"status"`
	Source         string `json:"source,omitempty"`
	FailureType    string `json:"failure_type,omitempty"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Handle         string `json:"handle,omitempty"`
}

type Inventory struct {
	System   Account
	Accounts []Account
	Failures []AccountOutcome
}

type IdentityCollision struct{ Message string }

func (e *IdentityCollision) Error() string { return e.Message }

type fileRevision struct {
	Identity fsutil.SecureFileIdentity
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	Owner    uint64
	CTime    string
	Group    string
}

func revision(info os.FileInfo) fileRevision {
	fs := fsutil.OSFileSystem{}
	identity, _ := fs.FileIdentity(info)
	owner, _ := fs.FileOwnerUID(info)
	value := fileRevision{Identity: identity, Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime(), Owner: owner}
	// Darwin and Linux spell ctime differently. Atime must not invalidate a read.
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if stat.IsValid() && stat.Kind() == reflect.Struct {
		for _, name := range []string{"Ctim", "Ctimespec"} {
			if field := stat.FieldByName(name); field.IsValid() && field.CanInterface() {
				value.CTime = fmt.Sprint(field.Interface())
			}
		}
		if field := stat.FieldByName("Gid"); field.IsValid() && field.CanInterface() {
			value.Group = fmt.Sprint(field.Interface())
		}
	}
	return value
}

// ReadOwned reads a declared credential source without following symlinks or refreshing it.
func ReadOwned(path, root string) ([]byte, error) { return readPrivate(path, root, false) }

func readPrivate(path, root string, exactMode bool) ([]byte, error) {
	if root != "" {
		if !cleanAbsolute(path) || !cleanAbsolute(root) || !within(root, path) {
			return nil, fmt.Errorf("invalid declared credential path")
		}
		current := root
		relative, _ := filepath.Rel(root, path)
		parts := append([]string{""}, strings.Split(relative, string(filepath.Separator))...)
		for _, part := range parts {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("symlink in declared credential path")
			}
		}
	}
	file, err := (fsutil.OSFileSystem{}).OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || !owned(before) || before.Size() > 1<<20 || before.Mode().Perm()&0o077 != 0 || exactMode && before.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
		return nil, fmt.Errorf("private state must be an owned regular file with private permissions and bounded size")
	}
	data, err := httputil.ReadBody(file)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if revision(before) != revision(after) {
		return nil, fmt.Errorf("credential revision changed during read")
	}
	return data, nil
}

// Claims decodes bounded JWT claims. The upstream service still authenticates the token.
func Claims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(token) == 0 || len(token) > 16384 || len(parts) != 3 {
		return nil, fmt.Errorf("invalid credential token")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("invalid credential claims")
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil || claims == nil {
		return nil, fmt.Errorf("invalid credential claims")
	}
	return claims, nil
}

func ParseCredential(data []byte, now time.Time) (Account, error) {
	var document map[string]any
	if json.Unmarshal(data, &document) != nil {
		return Account{}, fmt.Errorf("invalid credential structure")
	}
	tokens, ok := document["tokens"].(map[string]any)
	if !ok {
		return Account{}, fmt.Errorf("invalid credential tokens")
	}
	owner, ok := tokens["account_id"].(string)
	if !ok || owner == "" || len(owner) > 256 {
		return Account{}, fmt.Errorf("invalid credential account identity")
	}
	idToken, _ := tokens["id_token"].(string)
	accessToken, _ := tokens["access_token"].(string)
	identity, err := Claims(idToken)
	if err != nil {
		return Account{}, err
	}
	access, err := Claims(accessToken)
	if err != nil {
		return Account{}, err
	}
	identityAuth, _ := identity["https://api.openai.com/auth"].(map[string]any)
	auth, _ := access["https://api.openai.com/auth"].(map[string]any)
	if identityAuth == nil || auth == nil || identityAuth["chatgpt_account_id"] != owner || auth["chatgpt_account_id"] != owner {
		return Account{}, fmt.Errorf("credential principal mismatch")
	}
	expires, ok := access["exp"].(float64)
	if !ok || expires <= float64(now.UnixNano())/1e9+60 {
		return Account{}, fmt.Errorf("credential expired; read-only discovery will not refresh")
	}
	viewers := map[string]bool{}
	for _, key := range []string{"chatgpt_user_id", "chatgpt_account_user_id"} {
		if value, exists := auth[key]; exists && value != nil {
			text, ok := value.(string)
			if !ok || text == "" {
				return Account{}, fmt.Errorf("owner viewer identity unavailable")
			}
			viewers[text] = true
		}
	}
	if len(viewers) == 0 {
		return Account{}, fmt.Errorf("owner viewer identity unavailable")
	}
	email, _ := identity["email"].(string)
	return Account{Alias: AccountAlias(owner), AccountID: owner, Token: accessToken, ViewerIDs: viewers, Email: email}, nil
}

func privateSource(path string, declared bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned(info) || info.Mode().Perm()&0o022 != 0 || !declared && info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o700 {
		return fmt.Errorf("invalid credential source directory")
	}
	return nil
}

func managedReady(document map[string]any) (bool, error) {
	if document == nil {
		return false, fmt.Errorf("invalid managed credential document")
	}
	raw, present := document["_cq"]
	if !present {
		return true, nil
	}
	metadata, ok := raw.(map[string]any)
	if !ok || metadata["version"] != float64(1) {
		return false, fmt.Errorf("invalid managed credential metadata")
	}
	generation, ok := metadata["generation"].(float64)
	if !ok || generation < 1 || generation != float64(int64(generation)) {
		return false, fmt.Errorf("invalid managed credential metadata")
	}
	for _, field := range []string{"account_key", "candidate_id", "lineage_id", "revision"} {
		if text, ok := metadata[field].(string); !ok || text == "" {
			return false, fmt.Errorf("invalid managed credential metadata")
		}
	}
	if value := metadata["provenance"]; value != "cq_oauth" && value != "system_borrowed" && value != "legacy_unknown" {
		return false, fmt.Errorf("unknown managed credential provenance")
	}
	if value := metadata["refresh_ownership"]; value != "cq_owned_never_exported" && value != "exported_to_system" && value != "unknown" {
		return false, fmt.Errorf("unknown managed credential ownership")
	}
	tokens, ok := document["tokens"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("managed credential tokens missing")
	}
	// Restore borrowed maps before returning; callers may still parse the source document.
	want := metadata["revision"].(string)
	metadata["revision"] = ""
	operation, hadOperation := metadata["operation_id"]
	if operation == nil || operation == "" || operation == false || operation == float64(0) {
		delete(metadata, "operation_id")
	}
	_, hadRefresh := tokens["refresh_token"]
	if !hadRefresh {
		tokens["refresh_token"] = ""
	}
	encoded, err := json.Marshal(document)
	metadata["revision"] = want
	if hadOperation {
		metadata["operation_id"] = operation
	}
	if !hadRefresh {
		delete(tokens, "refresh_token")
	}
	if err != nil {
		return false, fmt.Errorf("managed credential revision unavailable")
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != want {
		return false, fmt.Errorf("managed credential revision mismatch")
	}
	if metadata["operation_state"] == "removing" {
		return false, nil
	}
	if metadata["operation_state"] != "ready" {
		return false, fmt.Errorf("managed credential operation unavailable")
	}
	return true, nil
}

func sourceFailure(source string, err error, document map[string]any) AccountOutcome {
	outcome := AccountOutcome{Status: "unavailable", Source: source, FailureType: "ValueError"}
	if errors.Is(err, os.ErrNotExist) {
		outcome.FailureType = "FileNotFoundError"
	} else if _, ok := err.(*os.PathError); ok {
		outcome.FailureType = "OSError"
	}
	if tokens, ok := document["tokens"].(map[string]any); ok {
		if owner, ok := tokens["account_id"].(string); ok && owner != "" {
			outcome.OwnerAccountID, outcome.OwnerAlias = owner, AccountAlias(owner)
		}
	}
	return outcome
}

// Inventory rereads sources only; no credential material is copied or refreshed.
func (c *CatalogueClient) Inventory() (Inventory, error) {
	s := c.Settings
	if s == nil {
		return Inventory{}, fmt.Errorf("Dots settings unavailable")
	}
	data, err := ReadOwned(s.SystemAuth, "")
	if err != nil {
		return Inventory{}, err
	}
	system, err := ParseCredential(data, c.now())
	if err != nil {
		return Inventory{}, err
	}
	if system.Alias != s.SystemAlias || s.SystemAccountID != "" && system.AccountID != s.SystemAccountID {
		return Inventory{}, fmt.Errorf("system credential principal changed")
	}
	result := Inventory{System: system, Accounts: []Account{system}, Failures: []AccountOutcome{}}
	accounts, aliases := map[string]bool{system.AccountID: true}, map[string]string{system.Alias: system.AccountID}
	add := func(account Account) error {
		if owner, exists := aliases[account.Alias]; exists && owner != account.AccountID {
			return &IdentityCollision{"account alias collision"}
		}
		aliases[account.Alias] = account.AccountID
		if !accounts[account.AccountID] {
			accounts[account.AccountID] = true
			result.Accounts = append(result.Accounts, account)
		}
		return nil
	}
	if _, err := os.Lstat(s.CQAccounts); err == nil {
		if err := privateSource(s.CQAccounts, false); err != nil {
			result.Failures = append(result.Failures, sourceFailure("cq", err, nil))
		} else {
			paths, err := filepath.Glob(filepath.Join(s.CQAccounts, "*.auth.json"))
			if err != nil {
				return Inventory{}, err
			}
			for _, path := range paths {
				var document map[string]any
				data, err := ReadOwned(path, s.CQAccounts)
				if err == nil {
					err = json.Unmarshal(data, &document)
				}
				ready := false
				if err == nil {
					ready, err = managedReady(document)
				}
				if err == nil && !ready {
					continue
				}
				if err == nil {
					var account Account
					account, err = ParseCredential(data, c.now())
					if err == nil {
						err = add(account)
					}
				}
				var collision *IdentityCollision
				if errors.As(err, &collision) {
					return Inventory{}, err
				}
				if err != nil {
					result.Failures = append(result.Failures, sourceFailure("cq", err, document))
				}
			}
		}
	} else if !os.IsNotExist(err) {
		result.Failures = append(result.Failures, sourceFailure("cq", err, nil))
	}
	manifestPath := filepath.Join(s.AccountSource, "managed-codex-accounts.json")
	if _, err := os.Lstat(manifestPath); s.AccountSource != "" && (err == nil || !os.IsNotExist(err)) {
		var manifest struct {
			Version  int `json:"version"`
			Accounts []struct {
				ManagedHomePath    string `json:"managedHomePath"`
				AuthFingerprint    string `json:"authFingerprint"`
				ProviderAccountID  string `json:"providerAccountID"`
				WorkspaceAccountID string `json:"workspaceAccountID"`
			} `json:"accounts"`
		}
		err := privateSource(s.AccountSource, true)
		if err == nil {
			var data []byte
			data, err = ReadOwned(manifestPath, s.AccountSource)
			if err == nil {
				err = json.Unmarshal(data, &manifest)
			}
			if err == nil && (manifest.Version != 3 || manifest.Accounts == nil) {
				err = fmt.Errorf("unsupported declared manifest")
			}
		}
		if err != nil {
			result.Failures = append(result.Failures, sourceFailure("codexbar", err, nil))
		} else {
			for _, row := range manifest.Accounts {
				var document map[string]any
				data, err := ReadOwned(filepath.Join(row.ManagedHomePath, "auth.json"), s.AccountSource)
				if err == nil {
					digest := sha256.Sum256(data)
					if hex.EncodeToString(digest[:]) != row.AuthFingerprint {
						err = fmt.Errorf("declared credential fingerprint changed")
					}
				}
				var account Account
				if err == nil {
					err = json.Unmarshal(data, &document)
				}
				if err == nil {
					account, err = ParseCredential(data, c.now())
				}
				if err == nil && (row.ProviderAccountID != account.AccountID || row.WorkspaceAccountID != account.AccountID) {
					err = fmt.Errorf("declared credential identity mismatch")
				}
				if err == nil {
					err = add(account)
				}
				var collision *IdentityCollision
				if errors.As(err, &collision) {
					return Inventory{}, err
				}
				if err != nil {
					result.Failures = append(result.Failures, sourceFailure("codexbar", err, document))
				}
			}
		}
	}
	return result, nil
}

// pythonJSON matches Python's ensure_ascii=True compact, sorted JSON used in ledger identities.
func pythonJSON(value any, ascii bool) ([]byte, error) {
	var buffer bytes.Buffer
	var write func(any) error
	writeString := func(value string) {
		buffer.WriteByte('"')
		for _, char := range value {
			switch char {
			case '\\', '"':
				buffer.WriteByte('\\')
				buffer.WriteRune(char)
			case '\b':
				buffer.WriteString(`\b`)
			case '\f':
				buffer.WriteString(`\f`)
			case '\n':
				buffer.WriteString(`\n`)
			case '\r':
				buffer.WriteString(`\r`)
			case '\t':
				buffer.WriteString(`\t`)
			default:
				if char < 32 || ascii && char >= 127 {
					if char > 0xffff {
						for _, half := range utf16.Encode([]rune{char}) {
							fmt.Fprintf(&buffer, `\u%04x`, half)
						}
					} else {
						fmt.Fprintf(&buffer, `\u%04x`, char)
					}
				} else {
					buffer.WriteRune(char)
				}
			}
		}
		buffer.WriteByte('"')
	}
	write = func(value any) error {
		switch item := value.(type) {
		case string:
			writeString(item)
		case map[string]any:
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			buffer.WriteByte('{')
			for index, key := range keys {
				if index > 0 {
					buffer.WriteByte(',')
				}
				writeString(key)
				buffer.WriteByte(':')
				if err := write(item[key]); err != nil {
					return err
				}
			}
			buffer.WriteByte('}')
		case nil:
			buffer.WriteString("null")
		case []any:
			buffer.WriteByte('[')
			for index, entry := range item {
				if index > 0 {
					buffer.WriteByte(',')
				}
				if err := write(entry); err != nil {
					return err
				}
			}
			buffer.WriteByte(']')
		case bool:
			buffer.WriteString(strconv.FormatBool(item))
		case float64:
			buffer.WriteString(strconv.FormatFloat(item, 'g', -1, 64))
		default:
			data, err := json.Marshal(item)
			if err != nil {
				return err
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			var generic any
			if err := decoder.Decode(&generic); err != nil {
				return err
			}
			return write(generic)
		}
		return nil
	}
	if err := write(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
