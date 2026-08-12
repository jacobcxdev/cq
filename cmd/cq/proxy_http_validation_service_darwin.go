//go:build darwin

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

const (
	installedHTTPValidationPlistMaxBytes      = 1 << 20
	installedHTTPValidationExecutableMaxBytes = 256 << 20
)

type installedHTTPValidationServiceOperations struct {
	executable     func() (string, error)
	plistPath      func(string) (string, error)
	launchctlPrint func(string) error
	evalSymlinks   func(string) (string, error)
}

type installedHTTPValidationCandidateOperations struct {
	resolveService func(string) (installedHTTPValidationServiceBinding, error)
	launchctlPrint func(string) ([]byte, error)
	lsof           func(int) ([]byte, error)
	effectiveUID   func() int
}

func validateInstalledHTTPValidationCandidate(port int) (installedHTTPValidationCandidateAuthority, error) {
	return validateInstalledHTTPValidationCandidateWithOperations(port, installedHTTPValidationCandidateOperations{
		resolveService: resolveInstalledHTTPValidationService,
		launchctlPrint: func(label string) ([]byte, error) {
			target, err := installedHTTPValidationLaunchctlTarget(label, os.Geteuid)
			if err != nil {
				return nil, err
			}
			return exec.Command("launchctl", "print", target).Output()
		},
		lsof: func(port int) ([]byte, error) {
			return exec.Command("/usr/sbin/lsof", "-nP", "-a", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fp").Output()
		},
		effectiveUID: os.Geteuid,
	})
}

func validateInstalledHTTPValidationCandidateWithOperations(port int, ops installedHTTPValidationCandidateOperations) (installedHTTPValidationCandidateAuthority, error) {
	if port <= 0 || port > 65535 || ops.resolveService == nil || ops.launchctlPrint == nil || ops.lsof == nil || ops.effectiveUID == nil {
		return installedHTTPValidationCandidateAuthority{}, errors.New("incomplete installed candidate authority")
	}
	binding, err := ops.resolveService("")
	if err != nil || binding.validate() != nil {
		return installedHTTPValidationCandidateAuthority{}, errors.Join(err, binding.validate())
	}
	launchctlOutput, err := ops.launchctlPrint(binding.label)
	if err != nil {
		return installedHTTPValidationCandidateAuthority{}, err
	}
	target, err := installedHTTPValidationLaunchctlTarget(binding.label, ops.effectiveUID)
	if err != nil {
		return installedHTTPValidationCandidateAuthority{}, err
	}
	pid, err := parseInstalledHTTPValidationLaunchctlPID(launchctlOutput, target)
	if err != nil {
		return installedHTTPValidationCandidateAuthority{}, err
	}
	lsofOutput, err := ops.lsof(port)
	if err != nil {
		return installedHTTPValidationCandidateAuthority{}, err
	}
	if err := requireInstalledHTTPValidationListenerPID(lsofOutput, pid); err != nil {
		return installedHTTPValidationCandidateAuthority{}, err
	}
	return installedHTTPValidationCandidateAuthority{binding: binding, pid: pid}, nil
}

func parseInstalledHTTPValidationLaunchctlPID(output []byte, target string) (int, error) {
	if len(output) == 0 || len(output) > installedHTTPValidationPlistMaxBytes || target == "" || !strings.HasPrefix(string(output), target+" = {\n") {
		return 0, errors.New("invalid installed candidate launchd authority")
	}
	pid := 0
	seen := 0
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid = ") {
			continue
		}
		seen++
		value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
		if err != nil || value <= 1 {
			return 0, errors.New("invalid installed candidate launchd pid")
		}
		pid = value
	}
	if seen != 1 {
		return 0, errors.New("ambiguous installed candidate launchd pid")
	}
	return pid, nil
}

