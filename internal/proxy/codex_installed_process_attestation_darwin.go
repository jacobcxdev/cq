//go:build darwin

package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	codexInstalledLaunchdServiceLabel   = "dev.jacobcx.cq.proxy"
	codexInstalledHomebrewServiceLabel  = "homebrew.mxcl.cq"
	codexInstalledCandidateServiceLabel = "dev.jacobcx.cq.proxy.candidate"

	codexInstalledLaunchctlOutputMaxBytes = 256 << 10
	codexInstalledServicePlistMaxBytes    = 256 << 10

	codexInstalledDarwinProcInfoCallPIDInfo = 2
	codexInstalledDarwinPIDRegionPathInfo   = 8
	codexInstalledDarwinVMProtectionExecute = 4
	codexInstalledDarwinOExec               = 0x40000000
	codexInstalledDarwinMaxRegions          = 4096
)

type codexInstalledDarwinProcessVerifierDependencies struct {
	pid                         func() int
	uid                         func() int
	executablePath              func() (string, error)
	launchctlPrint              func(context.Context, string) ([]byte, error)
	captureExecutable           func(string) (codexInstalledExecutableProof, error)
	verifyMappedExecutable      func(int, codexInstalledExecutableProof) error
	captureServiceConfiguration func(string) (codexInstalledServiceConfigurationProof, []byte, error)
}

type codexInstalledDarwinProcessVerifier struct {
	dependencies codexInstalledDarwinProcessVerifierDependencies
}

type codexInstalledDarwinLaunchctlJob struct {
	target      string
	path        string
	jobType     string
	state       string
	program     string
	arguments   []string
	properties  []string
	pid         int
	activeCount int
	keepAlive   bool
}

type codexInstalledDarwinServiceConfiguration struct {
	label            string
	programArguments []string
	keepAlive        bool
}

type codexInstalledServiceConfigurationProof struct {
	path   string
	device uint64
	inode  uint64
	links  uint64
	owner  uint64
	size   int64
	mode   os.FileMode
	sha256 [sha256.Size]byte
}

func defaultCodexInstalledProcessPlatformVerifier() codexInstalledProcessPlatformVerifier {
	return newCodexInstalledDarwinProcessVerifier(codexInstalledDarwinProcessVerifierDependencies{})
}

func newCodexInstalledDarwinProcessVerifier(dependencies codexInstalledDarwinProcessVerifierDependencies) *codexInstalledDarwinProcessVerifier {
	if dependencies.pid == nil {
		dependencies.pid = os.Getpid
	}
	if dependencies.uid == nil {
		dependencies.uid = os.Geteuid
	}
	if dependencies.executablePath == nil {
		dependencies.executablePath = os.Executable
	}
	if dependencies.launchctlPrint == nil {
		dependencies.launchctlPrint = runCodexInstalledLaunchctlPrint
	}
	if dependencies.captureExecutable == nil {
		dependencies.captureExecutable = captureCodexInstalledExecutable
	}
	if dependencies.verifyMappedExecutable == nil {
		dependencies.verifyMappedExecutable = verifyCodexInstalledDarwinMappedExecutable
	}
	if dependencies.captureServiceConfiguration == nil {
		dependencies.captureServiceConfiguration = captureCodexInstalledServiceConfiguration
	}
	return &codexInstalledDarwinProcessVerifier{dependencies: dependencies}
}

