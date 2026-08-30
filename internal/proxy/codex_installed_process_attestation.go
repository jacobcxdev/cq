package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttputil "github.com/jacobcxdev/cq/internal/httputil"
)

const (
	codexInstalledVersionOutputMaxBytes = 128
	codexInstalledHealthMaxBytes        = 64 << 10
	codexInstalledExecutableMaxBytes    = 1 << 30
	codexInstalledProcessProofTimeout   = 15 * time.Second
)

var errCodexInstalledProcessAttestation = errors.New("Codex installed process attestation unavailable")

func codexInstalledSecureOSFileSystem() (fsutil.OSFileSystem, fsutil.SecurePathInspector, fsutil.NoFollowFileOpener, bool) {
	fsys := fsutil.OSFileSystem{}
	inspector, inspectorOK := any(fsys).(fsutil.SecurePathInspector)
	opener, openerOK := any(fsys).(fsutil.NoFollowFileOpener)
	return fsys, inspector, opener, inspectorOK && openerOK
}

var (
	codexInstalledClientBuildPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	codexInstalledVersionOutputPattern = regexp.MustCompile(`^codex-cli ([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)\n?$`)
)

type codexInstalledExecutableProof struct {
	path   string
	device uint64
	inode  uint64
	links  uint64
	owner  uint64
	size   int64
	mode   os.FileMode
	sha256 [sha256.Size]byte
}

func (proof codexInstalledExecutableProof) valid() bool {
	return filepath.IsAbs(proof.path) && proof.device != 0 && proof.inode != 0 && proof.links == 1 &&
		proof.size > 0 && proof.size <= codexInstalledExecutableMaxBytes && proof.mode.IsRegular() &&
		proof.mode.Perm()&0o111 != 0 && proof.mode.Perm()&0o022 == 0 &&
		proof.mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && proof.sha256 != ([sha256.Size]byte{})
}

type codexInstalledProcessPlatformProof struct {
	pid                   int
	serviceKind           codexInstalledListenerServiceKind
	persistent            bool
	executable            codexInstalledExecutableProof
	serviceIdentitySHA256 [sha256.Size]byte
}

func (proof codexInstalledProcessPlatformProof) valid() bool {
	if proof.pid <= 1 || !proof.persistent || !proof.executable.valid() || proof.serviceIdentitySHA256 == ([sha256.Size]byte{}) {
		return false
	}
	switch proof.serviceKind {
	case codexInstalledListenerServiceLaunchd, codexInstalledListenerServiceHomebrew, codexInstalledListenerServiceSystemdUser:
		return true
	default:
		return false
	}
}

type codexInstalledProcessPlatformVerifier interface {
	Capture(context.Context) (codexInstalledProcessPlatformProof, error)
}

func captureCurrentCodexInstalledArtifacts(ctx context.Context, clientBuild string) (codexInstalledArtifactRequirement, error) {
	if ctx == nil || ctx.Err() != nil {
		return codexInstalledArtifactRequirement{}, codexInstalledAttestationError(ctx)
	}
	process, err := defaultCodexInstalledProcessPlatformVerifier().Capture(ctx)
	if err != nil || !process.valid() {
		return codexInstalledArtifactRequirement{}, codexInstalledAttestationError(ctx)
	}
	clientPath, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		return codexInstalledArtifactRequirement{}, codexInstalledAttestationError(ctx)
	}
	client, err := newCodexInstalledClientExecutableBuildProbe(
		ctx,
		clientPath,
		clientBuild,
		captureCodexInstalledExecutable,
		runCodexInstalledVersionCommand,
	)
	if err != nil || client == nil || !client.baseline.valid() {
		return codexInstalledArtifactRequirement{}, codexInstalledAttestationError(ctx)
	}
	requirement := codexInstalledArtifactRequirement{
		cqExecutableSHA256:     process.executable.sha256,
		clientExecutableSHA256: client.baseline.sha256,
		serviceKind:            process.serviceKind,
		serviceIdentitySHA256:  process.serviceIdentitySHA256,
	}
	if !requirement.valid() {
		return codexInstalledArtifactRequirement{}, errCodexInstalledProcessAttestation
	}
	return requirement, nil
}