func requireInstalledHTTPValidationListenerPID(output []byte, expectedPID int) error {
	if len(output) == 0 || len(output) > installedHTTPValidationPlistMaxBytes || expectedPID <= 1 {
		return errors.New("installed candidate listener is unavailable")
	}
	want := "p" + strconv.Itoa(expectedPID)
	seenPID := 0
	for _, line := range strings.Fields(string(output)) {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		seenPID++
		if line != want {
			return errors.New("installed candidate listener pid mismatch")
		}
	}
	if seenPID != 1 {
		return errors.New("installed candidate listener pid mismatch")
	}
	return nil
}

func restartInstalledHTTPValidationCandidate(label string) error {
	if label != proxyAgentLabel && label != homebrewProxyAgentLabel {
		return errors.New("unsupported installed candidate service label")
	}
	target, err := installedHTTPValidationLaunchctlTarget(label, os.Geteuid)
	if err != nil {
		return err
	}
	if err := runProxyLaunchctl("kickstart", "-k", target); err != nil {
		return fmt.Errorf("launchctl kickstart candidate: %w", err)
	}
	return nil
}

func resolveInstalledHTTPValidationService(expectedLabel string) (installedHTTPValidationServiceBinding, error) {
	return resolveInstalledHTTPValidationServiceWithOperations(expectedLabel, installedHTTPValidationServiceOperations{
		executable: os.Executable,
		plistPath: func(label string) (string, error) {
			switch label {
			case proxyAgentLabel:
				return proxyAgentPlistPath()
			case homebrewProxyAgentLabel:
				home, err := os.UserHomeDir()
				if err != nil {
					return "", err
				}
				return filepath.Join(home, "Library", "LaunchAgents", homebrewProxyAgentLabel+".plist"), nil
			default:
				return "", errors.New("unsupported installed proxy service label")
			}
		},
		launchctlPrint: func(label string) error {
			target, err := installedHTTPValidationLaunchctlTarget(label, os.Geteuid)
			if err != nil {
				return err
			}
			return exec.Command("launchctl", "print", target).Run()
		},
		evalSymlinks: filepath.EvalSymlinks,
	})
}

func installedHTTPValidationLaunchctlTarget(label string, effectiveUID func() int) (string, error) {
	if effectiveUID == nil || (label != proxyAgentLabel && label != homebrewProxyAgentLabel) {
		return "", errors.New("invalid installed proxy launchctl authority")
	}
	uid := effectiveUID()
	if uid < 0 {
		return "", errors.New("invalid installed proxy effective uid")
	}
	return fmt.Sprintf("gui/%d/%s", uid, label), nil
}

