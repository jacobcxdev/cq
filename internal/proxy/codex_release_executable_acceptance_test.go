//go:build darwin

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttputil "github.com/jacobcxdev/cq/internal/httputil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexReleaseExecutableAcceptanceEnvironment = "CQ_RUN_CODEX_RELEASE_EXECUTABLE_ACCEPTANCE"

type codexReleaseExecutableIsolation struct {
	root        string
	home        string
	codexHome   string
	tmp         string
	cache       string
	config      string
	data        string
	continuity  string
	resilience  string
	claude      string
	diagnostics string
	localToken  string
	port        int
}

type codexReleaseExecutableProcess struct {
	command    *exec.Cmd
	port       int
	localToken string
	done       chan struct{}
	stderr     *codexReleaseExecutableDiagnosticBuffer
	stopOnce   sync.Once
	stopErr    error
}

type codexReleaseSystemProxySnapshot struct {
	listenerPIDs string
	mode         string
	statusCode   int
}

type codexReleaseExecutableDiagnosticBuffer struct {
	mu   sync.Mutex
	data []byte
}

type codexReleaseExecutableRouteEvidence struct {
	httpOK             int
	webSocketProtocols int
	failures           int
	status503          int
	status502          int
}

func codexReleaseExecutableRouteEvidenceFromEvents(events []RouteEvent) codexReleaseExecutableRouteEvidence {
	var evidence codexReleaseExecutableRouteEvidence
	for _, event := range events {
		if event.Provider != "codex" {
			continue
		}
		isNativeHTTP := event.Method == http.MethodPost &&
			((event.RouteKind == "codex_native" && event.Path == legacyCodexResponsesPath) ||
				(event.RouteKind == "codex_compact" && event.Path == legacyCodexCompactResponsesPath))
		isWebSocket := event.RouteKind == "codex_websocket_broker" &&
			event.Method == http.MethodGet &&
			event.Path == legacyCodexResponsesPath
		if !isNativeHTTP && !isWebSocket {
			continue
		}

		success := event.Error == ""
		if isNativeHTTP {
			success = success && event.StatusCode == http.StatusOK
		} else {
			success = success && event.StatusCode == http.StatusSwitchingProtocols
		}
		if !success {
			evidence.failures++
			switch event.StatusCode {
			case http.StatusServiceUnavailable:
				evidence.status503++
			case http.StatusBadGateway:
				evidence.status502++
			}
			continue
		}
		if isNativeHTTP {
			evidence.httpOK++
		} else {
			evidence.webSocketProtocols++
		}
	}
	return evidence
}

func TestCodexReleaseExecutableRouteEvidenceRetainsFailuresBeforeRecovery(t *testing.T) {
	evidence := codexReleaseExecutableRouteEvidenceFromEvents([]RouteEvent{
		{Provider: "codex", RouteKind: "codex_native", Method: http.MethodPost, Path: legacyCodexResponsesPath, StatusCode: http.StatusServiceUnavailable},
		{Provider: "codex", RouteKind: "codex_native", Method: http.MethodPost, Path: legacyCodexResponsesPath, StatusCode: http.StatusOK},
		{Provider: "codex", RouteKind: "codex_compact", Method: http.MethodPost, Path: legacyCodexCompactResponsesPath, StatusCode: http.StatusBadGateway},
		{Provider: "codex", RouteKind: "codex_compact", Method: http.MethodPost, Path: legacyCodexCompactResponsesPath, StatusCode: http.StatusOK},
		{Provider: "codex", RouteKind: "codex_websocket_broker", Method: http.MethodGet, Path: legacyCodexResponsesPath, StatusCode: http.StatusSwitchingProtocols, Error: "api_error:upstream_outcome_indeterminate"},
		{Provider: "codex", RouteKind: "codex_websocket_broker", Method: http.MethodGet, Path: legacyCodexResponsesPath, StatusCode: http.StatusSwitchingProtocols},
		{Provider: "claude", RouteKind: "codex_native", Method: http.MethodPost, Path: legacyCodexResponsesPath, StatusCode: http.StatusOK},
	})
	if evidence.httpOK != 2 || evidence.webSocketProtocols != 1 || evidence.failures != 3 || evidence.status503 != 1 || evidence.status502 != 1 {
		t.Fatalf(
			"release executable route evidence = HTTP %d/WebSocket %d/failures %d/503 %d/502 %d, want 2/1/3/1/1",
			evidence.httpOK,
			evidence.webSocketProtocols,
			evidence.failures,
			evidence.status503,
			evidence.status502,
		)
	}
}

type codexReleaseCredentialSource interface {
	ProtectionSnapshot() (codex.CodexBarProtectionSnapshot, error)
	List(context.Context) ([]codex.ExternalCandidate, error)
	Resolve(context.Context, codex.ExternalCandidateRef) (codex.CredentialMaterial, error)
}