type codexInstalledListenerProcessAuthorityConfig struct {
	cqBuild          string
	clientBuild      string
	clientExecutable string
	listenerAddress  string
	servingAttestor  *ServingAttestor
	nativeHTTP       *CodexNativeHTTPHandler
}

type codexInstalledProcessAttestationDependencies struct {
	platform          codexInstalledProcessPlatformVerifier
	captureExecutable func(string) (codexInstalledExecutableProof, error)
	runVersion        func(context.Context, string, codexInstalledExecutableProof) ([]byte, error)
	dialContext       func(context.Context, string, string) (net.Conn, error)
	installProbe      func(*CodexNativeHTTPHandler, *codexInstalledHTTPGateProbe) (func(), error)
}

// codexInstalledClientExecutableBuildProbe binds every version probe to one
// exact resolved executable identity. Neither its path nor raw command output
// is exposed through readiness evidence.
type codexInstalledClientExecutableBuildProbe struct {
	expectedBuild string
	baseline      codexInstalledExecutableProof
	capture       func(string) (codexInstalledExecutableProof, error)
	runVersion    func(context.Context, string, codexInstalledExecutableProof) ([]byte, error)
}

func newCodexInstalledClientExecutableBuildProbe(
	ctx context.Context,
	path string,
	expectedBuild string,
	capture func(string) (codexInstalledExecutableProof, error),
	runVersion func(context.Context, string, codexInstalledExecutableProof) ([]byte, error),
) (*codexInstalledClientExecutableBuildProbe, error) {
	if ctx == nil || ctx.Err() != nil || strings.TrimSpace(path) == "" || expectedBuild != strings.TrimSpace(expectedBuild) ||
		!codexInstalledClientBuildPattern.MatchString(expectedBuild) || capture == nil || runVersion == nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	baseline, err := capture(path)
	if err != nil || !baseline.valid() {
		return nil, codexInstalledAttestationError(ctx)
	}
	probe := &codexInstalledClientExecutableBuildProbe{
		expectedBuild: expectedBuild,
		baseline:      baseline,
		capture:       capture,
		runVersion:    runVersion,
	}
	if build, err := probe.Probe(ctx); err != nil || build != expectedBuild {
		return nil, codexInstalledAttestationError(ctx)
	}
	return probe, nil
}

func (probe *codexInstalledClientExecutableBuildProbe) Probe(ctx context.Context) (string, error) {
	if ctx == nil || ctx.Err() != nil || probe == nil || !probe.baseline.valid() || probe.capture == nil || probe.runVersion == nil {
		return "", codexInstalledAttestationError(ctx)
	}
	current, err := probe.capture(probe.baseline.path)
	if err != nil || current != probe.baseline {
		return "", codexInstalledAttestationError(ctx)
	}
	output, err := probe.runVersion(ctx, probe.baseline.path, probe.baseline)
	if err != nil {
		return "", codexInstalledAttestationError(ctx)
	}
	after, err := probe.capture(probe.baseline.path)
	if err != nil || after != probe.baseline {
		return "", codexInstalledAttestationError(ctx)
	}
	build, ok := parseCodexInstalledVersionOutput(output)
	if !ok || build != probe.expectedBuild {
		return "", errCodexInstalledProcessAttestation
	}
	return build, nil
}

func parseCodexInstalledVersionOutput(output []byte) (string, bool) {
	if len(output) == 0 || len(output) > codexInstalledVersionOutputMaxBytes {
		return "", false
	}
	match := codexInstalledVersionOutputPattern.FindSubmatch(output)
	if len(match) != 2 {
		return "", false
	}
	return string(match[1]), true
}

// codexInstalledListenerProcessAuthority is both the platform authority and
// the independently repeated exact-client build probe consumed by the harness.
type codexInstalledListenerProcessAuthority struct {
	mu sync.Mutex

	config  codexInstalledListenerProcessAuthorityConfig
	deps    codexInstalledProcessAttestationDependencies
	process codexInstalledProcessPlatformProof
	client  *codexInstalledClientExecutableBuildProbe
	active  bool
}