func resolveInstalledHTTPValidationServiceWithOperations(expectedLabel string, ops installedHTTPValidationServiceOperations) (installedHTTPValidationServiceBinding, error) {
	if ops.executable == nil || ops.plistPath == nil || ops.launchctlPrint == nil {
		return installedHTTPValidationServiceBinding{}, errors.New("incomplete installed proxy service resolver")
	}
	if ops.evalSymlinks == nil {
		ops.evalSymlinks = filepath.EvalSymlinks
	}
	labels := []string{proxyAgentLabel, homebrewProxyAgentLabel}
	if expectedLabel != "" {
		if expectedLabel != proxyAgentLabel && expectedLabel != homebrewProxyAgentLabel {
			return installedHTTPValidationServiceBinding{}, errors.New("unsupported installed proxy service label")
		}
		labels = []string{expectedLabel}
	}
	loaded := make([]string, 0, 1)
	for _, label := range labels {
		if err := ops.launchctlPrint(label); err == nil {
			loaded = append(loaded, label)
		}
	}
	if len(loaded) != 1 {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy service identity is absent or ambiguous")
	}
	label := loaded[0]
	plistPath, err := ops.plistPath(label)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("resolve installed proxy service plist: %w", err)
	}
	plistData, plistDigest, err := readInstalledHTTPValidationRegularFile(plistPath, installedHTTPValidationPlistMaxBytes, false)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("read installed proxy service plist: %w", err)
	}
	plistLabel, arguments, err := parseInstalledHTTPValidationProxyPlist(plistData)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("parse installed proxy service plist: %w", err)
	}
	if plistLabel != label || len(arguments) != 3 || arguments[1] != "proxy" || arguments[2] != "start" {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy service is not exact cq proxy start")
	}
	currentExecutable, err := ops.executable()
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("resolve current CQ executable: %w", err)
	}
	currentExecutable, err = resolveInstalledHTTPValidationExecutablePath(currentExecutable, ops.evalSymlinks)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("resolve current CQ executable: %w", err)
	}
	serviceExecutable, err := resolveInstalledHTTPValidationExecutablePath(arguments[0], ops.evalSymlinks)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("resolve installed proxy executable: %w", err)
	}
	if !constantTimeStringEqual(currentExecutable, serviceExecutable) {
		return installedHTTPValidationServiceBinding{}, errors.New("installed proxy service executable differs from current CQ executable")
	}
	_, executableDigest, err := readInstalledHTTPValidationRegularFile(serviceExecutable, installedHTTPValidationExecutableMaxBytes, true)
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("read installed proxy executable: %w", err)
	}
	payload, err := json.Marshal(struct {
		Version          int    `json:"version"`
		Label            string `json:"label"`
		ExecutablePath   string `json:"executable_path"`
		ExecutableSHA256 string `json:"executable_sha256"`
		PlistSHA256      string `json:"plist_sha256"`
	}{
		Version:          1,
		Label:            label,
		ExecutablePath:   serviceExecutable,
		ExecutableSHA256: executableDigest,
		PlistSHA256:      plistDigest,
	})
	if err != nil {
		return installedHTTPValidationServiceBinding{}, fmt.Errorf("encode installed proxy service binding: %w", err)
	}
	serviceDigest := sha256.Sum256(payload)
	return installedHTTPValidationServiceBinding{
		label:            label,
		executableSHA256: executableDigest,
		serviceSHA256:    hex.EncodeToString(serviceDigest[:]),
	}, nil
}

func resolveInstalledHTTPValidationExecutablePath(path string, evalSymlinks func(string) (string, error)) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("executable path is not canonical and absolute")
	}
	resolved, err := evalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", errors.New("resolved executable path is not absolute")
	}
	return filepath.Clean(resolved), nil
}

