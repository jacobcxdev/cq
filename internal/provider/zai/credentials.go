package zai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// ReadAPIKey reads ZAI_API_KEY or CQ's private zai.json credential file.
// Environment credentials take precedence. A missing file is not an error.
func ReadAPIKey(fsys fsutil.FileSystem, env func(string) string, configDir string) (string, error) {
	if key := strings.TrimSpace(env("ZAI_API_KEY")); key != "" {
		return validateAPIKey(key)
	}
	data, err := fsys.ReadFile(filepath.Join(configDir, "zai.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("read Z.ai credentials")
	}
	var credentials struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return "", errors.New("invalid Z.ai credential file")
	}
	return validateAPIKey(strings.TrimSpace(credentials.APIKey))
}

func validateAPIKey(key string) (string, error) {
	for _, r := range key {
		if r <= ' ' || r >= 127 {
			return "", errors.New("invalid Z.ai API key")
		}
	}
	return key, nil
}

// DefaultAPIKey reads credentials from the current user's CQ configuration.
func DefaultAPIKey() (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", errors.New("resolve Z.ai credential directory")
	}
	return ReadAPIKey(fsutil.OSFileSystem{}, os.Getenv, roots.Config)
}