func newCodexInstalledListenerProcessAuthority(
	ctx context.Context,
	config codexInstalledListenerProcessAuthorityConfig,
) (*codexInstalledListenerProcessAuthority, error) {
	if strings.TrimSpace(config.clientExecutable) == "" {
		path, err := resolveCodexInstalledClientExecutable()
		if err != nil {
			return nil, errCodexInstalledProcessAttestation
		}
		config.clientExecutable = path
	}
	dialer := &net.Dialer{}
	return newCodexInstalledListenerProcessAuthorityWithDependencies(ctx, config, codexInstalledProcessAttestationDependencies{
		platform:          defaultCodexInstalledProcessPlatformVerifier(),
		captureExecutable: captureCodexInstalledExecutable,
		runVersion:        runCodexInstalledVersionCommand,
		dialContext:       dialer.DialContext,
	})
}

func newCodexInstalledListenerProcessAuthorityWithDependencies(
	ctx context.Context,
	config codexInstalledListenerProcessAuthorityConfig,
	deps codexInstalledProcessAttestationDependencies,
) (*codexInstalledListenerProcessAuthority, error) {
	if ctx == nil || ctx.Err() != nil || config.cqBuild != strings.TrimSpace(config.cqBuild) || config.cqBuild == "" ||
		config.clientBuild != strings.TrimSpace(config.clientBuild) || config.clientBuild == "" ||
		config.clientExecutable == "" || config.servingAttestor == nil || config.nativeHTTP == nil ||
		deps.platform == nil || deps.captureExecutable == nil || deps.runVersion == nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	listenerAddress, err := canonicalServingTCP4Address(config.listenerAddress)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	config.listenerAddress = listenerAddress
	if deps.dialContext == nil {
		dialer := &net.Dialer{}
		deps.dialContext = dialer.DialContext
	}
	if deps.installProbe == nil {
		deps.installProbe = func(handler *CodexNativeHTTPHandler, probe *codexInstalledHTTPGateProbe) (func(), error) {
			return handler.installCodexInstalledHTTPGateProbe(probe)
		}
	}
	process, err := deps.platform.Capture(ctx)
	if err != nil || !process.valid() {
		return nil, codexInstalledAttestationError(ctx)
	}
	client, err := newCodexInstalledClientExecutableBuildProbe(
		ctx, config.clientExecutable, config.clientBuild, deps.captureExecutable, deps.runVersion,
	)
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	return &codexInstalledListenerProcessAuthority{
		config:  config,
		deps:    deps,
		process: process,
		client:  client,
	}, nil
}

func (authority *codexInstalledListenerProcessAuthority) Acquire(
	ctx context.Context,
	tuple CodexReadinessTuple,
) (codexInstalledListenerProcessLease, error) {
	if ctx == nil || ctx.Err() != nil || authority == nil || !authority.validTuple(tuple) {
		return nil, codexInstalledAttestationError(ctx)
	}
	authority.mu.Lock()
	if authority.active {
		authority.mu.Unlock()
		return nil, errCodexInstalledProcessAttestation
	}
	authority.active = true
	authority.mu.Unlock()
	succeeded := false
	defer func() {
		if !succeeded {
			authority.release()
		}
	}()

	process, err := authority.deps.platform.Capture(ctx)
	if err != nil || process != authority.process {
		return nil, codexInstalledAttestationError(ctx)
	}
	if build, err := authority.client.Probe(ctx); err != nil || build != tuple.ClientBuild {
		return nil, codexInstalledAttestationError(ctx)
	}
	listenerBinding := codexInstalledListenerProcessBindingDigest(tuple, process, authority.client.baseline, authority.config.listenerAddress)
	if listenerBinding == ([sha256.Size]byte{}) {
		return nil, errCodexInstalledProcessAttestation
	}
	probe, err := newCodexInstalledHTTPGateProbe(listenerBinding)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	servingLease, err := acquireCodexInstalledServingProof(
		ctx, authority.config.listenerAddress, authority.config.servingAttestor, listenerBinding, authority.deps.dialContext,
	)
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	ownedServingLease := servingLease
	defer func() {
		if ownedServingLease != nil {
			ownedServingLease.Release()
		}
	}()
	detach, err := authority.deps.installProbe(authority.config.nativeHTTP, probe)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	binding := codexInstalledListenerProcessBinding{
		CQBuild:                tuple.CQBuild,
		ClientBuild:            tuple.ClientBuild,
		PID:                    process.pid,
		ServiceKind:            process.serviceKind,
		Persistent:             process.persistent,
		ExecutableSHA256:       process.executable.sha256,
		ClientExecutableSHA256: authority.client.baseline.sha256,
		ServiceIdentitySHA256:  process.serviceIdentitySHA256,
		ListenerBinding:        listenerBinding,
	}
	lease := &codexInstalledProcessLease{
		authority:     authority,
		tuple:         tuple,
		binding:       binding,
		process:       process,
		probe:         probe,
		detach:        detach,
		servingLeases: []ServingProofLease{servingLease},
	}
	ownedServingLease = nil
	succeeded = true
	return lease, nil
}

