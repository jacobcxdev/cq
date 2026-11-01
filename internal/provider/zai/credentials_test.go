package zai

import (
	"os"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestReadAPIKey(t *testing.T) {
	for _, tt := range []struct {
		name, env, file, want string
		wantErr               bool
	}{
		{name: "missing"},
		{name: "environment", env: " environment-key ", want: "environment-key"},
		{name: "file", file: `{"api_key":"file-key"}`, want: "file-key"},
		{name: "environment wins", env: "environment-key", file: `not json`, want: "environment-key"},
		{name: "empty file key", file: `{"api_key":" "}`},
		{name: "invalid JSON", file: `{"api_key":"private-key",`, wantErr: true},
		{name: "invalid key type", file: `{"api_key":123}`, wantErr: true},
		{name: "header injection", env: "private-key\r\nX-Secret: secret", wantErr: true},
		{name: "file header injection", file: `{"api_key":"private-key\nsecret"}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if tt.file != "" {
				if err := fsys.WriteFile("/config/zai.json", []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			key, err := ReadAPIKey(fsys, func(name string) string {
				if name == "ZAI_API_KEY" {
					return tt.env
				}
				return ""
			}, "/config")
			if (err != nil) != tt.wantErr || key != tt.want {
				t.Fatalf("key present=%t, error=%v; expected key present=%t, error=%t", key != "", err, tt.want != "", tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "private-key") {
				t.Fatal("credential leaked through error")
			}
		})
	}
}

type readErrorFS struct{ fsutil.FileSystem }

func (readErrorFS) ReadFile(string) ([]byte, error) { return nil, os.ErrPermission }

func TestReadAPIKeyReadError(t *testing.T) {
	_, err := ReadAPIKey(readErrorFS{fsutil.NewMemFS()}, func(string) string { return "" }, "/config")
	if err == nil {
		t.Fatal("expected read error")
	}
}
