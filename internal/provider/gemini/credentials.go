package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/fsutil"
	gokeyring "github.com/zalando/go-keyring"
)

const (
	antigravityKeychainService  = "gemini"
	antigravityKeychainAccount  = "antigravity"
	antigravityProjectCachePath = ".gemini/antigravity-cli/cache/default_project_id.txt"
	maxProjectIDBytes           = 256
)

var (
	errCredentialsNotFound = errors.New("Antigravity credentials not found")
	errInvalidCredentials  = errors.New("invalid Antigravity credentials")
	errCredentialRead      = errors.New("read Antigravity credentials")
	errInvalidProjectID    = errors.New("invalid Antigravity project ID")
)

type credentialReader interface {
	Get(service, account string) (string, error)
}

type systemCredentialReader struct{}

func (systemCredentialReader) Get(service, account string) (string, error) {
	return gokeyring.Get(service, account)
}

type credentials struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
}

type credentialEnvelope struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Expiry       string `json:"expiry"`
	} `json:"token"`
}

func readCredentials(reader credentialReader) (credentials, error) {
	raw, err := reader.Get(antigravityKeychainService, antigravityKeychainAccount)
	if err != nil {
		if isCredentialNotFound(err) {
			return credentials{}, errCredentialsNotFound
		}
		return credentials{}, errCredentialRead
	}

	var envelope credentialEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return credentials{}, errInvalidCredentials
	}
	expiry, err := time.Parse(time.RFC3339, envelope.Token.Expiry)
	if err != nil {
		return credentials{}, errInvalidCredentials
	}
	return credentials{
		AccessToken:  envelope.Token.AccessToken,
		RefreshToken: envelope.Token.RefreshToken,
		TokenType:    envelope.Token.TokenType,
		Expiry:       expiry,
	}, nil
}

func isCredentialNotFound(err error) bool {
	return errors.Is(err, gokeyring.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

func readProjectID(fsys fsutil.FileSystem) (string, error) {
	home, err := fsys.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Antigravity project cache: %w", err)
	}
	path := filepath.Join(home, antigravityProjectCachePath)
	info, err := fsys.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat Antigravity project cache: %w", err)
	}
	if info.Size() > maxProjectIDBytes {
		return "", errInvalidProjectID
	}
	data, err := fsys.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Antigravity project cache: %w", err)
	}
	if len(data) > maxProjectIDBytes {
		return "", errInvalidProjectID
	}
	return normaliseProjectID(string(data))
}

func normaliseProjectID(raw string) (string, error) {
	if len(raw) > maxProjectIDBytes {
		return "", errInvalidProjectID
	}
	projectID := strings.TrimSpace(raw)
	if projectID == "" || len(projectID) > maxProjectIDBytes || !utf8.ValidString(projectID) {
		return "", errInvalidProjectID
	}
	for _, r := range projectID {
		if unicode.IsControl(r) {
			return "", errInvalidProjectID
		}
	}
	return projectID, nil
}