func readInstalledHTTPValidationRegularFile(path string, maxBytes int64, executable bool) ([]byte, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || maxBytes <= 0 {
		return nil, "", errors.New("invalid installed service file path")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, "", errors.New("open installed service file descriptor")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	inspector := fsutil.OSFileSystem{}
	owner, ownerOK := inspector.FileOwnerUID(before)
	identity, identityOK := inspector.FileIdentity(before)
	validMode := before.Mode().IsRegular() && before.Mode().Perm()&0o022 == 0 &&
		before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
	if executable {
		validMode = validMode && before.Mode().Perm()&0o111 != 0
	}
	if !ownerOK || owner != uint64(os.Geteuid()) || !identityOK || identity.Links != 1 || !validMode || before.Size() <= 0 || before.Size() > maxBytes {
		return nil, "", errors.New("unsafe installed service file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) != before.Size() || int64(len(data)) > maxBytes {
		return nil, "", errors.New("installed service file changed while reading")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	afterIdentity, ok := inspector.FileIdentity(after)
	if !ok || afterIdentity != identity || after.Size() != before.Size() || after.Mode() != before.Mode() {
		return nil, "", errors.New("installed service file changed while reading")
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

func parseInstalledHTTPValidationProxyPlist(data []byte) (string, []string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	rootSeen := false
	dictionarySeen := false
	var label string
	var arguments []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", nil, errors.New("unterminated proxy service plist")
		}
		if err != nil {
			return "", nil, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				if typed.Name.Local != "plist" {
					return "", nil, errors.New("invalid proxy service plist root")
				}
				rootSeen = true
				continue
			}
			if dictionarySeen || typed.Name.Local != "dict" {
				return "", nil, errors.New("unexpected proxy service plist authority")
			}
			label, arguments, err = parseInstalledHTTPValidationProxyPlistDictionary(decoder)
			if err != nil {
				return "", nil, err
			}
			dictionarySeen = true
		case xml.EndElement:
			if typed.Name.Local != "plist" || !rootSeen || !dictionarySeen {
				return "", nil, errors.New("invalid proxy service plist structure")
			}
			for {
				trailing, trailingErr := decoder.Token()
				if errors.Is(trailingErr, io.EOF) {
					return label, arguments, nil
				}
				if trailingErr != nil {
					return "", nil, trailingErr
				}
				if characters, ok := trailing.(xml.CharData); !ok || strings.TrimSpace(string(characters)) != "" {
					return "", nil, errors.New("trailing proxy service plist data")
				}
			}
		case xml.CharData:
			if strings.TrimSpace(string(typed)) != "" {
				return "", nil, errors.New("invalid proxy service plist text")
			}
		case xml.ProcInst, xml.Directive, xml.Comment:
			if rootSeen {
				return "", nil, errors.New("unexpected proxy service plist metadata")
			}
		default:
			return "", nil, errors.New("invalid proxy service plist token")
		}
	}
}

func parseInstalledHTTPValidationProxyPlistDictionary(decoder *xml.Decoder) (string, []string, error) {
	seen := make(map[string]struct{})
	var label string
	var arguments []string
	for {
		token, err := nextInstalledHTTPValidationXMLToken(decoder)
		if err != nil {
			return "", nil, err
		}
		if end, ok := token.(xml.EndElement); ok && end.Name.Local == "dict" {
			if label == "" || arguments == nil {
				return "", nil, errors.New("missing proxy service plist authority fields")
			}
			return label, arguments, nil
		}
		keyStart, ok := token.(xml.StartElement)
		if !ok || keyStart.Name.Local != "key" {
			return "", nil, errors.New("invalid proxy service plist dictionary")
		}
		var key string
		if err := decoder.DecodeElement(&key, &keyStart); err != nil {
			return "", nil, err
		}
		folded := strings.ToLower(key)
		if _, duplicate := seen[folded]; duplicate {
			return "", nil, errors.New("duplicate proxy service plist field")
		}
		seen[folded] = struct{}{}
		valueToken, err := nextInstalledHTTPValidationXMLToken(decoder)
		if err != nil {
			return "", nil, err
		}
		valueStart, ok := valueToken.(xml.StartElement)
		if !ok {
			return "", nil, errors.New("invalid proxy service plist value")
		}
		switch key {
		case "Label":
			if valueStart.Name.Local != "string" {
				return "", nil, errors.New("invalid proxy service label")
			}
			if err := decoder.DecodeElement(&label, &valueStart); err != nil {
				return "", nil, err
			}
		case "ProgramArguments":
			if valueStart.Name.Local != "array" {
				return "", nil, errors.New("invalid proxy service arguments")
			}
			arguments, err = parseInstalledHTTPValidationProxyPlistArray(decoder)
			if err != nil {
				return "", nil, err
			}
		default:
			if strings.EqualFold(key, "Label") || strings.EqualFold(key, "ProgramArguments") {
				return "", nil, errors.New("non-canonical proxy service authority field")
			}
			if err := decoder.Skip(); err != nil {
				return "", nil, err
			}
		}
	}
}

func parseInstalledHTTPValidationProxyPlistArray(decoder *xml.Decoder) ([]string, error) {
	var values []string
	for {
		token, err := nextInstalledHTTPValidationXMLToken(decoder)
		if err != nil {
			return nil, err
		}
		if end, ok := token.(xml.EndElement); ok && end.Name.Local == "array" {
			return values, nil
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "string" {
			return nil, errors.New("invalid proxy service argument")
		}
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
}

func nextInstalledHTTPValidationXMLToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if characters, ok := token.(xml.CharData); ok && strings.TrimSpace(string(characters)) == "" {
			continue
		}
		return token, nil
	}
}
