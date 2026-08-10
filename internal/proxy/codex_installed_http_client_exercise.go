package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const codexInstalledHTTPClientTempPrefix = "cq-codex-installed-client-"

type codexInstalledHTTPClientExercise struct {
	mu sync.Mutex

	address    string
	executable codexInstalledExecutableProof
	localToken string
	runner     codexAcceptanceRunner
	outcome    *codexInstalledHTTPClientOutcome
	running    bool
	ran        bool
}

func newCodexInstalledHTTPClientExercise(
	address string,
	executable codexInstalledExecutableProof,
	localToken string,
	runner codexAcceptanceRunner,
	outcome *codexInstalledHTTPClientOutcome,
) (*codexInstalledHTTPClientExercise, error) {
	target, err := validateCodexInstalledHTTPValidationLoopbackAddress(address)
	if err != nil || !executable.valid() || !validCodexInstalledHTTPValidationToken(localToken) || runner == nil || outcome == nil {
		return nil, errCodexInstalledListenerAcceptance
	}
	return &codexInstalledHTTPClientExercise{
		address:    target,
		executable: executable,
		localToken: localToken,
		runner:     runner,
		outcome:    outcome,
	}, nil
}

func (exercise *codexInstalledHTTPClientExercise) Run(ctx context.Context) (returnErr error) {
	if ctx == nil || ctx.Err() != nil || exercise == nil {
		return errCodexInstalledListenerAcceptance
	}
	exercise.mu.Lock()
	if exercise.running || exercise.ran {
		exercise.mu.Unlock()
		return errCodexInstalledListenerAcceptance
	}
	exercise.running = true
	exercise.mu.Unlock()
	defer func() {
		exercise.mu.Lock()
		exercise.running = false
		exercise.ran = true
		exercise.mu.Unlock()
		if recover() != nil {
			returnErr = errCodexInstalledListenerAcceptance
		}
	}()

	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return errCodexInstalledListenerAcceptance
	}
	root, err := os.MkdirTemp(shortTempRoot, codexInstalledHTTPClientTempPrefix)
	if err != nil {
		return errCodexInstalledListenerAcceptance
	}
	defer func() {
		returnErr = errors.Join(returnErr, removeCodexInstalledHTTPClientTempRoot(root))
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return errCodexInstalledListenerAcceptance
	}
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex-home")
	work := filepath.Join(root, "work")
	tmp := filepath.Join(root, "tmp")
	cache := filepath.Join(root, "cache")
	config := filepath.Join(root, "config")
	data := filepath.Join(root, "data")
	for _, directory := range []string{home, codexHome, work, tmp, cache, config, data} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return errCodexInstalledListenerAcceptance
		}
	}
	if err := writeCodexAcceptanceAuthWithToken(filepath.Join(codexHome, "auth.json"), exercise.localToken); err != nil {
		return errCodexInstalledListenerAcceptance
	}

	egressListener, egressServer, egressErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		exercise.outcome.egressAttempts.Add(1)
		http.Error(writer, "installed validation egress denied", http.StatusBadGateway)
	}))
	if err != nil {
		return errCodexInstalledListenerAcceptance
	}
	egressClosed := false
	defer func() {
		if !egressClosed {
			shutdownCodexAcceptanceServer(egressServer)
		}
	}()

	baseURL := "http://" + exercise.address
	egressURL := "http://" + egressListener.Addr().String()
	outputPath := filepath.Join(root, "last-message.txt")
	environment := codexAcceptanceBaseEnvironment(home, codexHome, tmp, cache, config)
	environment = append(environment,
		"XDG_DATA_HOME="+data,
		"HTTP_PROXY="+egressURL,
		"HTTPS_PROXY="+egressURL,
		"ALL_PROXY="+egressURL,
		"http_proxy="+egressURL,
		"https_proxy="+egressURL,
		"all_proxy="+egressURL,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	command := codexAcceptanceCommand{
		executable:         exercise.executable.path,
		expectedExecutable: exercise.executable,
		args:               codexAcceptanceExecArguments(baseURL, work, outputPath),
		env:                environment,
		dir:                work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		egressProxyURL:     egressURL,
		sandboxWriteRoot:   root,
		loopbackOnly:       true,
	}
	execCtx, cancelExec := context.WithTimeout(ctx, codexAcceptanceExecTimeout)
	_, err = exercise.runner.Run(execCtx, command)
	cancelExec()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errCodexInstalledListenerAcceptance
	}
	output, err := readCodexAcceptanceOutput(outputPath)
	if err != nil {
		return errCodexInstalledListenerAcceptance
	}
	exactPong := bytes.Equal(output, []byte("PONG")) || bytes.Equal(output, []byte("PONG\n"))
	clearBytes(output)
	if !exactPong || exercise.outcome.egressAttempts.Load() != 0 {
		return errCodexInstalledListenerAcceptance
	}
	exercise.outcome.exactPong.Store(true)
	shutdownCodexAcceptanceServer(egressServer)
	egressClosed = true
	if err := codexAcceptanceServeError(egressErrors); err != nil {
		return errCodexInstalledListenerAcceptance
	}
	return nil
}

func validCodexInstalledHTTPValidationToken(token string) bool {
	if token == codexAcceptanceLocalToken || token != strings.TrimSpace(token) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	valid := err == nil && len(decoded) == 32
	clearBytes(decoded)
	return valid
}

func removeCodexInstalledHTTPClientTempRoot(root string) error {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) == clean || filepath.Dir(clean) == string(filepath.Separator) ||
		!strings.HasPrefix(filepath.Base(clean), codexInstalledHTTPClientTempPrefix) {
		return errCodexInstalledListenerAcceptance
	}
	if err := os.RemoveAll(clean); err != nil {
		return errCodexInstalledListenerAcceptance
	}
	return nil
}

type codexInstalledHTTPCompositeExercise struct {
	first  codexInstalledHTTPExercise
	second codexInstalledHTTPExercise
}

func (exercise *codexInstalledHTTPCompositeExercise) Run(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || exercise == nil || exercise.first == nil || exercise.second == nil {
		return errCodexInstalledListenerAcceptance
	}
	if err := exercise.first.Run(ctx); err != nil {
		return err
	}
	return exercise.second.Run(ctx)
}
