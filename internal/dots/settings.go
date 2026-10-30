// Package dots manages account-owned Dots in the signed native application.
package dots

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Dot is the persisted owner/profile binding. Display names are never identities.
type Dot struct {
	OwnerAlias     string `json:"owner_alias"`
	OwnerAccountID string `json:"owner_account_id,omitempty"`
	ProfileID      string `json:"profile_id"`
	RootThreadID   string `json:"root_thread_id"`
	DisplayName    string `json:"display_name,omitempty"`
	legacy         bool
}

// Settings retains the existing installation's JSON schema.
type Settings struct {
	RuntimeRoot       string          `json:"runtime_root"`
	AppPath           string          `json:"app_path"`
	AppVersion        string          `json:"app_version"`
	AppBuild          string          `json:"app_build"`
	ASARSHA256        string          `json:"asar_sha256"`
	ModuleAsset       string          `json:"module_asset"`
	NativeBindings    *NativeBindings `json:"native_bindings,omitempty"`
	AccountSource     string          `json:"account_source,omitempty"`
	SystemAuth        string          `json:"system_auth"`
	CQAccounts        string          `json:"cq_accounts,omitempty"`
	SystemAlias       string          `json:"system_alias"`
	SystemAccountID   string          `json:"system_account_id,omitempty"`
	SecondaryAlias    string          `json:"secondary_alias,omitempty"`
	SecondarySelector string          `json:"secondary_selector,omitempty"`
	Dots              map[string]Dot  `json:"dots"`
	DebugPort         int             `json:"debug_port"`
	IntegrationMode   string          `json:"integration_mode"`
}

// MarshalJSON retains legacy bindings until their full owner identity is known.
func (s Settings) MarshalJSON() ([]byte, error) {
	type plain Settings
	value := plain(s)
	value.Dots = make(map[string]Dot, len(s.Dots))
	for handle, dot := range s.Dots {
		if dot.OwnerAccountID == "" {
			dot.DisplayName = ""
		}
		value.Dots[handle] = dot
	}
	return json.Marshal(value)
}

var aliasPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var modulePattern = regexp.MustCompile(`^app-(?:shared|initial)-[A-Za-z0-9_-]+\.js$`)

