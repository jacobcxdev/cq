package zcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const fixtureSecret = "cq-zcode-fixture-secret"

// Generated with Node's createCipheriv("aes-256-gcm"), SHA-256(secret), and
// nonce 000102030405060708090a0b, matching ZCode's public credential format.
const fixtureCredentials = `{
  "oauth:active_provider":"enc:v1:AAECAwQFBgcICQoL.e3hYS5ffNh6uNUb9FjO73g.f_Jo",
  "zcodejwttoken":"enc:v1:AAECAwQFBgcICQoL.FDqaDFNPhgBHXlPPZ9mtyQ.f_BuqMHgrUK6bzZsvLLAVHBv7Q",
  "oauth:zai:access_token":"enc:v1:AAECAwQFBgcICQoL.m9_h3HbTHhXrftgXlIUC4A.Z-ZypcqouFjvfSpmrerGXjZ-7P4q3A",
  "unrelated:token":"enc:v1:unreadable"
}`

func TestSessionReaderReadOnlyRealFormat(t *testing.T) {
	fs := fsutil.NewMemFS()
	const path = "/zcode/credentials.json"
	if err := fs.WriteFile(path, []byte(fixtureCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (SessionReader{FS: fs, Path: path, CipherSecret: fixtureSecret}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != "zai" || got.ZCodeJWT != "zcode-fixture-token" || got.BusinessJWT != "business-fixture-token" || got.PlaintextCredentials {
		t.Fatal("session did not match the independently generated fixture")
	}
	after, err := fs.ReadFile(path)
	if err != nil || string(after) != fixtureCredentials {
		t.Fatal("reader changed external credentials")
	}
	data, err := json.Marshal(got)
	if err != nil || strings.Contains(string(data), "fixture-token") || strings.Contains(string(data), "JWT") {
		t.Fatal("public session JSON exposed tokens")
	}
}

func TestSessionReaderInvalidInputsAndSecretRedaction(t *testing.T) {
	for _, test := range []struct {
		name, data, secret string
	}{
		{"wrong secret", fixtureCredentials, "wrong-secret-sensitive"},
		{"missing secret", fixtureCredentials, ""},
		{"malformed JSON", `{"sensitive":"`, fixtureSecret},
		{"invalid value type", `{"oauth:active_provider":42}`, fixtureSecret},
		{"not object", `null`, fixtureSecret},
		{"unknown active family", `{"oauth:active_provider":"bigmodel","zcodejwttoken":"sensitive","oauth:zai:access_token":"sensitive"}`, fixtureSecret},
		{"missing business token", `{"oauth:active_provider":"zai","zcodejwttoken":"sensitive"}`, fixtureSecret},
		{"bad cipher", strings.Replace(fixtureCredentials, "m9_h3HbTHhXrftgXlIUC4A", "m9_h3HbTHhXrftgXlIUC4B", 1), fixtureSecret},
		{"extra cipher part", strings.Replace(fixtureCredentials, "e3hYS5ffNh6uNUb9FjO73g.f_Jo", "e3hYS5ffNh6uNUb9FjO73g.f_Jo.extra", 1), fixtureSecret},
		{"bad nonce", strings.Replace(fixtureCredentials, "AAECAwQFBgcICQoL", "AA", 1), fixtureSecret},
		{"unknown cipher version", strings.ReplaceAll(fixtureCredentials, "enc:v1:", "enc:v2:"), fixtureSecret},
		{"too large", strings.Repeat("sensitive", 1<<18), fixtureSecret},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := fsutil.NewMemFS()
			if err := fs.WriteFile("/credentials", []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (SessionReader{FS: fs, Path: "/credentials", CipherSecret: test.secret}).Read()
			if err == nil || got.ZCodeJWT != "" || got.BusinessJWT != "" {
				t.Fatal("invalid credentials yielded a session")
			}
			for _, secret := range []string{"sensitive", "fixture-token", fixtureSecret, "wrong-secret-sensitive", "enc:v1:"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("credential error exposed input")
				}
			}
		})
	}
	_, err := (SessionReader{FS: fsutil.NewMemFS(), Path: "/missing", CipherSecret: fixtureSecret}).Read()
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing file error = %v", err)
	}
	data, err := json.Marshal(SessionReader{CipherSecret: fixtureSecret})
	if err != nil || strings.Contains(string(data), fixtureSecret) {
		t.Fatal("reader JSON exposed cipher secret")
	}
}

func TestCredentialDecryptEmptyAndPlaintext(t *testing.T) {
	got, err := decryptCredential("enc:v1:AAECAwQFBgcICQoL.MCL4D-uOI_0YA0JYe3q4Wg.", fixtureSecret)
	if err != nil || got != "" {
		t.Fatal("valid encrypted empty value was rejected")
	}
	fs := fsutil.NewMemFS()
	if err := fs.WriteFile("/credentials", []byte(`{"oauth:active_provider":"zai","zcodejwttoken":"zcode-fixture-token","oauth:zai:access_token":"business-fixture-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := (SessionReader{FS: fs, Path: "/credentials"}).Read()
	if err != nil || !session.PlaintextCredentials {
		t.Fatal("legacy plaintext credentials were not identified")
	}
}