type fakeCodexReleaseCredentialSource struct {
	snapshots  []codex.CodexBarProtectionSnapshot
	candidates []codex.ExternalCandidate
	materials  map[codex.ExternalCandidateRef]codex.CredentialMaterial
	resolved   []codex.ExternalCandidateRef
}

func (source *fakeCodexReleaseCredentialSource) ProtectionSnapshot() (codex.CodexBarProtectionSnapshot, error) {
	if len(source.snapshots) == 0 {
		return codex.CodexBarProtectionSnapshot{}, errors.New("missing protection snapshot")
	}
	snapshot := source.snapshots[0]
	if len(source.snapshots) > 1 {
		source.snapshots = source.snapshots[1:]
	}
	return snapshot, nil
}

func (source *fakeCodexReleaseCredentialSource) List(context.Context) ([]codex.ExternalCandidate, error) {
	return append([]codex.ExternalCandidate(nil), source.candidates...), nil
}

func (source *fakeCodexReleaseCredentialSource) Resolve(_ context.Context, ref codex.ExternalCandidateRef) (codex.CredentialMaterial, error) {
	source.resolved = append(source.resolved, ref)
	material, ok := source.materials[ref]
	if !ok {
		return codex.CredentialMaterial{}, codex.ErrStaleRevision
	}
	return material, nil
}

func TestSnapshotCodexReleaseExecutableCredentialUsesExactAccessOnlyRevision(t *testing.T) {
	accessToken := codexLiveAcceptanceTestJWT(t, map[string]any{"exp": time.Now().Add(30 * time.Minute).Unix()})
	idToken := codexLiveAcceptanceTestJWT(t, map[string]any{
		"email": "validation@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "validation-account",
			"chatgpt_user_id":    "validation-user",
			"chatgpt_plan_type":  "plus",
		},
	})
	ref := codex.ExternalCandidateRef{Source: "codexbar", RecordID: "record", Revision: "revision"}
	proof := codex.CodexBarProtectionSnapshot{DeclaredAuthFiles: 1}
	source := &fakeCodexReleaseCredentialSource{
		snapshots: []codex.CodexBarProtectionSnapshot{proof, proof},
		candidates: []codex.ExternalCandidate{{
			Ref: ref,
			Identity: codex.AccountIdentity{
				AccountID: "validation-account", UserID: "validation-user", Email: "validation@example.test", PlanType: "plus",
			},
			AccessExpiresAt: time.Time{}, Routable: true,
		}},
		materials: map[codex.ExternalCandidateRef]codex.CredentialMaterial{ref: {
			AccessToken: accessToken, RefreshToken: "must-not-copy", IDToken: idToken, AccountID: "validation-account",
		}},
	}

	credential, gotProof, err := snapshotCodexReleaseExecutableCredential(context.Background(), source, codexLiveAcceptanceCredential{})
	if err != nil {
		t.Fatal(err)
	}
	if gotProof != proof || len(source.resolved) != 1 || source.resolved[0] != ref {
		t.Fatal("release validation did not preserve exact CodexBar source revision")
	}
	if credential.material.AccessToken != accessToken || credential.material.IDToken != idToken || credential.material.RefreshToken != "" || credential.localToken == "" {
		t.Fatal("release validation snapshot did not retain only access authority")
	}
}

func TestSnapshotCodexReleaseExecutableCredentialRejectsSourceMutation(t *testing.T) {
	accessToken := codexLiveAcceptanceTestJWT(t, map[string]any{"exp": time.Now().Add(30 * time.Minute).Unix()})
	idToken := codexLiveAcceptanceTestJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "validation-account",
			"chatgpt_user_id":    "validation-user",
		},
	})
	ref := codex.ExternalCandidateRef{Source: "codexbar", RecordID: "record", Revision: "revision"}
	source := &fakeCodexReleaseCredentialSource{
		snapshots: []codex.CodexBarProtectionSnapshot{{DeclaredAuthFiles: 1}, {DeclaredAuthFiles: 2}},
		candidates: []codex.ExternalCandidate{{
			Ref: ref, Identity: codex.AccountIdentity{AccountID: "validation-account", UserID: "validation-user"},
			AccessExpiresAt: time.Now().Add(30 * time.Minute), Routable: true,
		}},
		materials: map[codex.ExternalCandidateRef]codex.CredentialMaterial{ref: {
			AccessToken: accessToken, IDToken: idToken, AccountID: "validation-account",
		}},
	}
	if _, _, err := snapshotCodexReleaseExecutableCredential(context.Background(), source, codexLiveAcceptanceCredential{}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("release validation accepted a changing CodexBar source")
	}
}