func validText(value string, limit int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, char := range value {
		if char < 32 {
			return false
		}
	}
	return true
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func owned(info os.FileInfo) bool {
	fs := fsutil.OSFileSystem{}
	uid, ok := fs.FileOwnerUID(info)
	return ok && uid == fs.EffectiveUID()
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o700 {
		return fmt.Errorf("Dots configuration directory must be owned and mode 0700")
	}
	return nil
}

// PrivateDirectory creates private mutable state and rejects unsafe existing directories.
func PrivateDirectory(path string) error {
	if !cleanAbsolute(path) {
		return fmt.Errorf("private directory must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return privateDirectory(path)
}

// SaveJSON atomically replaces an owned private JSON state file.
func SaveJSON(path string, value any) error {
	if !cleanAbsolute(path) {
		return fmt.Errorf("private state path must be absolute")
	}
	if err := PrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
			return fmt.Errorf("private state must be an owned regular file with mode 0600")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("private state could not be encoded")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dots-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

// CanonicalPath resolves existing parents, including macOS's /tmp alias.
func CanonicalPath(path string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			relative, err := filepath.Rel(current, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, relative), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return "", err
		}
	}
}

func within(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// LoadSettings reads private configuration without discovering or refreshing credentials.
func LoadSettings(path string) (*Settings, error) {
	if path == "" {
		path = os.Getenv("CQ_DOTS_CONFIG")
	}
	if !cleanAbsolute(path) {
		return nil, fmt.Errorf("CQ_DOTS_CONFIG must name an absolute private configuration file")
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	data, err := readPrivate(path, "", true)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var settings Settings
	if err := decoder.Decode(&settings); err != nil {
		return nil, fmt.Errorf("Dots configuration must be valid JSON with supported fields")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("Dots configuration must be a JSON object")
	}
	for _, key := range []string{"system_account_id", "secondary_alias", "secondary_selector", "integration_mode", "cq_accounts"} {
		if raw, exists := fields[key]; exists && (bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte(`""`))) {
			return nil, fmt.Errorf("invalid Dots setting: %s", key)
		}
	}
	if raw, exists := fields["account_source"]; exists && bytes.Equal(raw, []byte(`""`)) {
		return nil, fmt.Errorf("invalid Dots setting: account_source")
	}
	if raw, exists := fields["dots"]; exists && bytes.Equal(raw, []byte("null")) {
		return nil, fmt.Errorf("invalid Dots profile configuration")
	}
	var dotFields map[string]map[string]json.RawMessage
	if raw, exists := fields["dots"]; exists {
		if json.Unmarshal(raw, &dotFields) != nil {
			return nil, fmt.Errorf("invalid Dots profile configuration")
		}
		for _, dot := range dotFields {
			for _, key := range []string{"owner_account_id", "display_name"} {
				if raw, exists := dot[key]; exists {
					var value string
					if json.Unmarshal(raw, &value) != nil || value == "" {
						return nil, fmt.Errorf("invalid Dots stable identity")
					}
				}
			}
		}
	}
	if settings.Dots == nil {
		settings.Dots = map[string]Dot{}
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return &settings, nil
}

// Validate checks ownership and protected paths; it never creates state.
func (s *Settings) Validate() error {
	for name, path := range map[string]string{"runtime_root": s.RuntimeRoot, "app_path": s.AppPath, "system_auth": s.SystemAuth} {
		if !validText(path, 4096) || !cleanAbsolute(path) {
			return fmt.Errorf("Dots path must be absolute: %s", name)
		}
	}
	if s.CQAccounts == "" {
		s.CQAccounts = filepath.Join(filepath.Dir(s.SystemAuth), "accounts")
	}
	for name, path := range map[string]string{"cq_accounts": s.CQAccounts, "account_source": s.AccountSource} {
		if name == "account_source" && path == "" {
			continue
		}
		if !validText(path, 4096) || !cleanAbsolute(path) {
			return fmt.Errorf("Dots path must be absolute: %s", name)
		}
	}
	root, err := CanonicalPath(s.RuntimeRoot)
	if err != nil {
		return err
	}
	// Developer builds must not put writable runtime state inside their source tree.
	_, source, _, ok := runtime.Caller(0)
	if ok {
		for current := filepath.Dir(source); filepath.Dir(current) != current; current = filepath.Dir(current) {
			if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
				if within(current, root) {
					return fmt.Errorf("Dots runtime must be outside the source tree")
				}
				break
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	home, err = CanonicalPath(home)
	if err != nil {
		return err
	}
	if within(root, home) {
		return fmt.Errorf("Dots runtime must not contain the home directory")
	}
	protected := []string{s.AppPath, s.CQAccounts, filepath.Dir(s.SystemAuth), filepath.Join(home, ".codex")}
	if s.AccountSource != "" {
		protected = append(protected, s.AccountSource)
	}
	for _, name := range []string{"Codex", "ChatGPT", "com.openai.codex", "com.openai.chat", "OpenAI"} {
		protected = append(protected, filepath.Join(home, "Library", "Application Support", name))
	}
	for _, path := range protected {
		canonical, err := CanonicalPath(path)
		if err != nil {
			return err
		}
		if within(root, canonical) || within(canonical, root) {
			return fmt.Errorf("Dots runtime overlaps a protected application or credential path")
		}
	}
	if !aliasPattern.MatchString(s.SystemAlias) || (s.SecondaryAlias != "" && (!aliasPattern.MatchString(s.SecondaryAlias) || s.SecondaryAlias == s.SystemAlias)) {
		return fmt.Errorf("invalid Dots account aliases")
	}
	if s.SystemAccountID != "" && (!validText(s.SystemAccountID, 256) || AccountAlias(s.SystemAccountID) != s.SystemAlias) {
		return fmt.Errorf("system account identity mismatch")
	}
	if s.SecondarySelector != "" && !validText(s.SecondarySelector, 256) {
		return fmt.Errorf("invalid Dots setting: secondary_selector")
	}
	if !digestPattern.MatchString(s.ASARSHA256) || !modulePattern.MatchString(s.ModuleAsset) || !validText(s.AppVersion, 256) || !validText(s.AppBuild, 256) {
		return fmt.Errorf("invalid Dots application pin")
	}
	if s.NativeBindings != nil {
		if err := s.NativeBindings.validate(); err != nil {
			return err
		}
	}
	if s.DebugPort < 0 || s.DebugPort > 65535 {
		return fmt.Errorf("invalid Dots setting: debug_port")
	}
	if s.IntegrationMode == "" {
		s.IntegrationMode = "isolated"
	}
	if s.IntegrationMode != "native" && s.IntegrationMode != "isolated" {
		return fmt.Errorf("invalid Dots setting: integration_mode")
	}
	aliases, profiles, roots := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for handle, dot := range s.Dots {
		if !validText(handle, 80) || handle == "directory" || handle == "board" || !aliasPattern.MatchString(dot.OwnerAlias) || !validText(dot.ProfileID, 128) || !validText(dot.RootThreadID, 128) {
			return fmt.Errorf("invalid Dots profile configuration")
		}
		if aliases[dot.OwnerAlias] || profiles[dot.ProfileID] || roots[dot.RootThreadID] {
			return fmt.Errorf("Dots profile ownership must be unique")
		}
		if dot.DisplayName == "" {
			dot.DisplayName = handle
			dot.legacy = dot.OwnerAccountID == ""
		}
		if dot.OwnerAccountID != "" || dot.DisplayName != "" && !dot.legacy {
			if !validText(dot.DisplayName, 80) || !validText(dot.OwnerAccountID, 256) || AccountAlias(dot.OwnerAccountID) != dot.OwnerAlias || StableHandle(dot.OwnerAccountID, dot.ProfileID) != handle {
				return fmt.Errorf("Dots stable identity mismatch")
			}
		}
		aliases[dot.OwnerAlias], profiles[dot.ProfileID], roots[dot.RootThreadID] = true, true, true
		s.Dots[handle] = dot
	}
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return err
	}
	s.RuntimeRoot = root
	return nil
}

// ValidateRuntimePaths checks mutable directories without creating or changing them.
func ValidateRuntimePaths(root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := privateDirectory(root); err != nil {
		return err
	}
	canonical, err := CanonicalPath(root)
	if err != nil {
		return err
	}
	for _, name := range []string{"user-data", "codex-home", "awareness", "null-executor", "null-executor/home", "null-executor/tmp", "null-executor/work", "null-executor/room-relay"} {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := privateDirectory(path); err != nil {
			return err
		}
		resolved, err := CanonicalPath(path)
		if err != nil || !within(canonical, resolved) {
			return fmt.Errorf("Dots mutable state escapes the private runtime")
		}
	}
	if info, err := os.Lstat(filepath.Join(root, "launch.json")); err == nil {
		if !info.Mode().IsRegular() || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
			return fmt.Errorf("Dots launch state must be an owned regular file with mode 0600")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// StableHandle survives display-name and root-thread changes.
func StableHandle(accountID, profileID string) string {
	digest := sha256.Sum256([]byte(accountID + "\x00" + profileID))
	return "dot-" + hex.EncodeToString(digest[:])[:24]
}

func AccountAlias(accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(digest[:])[:8]
}

func (s *Settings) ProfileIDs() map[string]string {
	result := make(map[string]string, len(s.Dots))
	for _, dot := range s.Dots {
		result[dot.OwnerAlias] = dot.ProfileID
	}
	return result
}

// CatalogueRevision matches Python's sorted compact JSON encoding exactly.
func (s *Settings) CatalogueRevision() string {
	dots, owners, names := map[string][2]string{}, map[string]string{}, map[string]string{}
	for handle, dot := range s.Dots {
		dots[handle] = [2]string{dot.ProfileID, dot.RootThreadID}
		owners[handle], names[handle] = dot.OwnerAlias, dot.DisplayName
	}
	value := map[string]any{"dots": dots, "owners": owners, "names": names}
	data, _ := pythonJSON(value, true)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