func (verifier *codexInstalledDarwinProcessVerifier) Capture(ctx context.Context) (codexInstalledProcessPlatformProof, error) {
	if ctx == nil || ctx.Err() != nil || verifier == nil {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	dependencies := verifier.dependencies
	if dependencies.pid == nil || dependencies.uid == nil || dependencies.executablePath == nil || dependencies.launchctlPrint == nil ||
		dependencies.captureExecutable == nil || dependencies.verifyMappedExecutable == nil || dependencies.captureServiceConfiguration == nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	pid, uid := dependencies.pid(), dependencies.uid()
	if pid <= 1 || uid < 0 {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	executablePath, err := dependencies.executablePath()
	if err != nil {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	executable, err := dependencies.captureExecutable(executablePath)
	if err != nil || !executable.valid() {
		return codexInstalledProcessPlatformProof{}, codexInstalledAttestationError(ctx)
	}
	if err := dependencies.verifyMappedExecutable(pid, executable); err != nil {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}

	type serviceCandidate struct {
		label string
		kind  codexInstalledListenerServiceKind
	}
	candidates := []serviceCandidate{
		{label: codexInstalledLaunchdServiceLabel, kind: codexInstalledListenerServiceLaunchd},
		{label: codexInstalledHomebrewServiceLabel, kind: codexInstalledListenerServiceHomebrew},
		{label: codexInstalledCandidateServiceLabel, kind: codexInstalledListenerServiceLaunchd},
	}
	matches := make([]codexInstalledProcessPlatformProof, 0, 1)
	for _, candidate := range candidates {
		target := "gui/" + strconv.Itoa(uid) + "/" + candidate.label
		output, err := dependencies.launchctlPrint(ctx, target)
		if err != nil {
			if ctx.Err() != nil {
				return codexInstalledProcessPlatformProof{}, ctx.Err()
			}
			continue
		}
		job, err := parseCodexInstalledDarwinLaunchctlJob(output, target, pid)
		if err != nil {
			continue
		}
		configurationProof, configurationData, err := dependencies.captureServiceConfiguration(job.path)
		if err != nil {
			continue
		}
		configuration, err := parseCodexInstalledDarwinServiceConfiguration(configurationData)
		if err != nil || configuration.label != candidate.label || !configuration.keepAlive || !job.keepAlive ||
			!validCodexInstalledServiceArguments(candidate.label, job.program, job.arguments) ||
			!equalCodexInstalledStrings(configuration.programArguments, job.arguments) ||
			filepath.Base(job.path) != candidate.label+".plist" {
			continue
		}
		serviceExecutable, err := dependencies.captureExecutable(job.program)
		if err != nil || serviceExecutable != executable {
			continue
		}
		if err := dependencies.verifyMappedExecutable(pid, executable); err != nil {
			continue
		}
		currentExecutable, err := dependencies.captureExecutable(executablePath)
		if err != nil || currentExecutable != executable {
			continue
		}
		serviceIdentity := codexInstalledDarwinServiceIdentity(
			target, candidate, job, configurationProof, executable,
		)
		if serviceIdentity == ([sha256.Size]byte{}) {
			continue
		}
		matches = append(matches, codexInstalledProcessPlatformProof{
			pid:                   pid,
			serviceKind:           candidate.kind,
			persistent:            true,
			executable:            executable,
			serviceIdentitySHA256: serviceIdentity,
		})
	}
	if len(matches) != 1 || !matches[0].valid() {
		return codexInstalledProcessPlatformProof{}, errCodexInstalledProcessAttestation
	}
	return matches[0], nil
}

func validCodexInstalledServiceArguments(label, program string, arguments []string) bool {
	if len(arguments) < 3 || arguments[0] != program || arguments[1] != "proxy" || arguments[2] != "start" {
		return false
	}
	if label != codexInstalledCandidateServiceLabel {
		return len(arguments) == 3
	}
	if len(arguments) != 5 || arguments[3] != "--port" {
		return false
	}
	port, err := strconv.Atoi(arguments[4])
	return err == nil && port > 0 && port <= 65535 && port != DefaultPort
}

func parseCodexInstalledDarwinLaunchctlJob(output []byte, target string, expectedPID int) (codexInstalledDarwinLaunchctlJob, error) {
	var job codexInstalledDarwinLaunchctlJob
	if len(output) == 0 || len(output) > codexInstalledLaunchctlOutputMaxBytes || target == "" || expectedPID <= 1 ||
		strings.IndexByte(string(output), 0) >= 0 {
		return job, errCodexInstalledProcessAttestation
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != target+" = {" {
		return job, errCodexInstalledProcessAttestation
	}
	job.target = target
	seen := make(map[string]bool)
	depth := 1
	closed := false
	for index := 1; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if closed {
			return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
		}
		if depth == 1 && line == "arguments = {" {
			if seen["arguments"] {
				return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
			}
			seen["arguments"] = true
			for index++; index < len(lines); index++ {
				argument := strings.TrimSpace(lines[index])
				if argument == "}" {
					break
				}
				if argument == "" || strings.ContainsAny(argument, "{}\x00") || len(job.arguments) >= 8 {
					return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
				}
				job.arguments = append(job.arguments, argument)
			}
			if index >= len(lines) {
				return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
			}
			continue
		}
		if line == "}" {
			depth--
			if depth < 0 {
				return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
			}
			if depth == 0 {
				closed = true
			}
			continue
		}
		if strings.HasSuffix(line, "= {") {
			depth++
			continue
		}
		if depth != 1 {
			continue
		}
		key, value, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch key {
		case "path", "type", "state", "program", "pid", "active count", "properties":
			if seen[key] || value == "" {
				return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
			}
			seen[key] = true
		default:
			continue
		}
		switch key {
		case "path":
			job.path = value
		case "type":
			job.jobType = value
		case "state":
			job.state = value
		case "program":
			job.program = value
		case "pid":
			job.pid, _ = strconv.Atoi(value)
		case "active count":
			job.activeCount, _ = strconv.Atoi(value)
		case "properties":
			for _, property := range strings.Split(value, " | ") {
				if property == "" || strings.TrimSpace(property) != property || strings.ContainsAny(property, "{}\x00") || len(job.properties) >= 16 {
					return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
				}
				for _, existing := range job.properties {
					if existing == property {
						return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
					}
				}
				job.properties = append(job.properties, property)
				if property == "keepalive" {
					job.keepAlive = true
				}
			}
		}
	}
	if !closed || depth != 0 || !filepath.IsAbs(job.path) || !filepath.IsAbs(job.program) || job.jobType != "LaunchAgent" ||
		job.state != "running" || job.pid != expectedPID || job.activeCount <= 0 || len(job.arguments) == 0 || !seen["properties"] {
		return codexInstalledDarwinLaunchctlJob{}, errCodexInstalledProcessAttestation
	}
	return job, nil
}

func parseCodexInstalledDarwinServiceConfiguration(data []byte) (codexInstalledDarwinServiceConfiguration, error) {
	var configuration codexInstalledDarwinServiceConfiguration
	if len(data) == 0 || len(data) > codexInstalledServicePlistMaxBytes {
		return configuration, errCodexInstalledProcessAttestation
	}
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	first, err := nextCodexInstalledPlistToken(decoder)
	if err != nil {
		return configuration, errCodexInstalledProcessAttestation
	}
	plist, ok := first.(xml.StartElement)
	if !ok || plist.Name.Local != "plist" {
		return configuration, errCodexInstalledProcessAttestation
	}
	next, err := nextCodexInstalledPlistToken(decoder)
	if err != nil {
		return configuration, errCodexInstalledProcessAttestation
	}
	dictionary, ok := next.(xml.StartElement)
	if !ok || dictionary.Name.Local != "dict" {
		return configuration, errCodexInstalledProcessAttestation
	}
	seen := make(map[string]bool)
	for {
		token, err := nextCodexInstalledPlistToken(decoder)
		if err != nil {
			return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name.Local != "dict" {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			break
		}
		keyElement, ok := token.(xml.StartElement)
		if !ok || keyElement.Name.Local != "key" {
			return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
		}
		var key string
		if err := decoder.DecodeElement(&key, &keyElement); err != nil || key == "" {
			return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
		}
		valueToken, err := nextCodexInstalledPlistToken(decoder)
		if err != nil {
			return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
		}
		value, ok := valueToken.(xml.StartElement)
		if !ok {
			return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
		}
		switch key {
		case "Label":
			if seen[key] || value.Name.Local != "string" || decoder.DecodeElement(&configuration.label, &value) != nil {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			seen[key] = true
		case "ProgramArguments":
			if seen[key] || value.Name.Local != "array" {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			arguments, err := decodeCodexInstalledPlistStringArray(decoder, value)
			if err != nil {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			configuration.programArguments = arguments
			seen[key] = true
		case "KeepAlive":
			if seen[key] || (value.Name.Local != "true" && value.Name.Local != "false") {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			configuration.keepAlive = value.Name.Local == "true"
			if err := decoder.Skip(); err != nil {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
			seen[key] = true
		default:
			if err := decoder.Skip(); err != nil {
				return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
			}
		}
	}
	closing, err := nextCodexInstalledPlistToken(decoder)
	if err != nil {
		return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
	}
	endPlist, ok := closing.(xml.EndElement)
	if !ok || endPlist.Name.Local != "plist" {
		return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
	}
	if _, err := nextCodexInstalledPlistToken(decoder); err != io.EOF || !seen["Label"] || !seen["ProgramArguments"] || !seen["KeepAlive"] {
		return codexInstalledDarwinServiceConfiguration{}, errCodexInstalledProcessAttestation
	}
	return configuration, nil
}

func decodeCodexInstalledPlistStringArray(decoder *xml.Decoder, array xml.StartElement) ([]string, error) {
	arguments := make([]string, 0, 3)
	for {
		token, err := nextCodexInstalledPlistToken(decoder)
		if err != nil {
			return nil, errCodexInstalledProcessAttestation
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name.Local != array.Name.Local {
				return nil, errCodexInstalledProcessAttestation
			}
			return arguments, nil
		}
		value, ok := token.(xml.StartElement)
		if !ok || value.Name.Local != "string" || len(arguments) >= 8 {
			return nil, errCodexInstalledProcessAttestation
		}
		var argument string
		if err := decoder.DecodeElement(&argument, &value); err != nil || argument == "" {
			return nil, errCodexInstalledProcessAttestation
		}
		arguments = append(arguments, argument)
	}
}

func nextCodexInstalledPlistToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if characters, ok := token.(xml.CharData); ok && strings.TrimSpace(string(characters)) == "" {
			continue
		}
		switch token.(type) {
		case xml.Comment, xml.Directive, xml.ProcInst:
			continue
		default:
			return token, nil
		}
	}
}

func captureCodexInstalledServiceConfiguration(path string) (codexInstalledServiceConfigurationProof, []byte, error) {
	return captureCodexInstalledServiceConfigurationWithResolver(path, filepath.EvalSymlinks)
}

func captureCodexInstalledServiceConfigurationWithResolver(path string, resolve func(string) (string, error)) (codexInstalledServiceConfigurationProof, []byte, error) {
	var empty codexInstalledServiceConfigurationProof
	if !filepath.IsAbs(path) {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	original := filepath.Clean(path)
	resolved, err := resolve(original)
	if err != nil {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	resolved = filepath.Clean(resolved)
	inspector := fsutil.OSFileSystem{}
	opened, err := inspector.OpenNoFollow(resolved)
	if err != nil {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	defer opened.Close()
	before, err := opened.Stat()
	if err != nil {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	owner, ownerOK := inspector.FileOwnerUID(before)
	if !ownerOK || (owner != 0 && owner != inspector.EffectiveUID()) || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o022 != 0 || before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		before.Size() <= 0 || before.Size() > codexInstalledServicePlistMaxBytes {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	identity, ok := inspector.FileIdentity(before)
	if !ok || identity.Device == 0 || identity.Inode == 0 || identity.Links != 1 {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	data, err := io.ReadAll(io.LimitReader(opened, codexInstalledServicePlistMaxBytes+1))
	if err != nil || int64(len(data)) != before.Size() || len(data) > codexInstalledServicePlistMaxBytes {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	after, err := opened.Stat()
	if err != nil {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	afterIdentity, ok := inspector.FileIdentity(after)
	if !ok || afterIdentity != identity || after.Size() != before.Size() || after.Mode() != before.Mode() {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	pathInfo, err := inspector.Lstat(resolved)
	if err != nil {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	pathIdentity, ok := inspector.FileIdentity(pathInfo)
	if !ok || pathIdentity != identity || pathInfo.Size() != before.Size() || pathInfo.Mode() != before.Mode() {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	finalResolved, err := resolve(original)
	if err != nil || filepath.Clean(finalResolved) != resolved {
		return empty, nil, errCodexInstalledProcessAttestation
	}
	return codexInstalledServiceConfigurationProof{
		path:   resolved,
		device: identity.Device,
		inode:  identity.Inode,
		links:  identity.Links,
		owner:  owner,
		size:   before.Size(),
		mode:   before.Mode(),
		sha256: sha256.Sum256(data),
	}, data, nil
}

func codexInstalledDarwinServiceIdentity(
	target string,
	candidate struct {
		label string
		kind  codexInstalledListenerServiceKind
	},
	job codexInstalledDarwinLaunchctlJob,
	configuration codexInstalledServiceConfigurationProof,
	executable codexInstalledExecutableProof,
) [sha256.Size]byte {
	destination := sha256.New()
	writeCodexInstalledProcessBindingField(destination, []byte("cq-codex-installed-darwin-service-v1"))
	writeCodexInstalledProcessBindingField(destination, []byte(target))
	writeCodexInstalledProcessBindingField(destination, []byte(candidate.label))
	writeCodexInstalledProcessBindingField(destination, []byte(candidate.kind))
	writeCodexInstalledProcessBindingField(destination, []byte(job.path))
	writeCodexInstalledProcessBindingField(destination, []byte(job.program))
	for _, argument := range job.arguments {
		writeCodexInstalledProcessBindingField(destination, []byte(argument))
	}
	for _, property := range job.properties {
		writeCodexInstalledProcessBindingField(destination, []byte(property))
	}
	writeCodexInstalledProcessBindingField(destination, []byte(configuration.path))
	writeCodexInstalledProcessBindingUint64(destination, configuration.device)
	writeCodexInstalledProcessBindingUint64(destination, configuration.inode)
	writeCodexInstalledProcessBindingUint64(destination, configuration.links)
	writeCodexInstalledProcessBindingUint64(destination, configuration.owner)
	writeCodexInstalledProcessBindingUint64(destination, uint64(configuration.size))
	writeCodexInstalledProcessBindingUint64(destination, uint64(configuration.mode))
	writeCodexInstalledProcessBindingField(destination, configuration.sha256[:])
	writeCodexInstalledProcessBindingField(destination, executable.sha256[:])
	var digest [sha256.Size]byte
	copy(digest[:], destination.Sum(nil))
	return digest
}

func writeCodexInstalledProcessBindingUint64(destination io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = destination.Write(encoded[:])
}

func equalCodexInstalledStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

const codexInstalledDarwinRegionInfoSize = 1272

type codexInstalledDarwinMappedRegion struct {
	protection uint32
	offset     uint64
	address    uint64
	size       uint64
	device     uint64
	mode       uint16
	links      uint64
	inode      uint64
	owner      uint64
	fileSize   int64
	vnodeType  int32
}

//go:noinline
func codexInstalledDarwinExecutableAnchor() {}

func verifyCodexInstalledDarwinMappedExecutable(pid int, proof codexInstalledExecutableProof) error {
	pc := reflect.ValueOf(codexInstalledDarwinExecutableAnchor).Pointer()
	region, err := captureCodexInstalledDarwinMappedRegion(pid, uint64(pc))
	if err != nil || pc == 0 || uint64(pc) < region.address || uint64(pc)-region.address >= region.size ||
		region.protection&codexInstalledDarwinVMProtectionExecute == 0 || !region.matches(proof) {
		return errCodexInstalledProcessAttestation
	}
	return nil
}

func verifyCodexInstalledDarwinMainExecutable(pid int, proof codexInstalledExecutableProof) error {
	if pid <= 1 || !proof.valid() {
		return errCodexInstalledProcessAttestation
	}
	var address uint64
	for range codexInstalledDarwinMaxRegions {
		region, err := captureCodexInstalledDarwinMappedRegion(pid, address)
		if err != nil || region.address < address || region.size == 0 || region.address > ^uint64(0)-region.size {
			return errCodexInstalledProcessAttestation
		}
		if region.protection&codexInstalledDarwinVMProtectionExecute != 0 && region.offset == 0 && region.vnodeType == 1 {
			if !region.matches(proof) {
				return errCodexInstalledProcessAttestation
			}
			return nil
		}
		address = region.address + region.size
	}
	return errCodexInstalledProcessAttestation
}

func captureCodexInstalledDarwinMappedRegion(pid int, address uint64) (codexInstalledDarwinMappedRegion, error) {
	var raw [codexInstalledDarwinRegionInfoSize]byte
	written, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		codexInstalledDarwinProcInfoCallPIDInfo,
		uintptr(pid),
		codexInstalledDarwinPIDRegionPathInfo,
		uintptr(address),
		uintptr(unsafe.Pointer(&raw[0])),
		uintptr(len(raw)),
	)
	runtime.KeepAlive(&raw)
	if errno != 0 || written != uintptr(len(raw)) {
		return codexInstalledDarwinMappedRegion{}, errCodexInstalledProcessAttestation
	}
	order := binary.LittleEndian
	region := codexInstalledDarwinMappedRegion{
		protection: order.Uint32(raw[0:4]),
		offset:     order.Uint64(raw[16:24]),
		address:    order.Uint64(raw[80:88]),
		size:       order.Uint64(raw[88:96]),
		device:     uint64(order.Uint32(raw[96:100])),
		mode:       order.Uint16(raw[100:102]),
		links:      uint64(order.Uint16(raw[102:104])),
		inode:      order.Uint64(raw[104:112]),
		owner:      uint64(order.Uint32(raw[112:116])),
		fileSize:   int64(order.Uint64(raw[184:192])),
		vnodeType:  int32(order.Uint32(raw[232:236])),
	}
	return region, nil
}

func (region codexInstalledDarwinMappedRegion) matches(proof codexInstalledExecutableProof) bool {
	return proof.valid() && region.vnodeType == 1 && region.device == proof.device && region.inode == proof.inode &&
		region.links == proof.links && region.owner == proof.owner && region.fileSize == proof.size &&
		os.FileMode(region.mode&0o777) == proof.mode.Perm()
}

func runCodexInstalledVersionCommand(
	ctx context.Context,
	path string,
	expected codexInstalledExecutableProof,
) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || path != expected.path || !expected.valid() || !filepath.IsAbs(path) {
		return nil, codexInstalledAttestationError(ctx)
	}
	commandCtx, cancel := context.WithTimeout(ctx, codexInstalledProcessProofTimeout)
	defer cancel()
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	root, err := os.MkdirTemp(shortTempRoot, codexInstalledHTTPClientTempPrefix)
	if err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	defer func() { _ = removeCodexInstalledHTTPClientTempRoot(root) }()
	output, err := (osCodexAcceptanceRunner{}).Run(commandCtx, codexAcceptanceCommand{
		executable:         path,
		expectedExecutable: expected,
		args:               []string{"--version"},
		env:                codexAcceptanceBaseEnvironment("", "", "", "", ""),
		sandboxWriteRoot:   root,
		captureOutput:      true,
		loopbackOnly:       true,
	})
	if err != nil || len(output) == 0 || len(output) > codexInstalledVersionOutputMaxBytes {
		clearBytes(output)
		return nil, codexInstalledAttestationError(ctx)
	}
	return output, nil
}

func runCodexInstalledLaunchctlPrint(ctx context.Context, target string) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || !strings.HasPrefix(target, "gui/") || strings.Count(target, "/") != 2 {
		return nil, codexInstalledAttestationError(ctx)
	}
	commandCtx, cancel := context.WithTimeout(ctx, codexInstalledProcessProofTimeout)
	defer cancel()
	output := &codexInstalledBoundedBuffer{limit: codexInstalledLaunchctlOutputMaxBytes}
	command := exec.CommandContext(commandCtx, "/bin/launchctl", "print", target)
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, codexInstalledAttestationError(ctx)
	}
	return append([]byte(nil), output.data...), nil
}