func TestSnapshotCodexReleaseExecutableCredentialExcludesSystemCallerIdentity(t *testing.T) {
	callerAccessToken := codexLiveAcceptanceTestJWT(t, map[string]any{
		"exp": time.Now().Add(30 * time.Minute).Unix(), "jti": "caller",
	})
	routingAccessToken := codexLiveAcceptanceTestJWT(t, map[string]any{
		"exp": time.Now().Add(30 * time.Minute).Unix(), "jti": "routing",
	})
	idToken := func(account, user string) string {
		return codexLiveAcceptanceTestJWT(t, map[string]any{
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account, "chatgpt_user_id": user},
		})
	}
	matchingRef := codex.ExternalCandidateRef{Source: "codexbar", RecordID: "matching", Revision: "revision-a"}
	sameBearerRef := codex.ExternalCandidateRef{Source: "codexbar", RecordID: "same-bearer", Revision: "revision-b"}
	distinctRef := codex.ExternalCandidateRef{Source: "codexbar", RecordID: "distinct", Revision: "revision-c"}
	proof := codex.CodexBarProtectionSnapshot{DeclaredAuthFiles: 3}
	source := &fakeCodexReleaseCredentialSource{
		snapshots: []codex.CodexBarProtectionSnapshot{proof, proof},
		candidates: []codex.ExternalCandidate{
			{Ref: matchingRef, Identity: codex.AccountIdentity{AccountID: "system-account", UserID: "system-user"}, Routable: true},
			{Ref: sameBearerRef, Identity: codex.AccountIdentity{AccountID: "other-account", UserID: "other-user"}, Routable: true},
			{Ref: distinctRef, Identity: codex.AccountIdentity{AccountID: "routing-account", UserID: "routing-user"}, Routable: true},
		},
		materials: map[codex.ExternalCandidateRef]codex.CredentialMaterial{
			matchingRef:   {AccessToken: callerAccessToken, IDToken: idToken("system-account", "system-user"), AccountID: "system-account"},
			sameBearerRef: {AccessToken: callerAccessToken, IDToken: idToken("other-account", "other-user"), AccountID: "other-account"},
			distinctRef:   {AccessToken: routingAccessToken, IDToken: idToken("routing-account", "routing-user"), AccountID: "routing-account"},
		},
	}

	clientCredential := codexLiveAcceptanceCredential{
		identity: codex.AccountIdentity{AccountID: "system-account", UserID: "system-user"},
		material: codex.CredentialMaterial{AccessToken: callerAccessToken},
	}
	credential, _, err := snapshotCodexReleaseExecutableCredential(context.Background(), source, clientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.resolved) != 2 || source.resolved[0] != sameBearerRef || source.resolved[1] != distinctRef ||
		credential.identity.AccountID != "routing-account" || credential.material.AccessToken != routingAccessToken {
		t.Fatalf("resolved refs = %#v, identity = %#v", source.resolved, credential.identity)
	}
	matchingOnly := &fakeCodexReleaseCredentialSource{
		snapshots:  []codex.CodexBarProtectionSnapshot{proof, proof},
		candidates: source.candidates[:1],
		materials:  source.materials,
	}
	if _, _, err := snapshotCodexReleaseExecutableCredential(context.Background(), matchingOnly, clientCredential); err == nil || len(matchingOnly.resolved) != 0 {
		t.Fatal("release validation accepted system caller auth as routing auth")
	}
}

func snapshotCodexReleaseExecutableCredential(ctx context.Context, source codexReleaseCredentialSource, excludedCredential codexLiveAcceptanceCredential) (codexLiveAcceptanceCredential, codex.CodexBarProtectionSnapshot, error) {
	var result codexLiveAcceptanceCredential
	before, err := source.ProtectionSnapshot()
	if err != nil {
		return result, before, errors.New("snapshot normal CQ Codex auth")
	}
	candidates, err := source.List(ctx)
	if err != nil {
		return result, before, errors.New("list normal CQ Codex auth")
	}
	minimumExpiry := time.Now().Add(codexLiveAcceptanceMinimumCredentialLifetime)
	for _, candidate := range candidates {
		if !candidate.Routable || candidate.Identity.AccountID == "" || candidate.Identity.UserID == "" {
			continue
		}
		if excludedCredential.identity.AccountID != "" && excludedCredential.identity.UserID != "" &&
			candidate.Identity.AccountID == excludedCredential.identity.AccountID && candidate.Identity.UserID == excludedCredential.identity.UserID {
			continue
		}
		material, resolveErr := source.Resolve(ctx, candidate.Ref)
		if resolveErr != nil {
			return result, before, errors.New("resolve exact normal CQ Codex auth revision")
		}
		identityClaims := auth.DecodeCodexClaims(material.IDToken)
		accessClaims := auth.DecodeCodexClaims(material.AccessToken)
		if material.AccessToken == "" || material.IDToken == "" || material.AccountID != candidate.Identity.AccountID ||
			identityClaims.AccountID != candidate.Identity.AccountID || identityClaims.UserID != candidate.Identity.UserID {
			return result, before, errors.New("normal CQ Codex auth identity mismatch")
		}
		if excludedCredential.material.AccessToken != "" && material.AccessToken == excludedCredential.material.AccessToken {
			continue
		}
		if accessClaims.ExpiresAt == 0 || time.Unix(accessClaims.ExpiresAt, 0).Before(minimumExpiry) {
			continue
		}
		localToken, tokenErr := newCodexInstalledHTTPValidationToken()
		if tokenErr != nil {
			return result, before, tokenErr
		}
		material.RefreshToken = ""
		result = codexLiveAcceptanceCredential{
			identity: candidate.Identity, material: material, localToken: localToken,
		}
		break
	}
	after, err := source.ProtectionSnapshot()
	if err != nil || after != before {
		return codexLiveAcceptanceCredential{}, before, errors.New("normal CQ Codex auth changed while staging validation access")
	}
	if result.material.AccessToken == "" {
		return result, before, errors.New("normal CQ has no fresh distinct routable Codex auth for validation")
	}
	return result, before, nil
}