func (authority *codexInstalledListenerProcessAuthority) Probe(ctx context.Context) (string, error) {
	if authority == nil || authority.client == nil {
		return "", errCodexInstalledProcessAttestation
	}
	return authority.client.Probe(ctx)
}

func (authority *codexInstalledListenerProcessAuthority) validTuple(tuple CodexReadinessTuple) bool {
	return authority != nil && tuple.Transport == CodexRoutingHTTP && tuple.CQBuild == authority.config.cqBuild &&
		tuple.ParserSchema > 0 && tuple.LeaseSchema > 0 && strings.TrimSpace(tuple.SemanticsRevision) != "" &&
		tuple.ClientBuild == authority.config.clientBuild && tuple.RetryBudget >= 0 && strings.TrimSpace(tuple.FixtureHash) != ""
}

func (authority *codexInstalledListenerProcessAuthority) release() {
	if authority == nil {
		return
	}
	authority.mu.Lock()
	authority.active = false
	authority.mu.Unlock()
}

type codexInstalledProcessLease struct {
	mu sync.Mutex

	authority     *codexInstalledListenerProcessAuthority
	tuple         CodexReadinessTuple
	binding       codexInstalledListenerProcessBinding
	process       codexInstalledProcessPlatformProof
	probe         *codexInstalledHTTPGateProbe
	detach        func()
	servingLeases []ServingProofLease
	released      bool
}

