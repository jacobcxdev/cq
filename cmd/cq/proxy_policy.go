package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const proxyPolicyMaxBytes = 1 << 20

var proxyPolicyNow = time.Now

type proxyPolicyOptions struct {
	StateRoot string
	File      string
}

func runProxyPolicy(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: cq proxy policy <initialise|apply|status>")
	}
	command := args[0]
	options, err := parseProxyPolicyOptions(args[1:])
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("proxy policy output unavailable")
	}
	switch command {
	case "initialise":
		if options.StateRoot == "" || options.File != "" {
			return errors.New("usage: cq proxy policy initialise --state-root DIR")
		}
		if err := proxy.InitialiseProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: options.StateRoot, Random: rand.Reader, Now: proxyPolicyNow}); err != nil {
			return err
		}
		cfg, err := proxy.LoadConfig()
		if err != nil {
			return err
		}
		cfg.ProxyResilienceStateDir = options.StateRoot
		if err := proxy.SaveConfig(cfg); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "proxy resilience state initialised: %s\n", options.StateRoot)
		return err
	case "apply":
		if options.File == "" {
			return errors.New("usage: cq proxy policy apply --file FILE [--state-root DIR]")
		}
		root, err := resolveProxyPolicyRoot(options.StateRoot)
		if err != nil {
			return err
		}
		policy, err := readProxyRoutingPolicy(options.File)
		if err != nil {
			return err
		}
		state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow})
		if err != nil {
			return err
		}
		defer state.Close()
		if err := state.Routing.Publish(policy); err != nil {
			return err
		}
		return writeProxyPolicyJSON(output, state.Routing.Current())
	case "status":
		if options.File != "" {
			return errors.New("usage: cq proxy policy status [--state-root DIR]")
		}
		root, err := resolveProxyPolicyRoot(options.StateRoot)
		if err != nil {
			return err
		}
		state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow})
		if err != nil {
			return err
		}
		defer state.Close()
		return writeProxyPolicyJSON(output, state.Routing.Current())
	default:
		return fmt.Errorf("unknown proxy policy command: %s", command)
	}
}

func parseProxyPolicyOptions(args []string) (proxyPolicyOptions, error) {
	var options proxyPolicyOptions
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return options, fmt.Errorf("proxy policy: %s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--state-root":
			if options.StateRoot != "" {
				return options, errors.New("proxy policy: duplicate --state-root")
			}
			clean := filepath.Clean(value)
			if !filepath.IsAbs(value) || clean != value || clean == string(filepath.Separator) {
				return options, errors.New("proxy policy: state root must be a clean absolute non-root path")
			}
			options.StateRoot = value
		case "--file":
			if options.File != "" {
				return options, errors.New("proxy policy: duplicate --file")
			}
			options.File = value
		default:
			return options, fmt.Errorf("proxy policy: unknown option %s", args[index])
		}
		index++
	}
	return options, nil
}

func resolveProxyPolicyRoot(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.ProxyResilienceStateDir == "" {
		return "", errors.New("proxy resilience state is not configured")
	}
	return cfg.ProxyResilienceStateDir, nil
}

func readProxyRoutingPolicy(path string) (proxy.RoutingPolicyV1, error) {
	file, err := os.Open(path)
	if err != nil {
		return proxy.RoutingPolicyV1{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, proxyPolicyMaxBytes+1))
	if err != nil {
		return proxy.RoutingPolicyV1{}, err
	}
	if len(body) > proxyPolicyMaxBytes {
		return proxy.RoutingPolicyV1{}, errors.New("proxy policy exceeds 1 MiB")
	}
	var policy proxy.RoutingPolicyV1
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return proxy.RoutingPolicyV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return proxy.RoutingPolicyV1{}, errors.New("proxy policy contains trailing data")
	}
	return policy, nil
}

func writeProxyPolicyJSON(output io.Writer, policy proxy.RoutingPolicyV1) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(policy)
}