func TestWriteCodexReleaseExecutableAccessSnapshotOmitsRefreshAuthority(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	credential := codexLiveAcceptanceCredential{material: codex.CredentialMaterial{
		AccessToken:  "access-secret",
		RefreshToken: "must-not-copy",
		IDToken:      "identity-secret",
		AccountID:    "validation-account",
	}}

	if err := writeCodexReleaseExecutableAccessSnapshot(path, credential); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(credential.material.RefreshToken)) {
		t.Fatal("snapshot retained refresh authority")
	}
	var document struct {
		AuthMode    string `json:"auth_mode"`
		CQExpiresAt int64  `json:"cq_expires_at"`
		Metadata    struct {
			Version        int                  `json:"version"`
			OperationState codex.OperationState `json:"operation_state"`
		} `json:"_cq"`
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.AuthMode != "chatgpt" || document.Tokens.AccessToken != credential.material.AccessToken ||
		document.Tokens.IDToken != credential.material.IDToken || document.Tokens.AccountID != credential.material.AccountID ||
		document.Tokens.RefreshToken != "" || document.Metadata.Version != 1 || document.Metadata.OperationState != codex.OperationReady {
		t.Fatal("snapshot did not contain only the required access material")
	}
}

func TestWriteCodexReleaseExecutableClientAuthSnapshotUsesSystemAccessOnly(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	credential := codexLiveAcceptanceCredential{material: codex.CredentialMaterial{
		AccessToken:  "system-access-secret",
		RefreshToken: "must-not-copy",
		IDToken:      "system-identity-secret",
		AccountID:    "system-account",
	}}

	if err := writeCodexReleaseExecutableClientAuthSnapshot(path, credential); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("client snapshot mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(credential.material.RefreshToken)) {
		t.Fatal("client snapshot retained refresh authority")
	}
	var document struct {
		AuthMode    string `json:"auth_mode"`
		LastRefresh string `json:"last_refresh"`
		Tokens      struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.AuthMode != "chatgpt" || document.Tokens.AccessToken != credential.material.AccessToken ||
		document.Tokens.IDToken != credential.material.IDToken || document.Tokens.AccountID != credential.material.AccountID ||
		document.Tokens.RefreshToken != "" || document.LastRefresh == "" {
		t.Fatal("client snapshot did not contain exact access-only system auth")
	}
}

func TestNewCodexReleaseExecutableIsolationSeparatesSystemCallerFromRoutingAuth(t *testing.T) {
	routingCredential := codexLiveAcceptanceCredential{
		material: codex.CredentialMaterial{
			AccessToken: "routing-access-secret", IDToken: "routing-identity-secret", AccountID: "routing-account",
		},
		localToken: "local-validation-token",
	}
	clientCredential := codexLiveAcceptanceCredential{material: codex.CredentialMaterial{
		AccessToken: "system-access-secret", IDToken: "system-identity-secret", AccountID: "system-account",
	}}

	isolation := newCodexReleaseExecutableIsolation(t, routingCredential, clientCredential)
	body, err := os.ReadFile(filepath.Join(isolation.codexHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(clientCredential.material.AccessToken)) ||
		bytes.Contains(body, []byte(routingCredential.material.AccessToken)) {
		t.Fatal("private CQ system auth did not remain separate from routing auth")
	}
	config, err := loadConfigFile(filepath.Join(isolation.config, "cq", "proxy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config.CodexRoutingPinnedAccountKey != "release-validation-account" {
		t.Fatalf("validation routing pin = %q, want release-validation-account", config.CodexRoutingPinnedAccountKey)
	}
	if config.DiagnosticsLog != isolation.diagnostics || filepath.Dir(config.DiagnosticsLog) != isolation.root {
		t.Fatalf("validation diagnostics log = %q, want isolated path %q", config.DiagnosticsLog, isolation.diagnostics)
	}
	managedBody, err := os.ReadFile(filepath.Join(isolation.codexHome, "accounts", "release-validation.auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var managed struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(managedBody, &managed); err != nil {
		t.Fatal(err)
	}
	if managed.Tokens.AccountID != routingCredential.material.AccountID || managed.Tokens.AccountID == clientCredential.material.AccountID {
		t.Fatal("validation routing auth did not remain distinct from system caller auth")
	}
}

func TestNewCodexReleaseExecutableIsolationInitialisesRescueState(t *testing.T) {
	credential := codexLiveAcceptanceCredential{
		material: codex.CredentialMaterial{
			AccessToken: "access-secret", IDToken: "identity-secret", AccountID: "validation-account",
		},
		localToken: "local-validation-token",
	}
	isolation := newCodexReleaseExecutableIsolation(t, credential, credential)
	temporaryCredentialSocket := filepath.Join(isolation.config, "cq", "state", ".cq-credential-"+strings.Repeat("f", 32)+".sock")
	if len(temporaryCredentialSocket) >= len(syscall.RawSockaddrUnix{}.Path) {
		t.Fatalf("validation credential socket path has %d bytes, limit is %d", len(temporaryCredentialSocket), len(syscall.RawSockaddrUnix{}.Path)-1)
	}
	state, err := OpenProxyRescueState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: isolation.resilience, Random: strings.NewReader(strings.Repeat("r", 4096)), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexExactExecutableNormalPassesThroughLiveUpstream(t *testing.T) {
	if os.Getenv(codexReleaseExecutableAcceptanceEnvironment) != "1" {
		t.Skip("release executable acceptance requires explicit opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	productionBefore, err := captureCodexReleaseSystemProxySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	productionChecked := false
	t.Cleanup(func() {
		if productionChecked {
			return
		}
		after, checkErr := captureCodexReleaseSystemProxySnapshot()
		if checkErr != nil || after != productionBefore {
			t.Error("system proxy changed during release executable acceptance")
		}
	})
	codexBarRoot := os.Getenv("CQ_CODEXBAR_LIVE_ROOT")
	if !filepath.IsAbs(codexBarRoot) {
		t.Fatal("CQ_CODEXBAR_LIVE_ROOT must be absolute")
	}
	clientCredential, err := readCodexLiveAcceptanceCredential(os.Getenv("CQ_CODEX_LIVE_AUTH_FILE"))
	if err != nil {
		t.Fatalf("snapshot system Codex client auth: %v", err)
	}
	source := codex.NewCodexBarSource(codexBarRoot)
	routingCredential, sourceBefore, err := snapshotCodexReleaseExecutableCredential(ctx, source, clientCredential)
	if err != nil {
		t.Fatal(err)
	}
	sourceChecked := false
	t.Cleanup(func() {
		if sourceChecked {
			return
		}
		after, checkErr := source.ProtectionSnapshot()
		if checkErr != nil || after != sourceBefore {
			t.Error("normal CQ Codex auth changed during release executable acceptance")
		}
	})

	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	cqPath := os.Getenv("CQ_CODEX_PROXY_ACCEPTANCE_EXECUTABLE")
	if !filepath.IsAbs(cqPath) {
		t.Fatal("CQ release acceptance executable must be absolute")
	}
	cqProof, err := captureCodexInstalledExecutable(cqPath)
	if err != nil {
		t.Fatalf("capture CQ release acceptance executable: %v", err)
	}

	isolation := newCodexReleaseExecutableIsolation(t, routingCredential, clientCredential)
	process, err := startCodexReleaseExecutableProcess(cqProof, isolation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if stopErr := process.Stop(); stopErr != nil {
			t.Error(stopErr)
		}
	})
	baseURL := "http://127.0.0.1:" + strconv.Itoa(isolation.port)
	if err := waitCodexReleaseExecutableNormalReady(ctx, process, baseURL); err != nil {
		t.Fatal(err)
	}
	if err := verifyCodexReleaseExecutableListenerOwnership(process); err != nil {
		t.Fatal(err)
	}
	for _, webSocket := range []bool{false, true} {
		appServerIsolation := newCodexTaskAffinityAcceptanceIsolationDirectories(t)
		if err := writeCodexReleaseExecutableClientAuthSnapshot(filepath.Join(appServerIsolation.codexHome, "auth.json"), clientCredential); err != nil {
			t.Fatal(err)
		}
		if err := runCodexAppServerContinuityAcceptance(ctx, clientProof, baseURL, appServerIsolation, webSocket); err != nil {
			t.Fatalf("release executable normal continuity (websocket=%t): %v; CQ diagnostics: %s", webSocket, err, process.diagnostic())
		}
		if err := waitCodexReleaseExecutableNormalReady(ctx, process, baseURL); err != nil {
			t.Fatalf("release executable lost normal readiness (websocket=%t): %v", webSocket, err)
		}
	}
	afterCQ, err := captureCodexInstalledExecutable(cqProof.path)
	if err != nil || afterCQ != cqProof {
		t.Fatal("CQ release acceptance executable changed during validation")
	}
	if err := process.Stop(); err != nil {
		t.Fatal(err)
	}
	evidence := codexReleaseExecutableRouteEvidenceFromEvents(readDiagnosticsEvents(t, isolation.diagnostics))
	if evidence.httpOK == 0 || evidence.webSocketProtocols == 0 || evidence.failures != 0 {
		t.Fatalf(
			"release executable normal route evidence = HTTP POST /responses-or-compact 200: %d, WebSocket GET /responses 101: %d, failures: %d (503: %d, 502: %d), want positive/positive/0",
			evidence.httpOK,
			evidence.webSocketProtocols,
			evidence.failures,
			evidence.status503,
			evidence.status502,
		)
	}
	t.Logf(
		"release executable normal route evidence: port=%d HTTP POST /responses-or-compact 200=%d WebSocket GET /responses 101=%d",
		isolation.port,
		evidence.httpOK,
		evidence.webSocketProtocols,
	)
	sourceAfter, err := source.ProtectionSnapshot()
	if err != nil || sourceAfter != sourceBefore {
		t.Fatal("normal CQ Codex auth changed during release executable acceptance")
	}
	productionAfter, err := captureCodexReleaseSystemProxySnapshot()
	if err != nil || productionAfter != productionBefore {
		t.Fatal("system proxy changed during release executable acceptance")
	}
	productionChecked = true
	sourceChecked = true
}

func writeCodexReleaseExecutableClientAuthSnapshot(path string, credential codexLiveAcceptanceCredential) error {
	material := credential.material
	if material.AccessToken == "" || material.IDToken == "" || material.AccountID == "" || !filepath.IsAbs(path) {
		return errors.New("release acceptance client access material is incomplete")
	}
	document := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"last_refresh":   time.Now().UTC().Format(time.RFC3339Nano),
		"tokens": map[string]any{
			"access_token":  material.AccessToken,
			"refresh_token": "",
			"id_token":      material.IDToken,
			"account_id":    material.AccountID,
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func writeCodexReleaseExecutableAccessSnapshot(path string, credential codexLiveAcceptanceCredential) error {
	material := credential.material
	if material.AccessToken == "" || material.IDToken == "" || material.AccountID == "" || !filepath.IsAbs(path) {
		return errors.New("release acceptance access material is incomplete")
	}
	document := map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil}
	if expiresAt := auth.DecodeCodexClaims(material.AccessToken).ExpiresAt; expiresAt > 0 {
		document["cq_expires_at"] = expiresAt * 1000
	}
	record := codex.ManagedRecord{
		Path: path,
		Metadata: codex.ManagedMetadata{
			Version: 1, AccountKey: "release-validation-account", CandidateID: "release-validation-candidate",
			LineageID: "release-validation-lineage", Generation: 1,
			Provenance: codex.ProvenanceLegacyUnknown, RefreshOwnership: codex.RefreshOwnershipUnknown,
			OperationState: codex.OperationReady,
		},
		Credential: codex.CredentialMaterial{
			AccessToken: material.AccessToken,
			IDToken:     material.IDToken,
			AccountID:   material.AccountID,
		},
		Document: document,
	}
	store := &codex.ManagedStore{
		FS: fsutil.OSFileSystem{}, Home: filepath.Dir(filepath.Dir(filepath.Dir(path))),
		EnsureEpoch: func() error { return nil },
	}
	if err := store.Commit(&record, ""); err != nil {
		return err
	}
	loaded, err := store.Load(path)
	if err != nil {
		return err
	}
	if loaded.Credential.RefreshToken != "" || !loaded.RefreshSuspended || codex.RefreshEligible(loaded) {
		return errors.New("release acceptance credential retained refresh authority")
	}
	return nil
}

func newCodexReleaseExecutableIsolation(t *testing.T, routingCredential, clientCredential codexLiveAcceptanceCredential) codexReleaseExecutableIsolation {
	t.Helper()
	tmpRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpRoot, "cqv-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	isolation := codexReleaseExecutableIsolation{
		root: root, home: filepath.Join(root, "home"), tmp: filepath.Join(root, "tmp"),
		cache: filepath.Join(root, "cache"), config: filepath.Join(root, "config"), data: filepath.Join(root, "data"),
		continuity: filepath.Join(root, "continuity"), resilience: filepath.Join(root, "resilience"),
		claude: filepath.Join(root, "claude"), diagnostics: filepath.Join(root, "routes.jsonl"), localToken: routingCredential.localToken,
	}
	isolation.codexHome = filepath.Join(isolation.home, ".codex")
	for _, directory := range []string{isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config, isolation.data, isolation.continuity, isolation.resilience, isolation.claude} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCodexReleaseExecutableClientAuthSnapshot(filepath.Join(isolation.codexHome, "auth.json"), clientCredential); err != nil {
		t.Fatal(err)
	}
	if err := InitialiseProxyResilienceState(context.Background(), ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: isolation.resilience, Random: rand.Reader, Now: time.Now,
	}); err != nil {
		t.Fatal(err)
	}
	isolation.port = reserveCodexReleaseExecutablePort(t)
	if isolation.port == DefaultPort {
		t.Fatal("release executable acceptance selected system proxy port")
	}
	managedPath := filepath.Join(isolation.codexHome, "accounts", "release-validation.auth.json")
	if err := writeCodexReleaseExecutableAccessSnapshot(managedPath, routingCredential); err != nil {
		t.Fatal(err)
	}
	config := &Config{
		Port: isolation.port, ClaudeUpstream: DefaultUpstream, CodexUpstream: DefaultCodexUpstream,
		LocalToken: routingCredential.localToken, DiagnosticsLog: isolation.diagnostics,
		CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce,
		CodexRoutingPinnedAccountKey: "release-validation-account",
		CodexLeaseRetentionDays:      7, CodexContinuityStateDir: isolation.continuity,
		ProxyResilienceStateDir: isolation.resilience, CodexWindowPriming: CodexWindowPrimingConfig{Enabled: false},
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(filepath.Join(isolation.config, "cq", "proxy.json"), config); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := json.Marshal(ProxyRescueBootstrapConfig{
		SchemaVersion: 1,
		LocalToken:    config.LocalToken,
		StateRoot:     config.ProxyResilienceStateDir,
		Port:          config.Port,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.SecureAtomicWrite(
		fsutil.OSFileSystem{},
		filepath.Join(isolation.config, "cq", proxyRescueBootstrapName),
		bootstrap,
	); err != nil {
		t.Fatal(err)
	}
	return isolation
}

func reserveCodexReleaseExecutablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startCodexReleaseExecutableProcess(proof codexInstalledExecutableProof, isolation codexReleaseExecutableIsolation) (*codexReleaseExecutableProcess, error) {
	current, err := captureCodexInstalledExecutable(proof.path)
	if err != nil || current != proof {
		return nil, errors.New("CQ release acceptance executable changed before launch")
	}
	environment := codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config)
	for index := range environment {
		if environment[index] == "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin" {
			environment[index] = "PATH=/bin:/usr/sbin:/sbin"
		}
	}
	profile, err := codexReleaseExecutableSandboxProfile(isolation.root)
	if err != nil {
		return nil, err
	}
	command := exec.Command(
		"/usr/bin/sandbox-exec", "-p", profile,
		proof.path, "proxy", "start", "--port", strconv.Itoa(isolation.port),
	)
	command.Env = append(environment,
		"XDG_DATA_HOME="+isolation.data,
		"CLAUDE_CONFIG_DIR="+isolation.claude,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	command.Dir = isolation.root
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	diagnostic := &codexReleaseExecutableDiagnosticBuffer{}
	command.Stdout = diagnostic
	command.Stderr = diagnostic
	if err := command.Start(); err != nil {
		return nil, errors.New("start CQ release acceptance executable")
	}
	process := &codexReleaseExecutableProcess{
		command: command, port: isolation.port, localToken: isolation.localToken,
		done: make(chan struct{}), stderr: diagnostic,
	}
	go func() {
		_ = command.Wait()
		close(process.done)
	}()
	return process, nil
}

func codexReleaseExecutableSandboxProfile(root string) (string, error) {
	clean := filepath.Clean(root)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || resolved != clean || !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("CQ release acceptance sandbox root is unsafe")
	}
	return `(version 1) (allow default) (deny file-write*)` +
		` (allow file-write* (literal "/dev/null"))` +
		` (allow file-write* (subpath ` + strconv.Quote(clean) + `))` +
		` (deny process-exec (literal "/usr/bin/security"))`, nil
}

func waitCodexReleaseExecutableNormalReady(ctx context.Context, process *codexReleaseExecutableProcess, baseURL string) error {
	readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ctx = readyCtx
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("CQ release acceptance normal readiness timed out: %s", process.diagnostic())
		case <-process.done:
			return fmt.Errorf("CQ release acceptance exited before readiness: %s", process.diagnostic())
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		if err != nil {
			return err
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			body, bodyErr := cqhttputil.ReadBody(response.Body)
			closeErr := response.Body.Close()
			var health struct {
				Status   string `json:"status"`
				Accounts struct {
					Claude int `json:"claude"`
				} `json:"accounts"`
				HTTPRouting      CodexModeStatus `json:"codex_turn_routing"`
				WebSocketRouting CodexModeStatus `json:"codex_ws_turn_routing"`
			}
			if bodyErr == nil && closeErr == nil && response.StatusCode == http.StatusOK && json.Unmarshal(body, &health) == nil &&
				health.Status == "ok" && health.Accounts.Claude == 0 &&
				health.HTTPRouting.Configured == CodexRoutingEnforce && health.HTTPRouting.Effective == CodexRoutingEnforce &&
				health.WebSocketRouting.Configured == CodexRoutingEnforce && health.WebSocketRouting.Effective == CodexRoutingEnforce &&
				strings.Contains(process.diagnostic(), "cq: claude accounts: 0") {
				if err := verifyCodexReleaseExecutableNormalMode(ctx, client, baseURL, process.localToken); err == nil {
					return nil
				}
			}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("CQ release acceptance normal readiness timed out: %s", process.diagnostic())
		case <-process.done:
			timer.Stop()
			return fmt.Errorf("CQ release acceptance exited before readiness: %s", process.diagnostic())
		case <-timer.C:
		}
	}
}

func verifyCodexReleaseExecutableNormalMode(ctx context.Context, client *http.Client, baseURL, localToken string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+RuntimeRescueStatusPath, http.NoBody)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+localToken)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	body, readErr := cqhttputil.ReadBody(response.Body)
	closeErr := response.Body.Close()
	var status struct {
		Mode TrafficMode `json:"mode"`
	}
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || json.Unmarshal(body, &status) != nil || status.Mode != TrafficModeNormal {
		return errors.New("CQ release acceptance proxy is not in normal mode")
	}
	return nil
}

func verifyCodexReleaseExecutableListenerOwnership(process *codexReleaseExecutableProcess) error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return errors.New("CQ release acceptance process is unavailable")
	}
	pids, err := codexReleaseListenerPIDs(process.port)
	if err != nil || len(pids) == 0 {
		return errors.New("CQ release acceptance listener ownership is unavailable")
	}
	wantGroup := process.command.Process.Pid
	for _, pid := range pids {
		group, groupErr := syscall.Getpgid(pid)
		if groupErr == nil && group == wantGroup {
			return nil
		}
	}
	return errors.New("CQ release acceptance listener is outside validation process group")
}

func captureCodexReleaseSystemProxySnapshot() (codexReleaseSystemProxySnapshot, error) {
	var snapshot codexReleaseSystemProxySnapshot
	pids, err := codexReleaseListenerPIDs(DefaultPort)
	if err != nil {
		return snapshot, err
	}
	if len(pids) == 0 {
		return snapshot, nil
	}
	values := make([]string, len(pids))
	for index, pid := range pids {
		values[index] = strconv.Itoa(pid)
	}
	snapshot.listenerPIDs = strings.Join(values, ",")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(DefaultPort) + "/health")
	if err != nil {
		return snapshot, errors.New("read system proxy health")
	}
	body, readErr := cqhttputil.ReadBody(response.Body)
	closeErr := response.Body.Close()
	var health struct {
		Status string      `json:"status"`
		Mode   TrafficMode `json:"mode"`
	}
	if readErr != nil || closeErr != nil || json.Unmarshal(body, &health) != nil || health.Status == "" {
		return snapshot, errors.New("decode system proxy health")
	}
	snapshot.statusCode = response.StatusCode
	snapshot.mode = string(health.Mode)
	if snapshot.mode == "" {
		snapshot.mode = string(TrafficModeNormal)
	}
	return snapshot, nil
}

func codexReleaseListenerPIDs(port int) ([]int, error) {
	command := exec.Command("/usr/sbin/lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, errors.New("inspect CQ listener ownership")
	}
	fields := strings.Fields(string(output))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || pid <= 1 {
			return nil, errors.New("decode CQ listener ownership")
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func (process *codexReleaseExecutableProcess) Stop() error {
	if process == nil {
		return nil
	}
	process.stopOnce.Do(func() {
		pid := process.command.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			process.stopErr = fmt.Errorf("stop CQ release acceptance process group: %w", err)
		}
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				process.stopErr = errors.Join(process.stopErr, fmt.Errorf("kill CQ release acceptance process group: %w", err))
			}
			select {
			case <-process.done:
			case <-time.After(5 * time.Second):
				process.stopErr = errors.Join(process.stopErr, errors.New("CQ release acceptance process group did not exit"))
			}
		}
		if !waitCodexReleaseExecutableProcessGroupGone(pid, 2*time.Second) {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				process.stopErr = errors.Join(process.stopErr, fmt.Errorf("kill remaining CQ release acceptance process group: %w", err))
			}
			if !waitCodexReleaseExecutableProcessGroupGone(pid, 2*time.Second) {
				process.stopErr = errors.Join(process.stopErr, errors.New("CQ release acceptance process group remains alive"))
			}
		}
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(process.port))
		if connection, err := net.DialTimeout("tcp4", address, 200*time.Millisecond); err == nil {
			_ = connection.Close()
			process.stopErr = errors.Join(process.stopErr, errors.New("CQ release acceptance listener remains reachable"))
		}
	})
	return process.stopErr
}

func waitCodexReleaseExecutableProcessGroupGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if err != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (process *codexReleaseExecutableProcess) diagnostic() string {
	if process == nil || process.stderr == nil {
		return ""
	}
	return process.stderr.String()
}

func (buffer *codexReleaseExecutableDiagnosticBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	const limit = 32 << 10
	remaining := limit - len(buffer.data)
	if remaining > 0 {
		buffer.data = append(buffer.data, value[:min(len(value), remaining)]...)
	}
	return len(value), nil
}

func (buffer *codexReleaseExecutableDiagnosticBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return sanitiseCodexAcceptanceDiagnostic(string(buffer.data))
}