func (lease *codexInstalledProcessLease) Binding() codexInstalledListenerProcessBinding {
	if lease == nil {
		return codexInstalledListenerProcessBinding{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.binding
}

func (lease *codexInstalledProcessLease) Snapshot(ctx context.Context) (codexInstalledHTTPProbeSnapshot, error) {
	if ctx == nil || ctx.Err() != nil || lease == nil {
		return codexInstalledHTTPProbeSnapshot{}, codexInstalledAttestationError(ctx)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released || lease.authority == nil || lease.probe == nil ||
		lease.authority.config.nativeHTTP.installedProbe.Load() != lease.probe {
		return codexInstalledHTTPProbeSnapshot{}, errCodexInstalledProcessAttestation
	}
	snapshot, err := lease.probe.snapshot(ctx, lease.binding.ListenerBinding)
	if err != nil {
		return codexInstalledHTTPProbeSnapshot{}, errCodexInstalledProcessAttestation
	}
	return snapshot, nil
}

func (lease *codexInstalledProcessLease) Revalidate(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || lease == nil {
		return codexInstalledAttestationError(ctx)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released || lease.authority == nil || lease.probe == nil ||
		lease.authority.config.nativeHTTP.installedProbe.Load() != lease.probe {
		return errCodexInstalledProcessAttestation
	}
	process, err := lease.authority.deps.platform.Capture(ctx)
	if err != nil || process != lease.process {
		return codexInstalledAttestationError(ctx)
	}
	if build, err := lease.authority.client.Probe(ctx); err != nil || build != lease.binding.ClientBuild {
		return codexInstalledAttestationError(ctx)
	}
	wantBinding := codexInstalledListenerProcessBindingDigest(
		lease.tuple, process, lease.authority.client.baseline, lease.authority.config.listenerAddress,
	)
	if wantBinding != lease.binding.ListenerBinding {
		return errCodexInstalledProcessAttestation
	}
	servingLease, err := acquireCodexInstalledServingProof(
		ctx,
		lease.authority.config.listenerAddress,
		lease.authority.config.servingAttestor,
		lease.binding.ListenerBinding,
		lease.authority.deps.dialContext,
	)
	if err != nil {
		return codexInstalledAttestationError(ctx)
	}
	if lease.authority.config.nativeHTTP.installedProbe.Load() != lease.probe {
		servingLease.Release()
		return errCodexInstalledProcessAttestation
	}
	lease.servingLeases = append(lease.servingLeases, servingLease)
	return nil
}

func (lease *codexInstalledProcessLease) Release() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	if lease.released {
		lease.mu.Unlock()
		return
	}
	lease.released = true
	detach := lease.detach
	lease.detach = nil
	servingLeases := append([]ServingProofLease(nil), lease.servingLeases...)
	lease.servingLeases = nil
	authority := lease.authority
	lease.mu.Unlock()
	if detach != nil {
		detach()
	}
	for _, servingLease := range servingLeases {
		servingLease.Release()
	}
	if authority != nil {
		authority.release()
	}
}

func acquireCodexInstalledServingProof(
	ctx context.Context,
	listenerAddress string,
	attestor *ServingAttestor,
	binding [sha256.Size]byte,
	dialContext func(context.Context, string, string) (net.Conn, error),
) (ServingProofLease, error) {
	if ctx == nil || ctx.Err() != nil || listenerAddress == "" || attestor == nil || binding == ([sha256.Size]byte{}) || dialContext == nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	servingLease, err := attestor.Acquire(binding)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	release := true
	defer func() {
		if release {
			servingLease.Release()
		}
	}()

	requestCtx, cancel := context.WithTimeout(ctx, codexInstalledProcessProofTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+listenerAddress+"/health", nil)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	request.Header.Set(ServingProofChallengeHeader, servingLease.Challenge())
	request.Header.Set("Connection", "close")
	var localAddress, remoteAddress string
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil && !info.Reused && !info.WasIdle {
				localAddress = info.Conn.LocalAddr().String()
				remoteAddress = info.Conn.RemoteAddr().String()
			}
		},
	}))
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  true,
		ForceAttemptHTTP2:  false,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != listenerAddress {
				return nil, errCodexInstalledProcessAttestation
			}
			return dialContext(ctx, "tcp4", listenerAddress)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	defer response.Body.Close()
	proofHeaders := response.Header.Values(ServingProofResponseHeader)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Content-Encoding") != "" || len(proofHeaders) != 1 || localAddress == "" || remoteAddress == "" {
		return nil, errCodexInstalledProcessAttestation
	}
	body, err := cqhttputil.ReadBody(response.Body)
	if err != nil || len(body) == 0 || len(body) > codexInstalledHealthMaxBytes {
		return nil, errCodexInstalledProcessAttestation
	}
	if err := servingLease.VerifyResponse(body, proofHeaders[0], localAddress, remoteAddress); err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	if err := servingLease.Seal(); err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	release = false
	return servingLease, nil
}

func codexInstalledListenerProcessBindingDigest(
	tuple CodexReadinessTuple,
	process codexInstalledProcessPlatformProof,
	client codexInstalledExecutableProof,
	listenerAddress string,
) [sha256.Size]byte {
	destination := sha256.New()
	writeCodexInstalledProcessBindingField(destination, []byte("cq-codex-installed-listener-generation-v1"))
	writeCodexInstalledProcessBindingField(destination, []byte(tuple.Transport))
	writeCodexInstalledProcessBindingField(destination, []byte(tuple.CQBuild))
	writeCodexInstalledProcessBindingInt(destination, tuple.ParserSchema)
	writeCodexInstalledProcessBindingInt(destination, tuple.LeaseSchema)
	writeCodexInstalledProcessBindingField(destination, []byte(tuple.SemanticsRevision))
	writeCodexInstalledProcessBindingField(destination, []byte(tuple.ClientBuild))
	writeCodexInstalledProcessBindingInt(destination, tuple.RetryBudget)
	writeCodexInstalledProcessBindingField(destination, []byte(tuple.FixtureHash))
	writeCodexInstalledProcessBindingInt(destination, process.pid)
	writeCodexInstalledProcessBindingField(destination, []byte(process.serviceKind))
	if process.persistent {
		writeCodexInstalledProcessBindingField(destination, []byte{1})
	} else {
		writeCodexInstalledProcessBindingField(destination, []byte{0})
	}
	writeCodexInstalledProcessBindingField(destination, process.executable.sha256[:])
	writeCodexInstalledProcessBindingField(destination, client.sha256[:])
	writeCodexInstalledProcessBindingField(destination, process.serviceIdentitySHA256[:])
	writeCodexInstalledProcessBindingField(destination, []byte(listenerAddress))
	var digest [sha256.Size]byte
	copy(digest[:], destination.Sum(nil))
	return digest
}

func writeCodexInstalledProcessBindingInt(destination hash.Hash, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	writeCodexInstalledProcessBindingField(destination, encoded[:])
}

func writeCodexInstalledProcessBindingField(destination hash.Hash, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func captureCodexInstalledExecutable(path string) (codexInstalledExecutableProof, error) {
	return captureCodexInstalledExecutableWithResolver(path, filepath.EvalSymlinks)
}

func captureCodexInstalledExecutableWithResolver(path string, resolve func(string) (string, error)) (codexInstalledExecutableProof, error) {
	if strings.TrimSpace(path) == "" {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	if !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
		}
		path = resolved
	}
	original := filepath.Clean(path)
	resolved, err := resolve(original)
	if err != nil {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	_, inspector, opener, ok := codexInstalledSecureOSFileSystem()
	if !ok {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	opened, err := opener.OpenNoFollow(resolved)
	if err != nil {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	defer opened.Close()
	before, err := opened.Stat()
	if err != nil {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	owner, ownerOK := inspector.FileOwnerUID(before)
	if !ownerOK || (owner != 0 && owner != inspector.EffectiveUID()) || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o111 == 0 || before.Mode().Perm()&0o022 != 0 ||
		before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		before.Size() <= 0 || before.Size() > codexInstalledExecutableMaxBytes {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	identity, ok := inspector.FileIdentity(before)
	if !ok || identity.Device == 0 || identity.Inode == 0 || identity.Links != 1 {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	destination := sha256.New()
	if copied, err := io.Copy(destination, io.LimitReader(opened, codexInstalledExecutableMaxBytes+1)); err != nil || copied != before.Size() {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	after, err := opened.Stat()
	if err != nil {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	afterIdentity, ok := inspector.FileIdentity(after)
	if !ok || afterIdentity != identity || after.Size() != before.Size() || after.Mode() != before.Mode() {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	pathInfo, err := inspector.Lstat(resolved)
	if err != nil {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	pathIdentity, ok := inspector.FileIdentity(pathInfo)
	if !ok || pathIdentity != identity || pathInfo.Size() != before.Size() || pathInfo.Mode() != before.Mode() {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	finalResolved, err := resolve(original)
	if err != nil || filepath.Clean(finalResolved) != resolved {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	var digest [sha256.Size]byte
	copy(digest[:], destination.Sum(nil))
	proof := codexInstalledExecutableProof{
		path:   resolved,
		device: identity.Device,
		inode:  identity.Inode,
		links:  identity.Links,
		owner:  owner,
		size:   before.Size(),
		mode:   before.Mode(),
		sha256: digest,
	}
	if !proof.valid() {
		return codexInstalledExecutableProof{}, errCodexInstalledProcessAttestation
	}
	return proof, nil
}

type codexInstalledBoundedBuffer struct {
	data  []byte
	limit int
}

func (buffer *codexInstalledBoundedBuffer) Write(data []byte) (int, error) {
	if buffer == nil || buffer.limit <= 0 || len(data) > buffer.limit-len(buffer.data) {
		return 0, errCodexInstalledProcessAttestation
	}
	buffer.data = append(buffer.data, data...)
	return len(data), nil
}

func resolveCodexInstalledClientExecutable() (string, error) {
	for _, path := range []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return resolveCodexInstalledClientExecutableFromPath()
}

func codexInstalledAttestationError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return errCodexInstalledProcessAttestation
}
