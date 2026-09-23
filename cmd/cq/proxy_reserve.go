package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func runProxyReserve(args []string, output io.Writer) error {
	return runProxyReserveWithDependencies(context.Background(), args, output, proxyPolicyDependencies{
		LoadConfig: proxy.LoadExistingConfig,
		Doer:       &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("proxy reserve redirect refused") }},
	})
}

type proxyReserveOptions struct {
	action  string
	window  quota.WindowName
	percent float64
	port    int
	asJSON  bool
}

func parseProxyReserveOptions(args []string) (proxyReserveOptions, error) {
	if len(args) == 0 {
		return proxyReserveOptions{}, errors.New("usage: cq proxy reserve <set|disable|enable|clear|status|windows>")
	}
	action := args[0]
	switch action {
	case "set", "disable", "enable", "clear", "status", "windows":
	default:
		return proxyReserveOptions{}, fmt.Errorf("unknown proxy reserve command: %s", action)
	}
	var window quota.WindowName
	var percent float64
	var port int
	var asJSON bool
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return proxyReserveOptions{}, fmt.Errorf("proxy reserve: duplicate %s", flag)
		}
		seen[flag] = true
		if flag == "--json" {
			asJSON = true
			continue
		}
		if i+1 >= len(args) {
			return proxyReserveOptions{}, fmt.Errorf("proxy reserve: %s requires a value", flag)
		}
		value := args[i+1]
		i++
		switch flag {
		case "--window":
			window = quota.WindowName(value)
		case "--percent":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed >= 100 {
				return proxyReserveOptions{}, errors.New("proxy reserve: percent must be greater than 0 and less than 100")
			}
			percent = parsed
		case "--port":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 65535 {
				return proxyReserveOptions{}, errors.New("proxy reserve: invalid port")
			}
			port = parsed
		default:
			return proxyReserveOptions{}, fmt.Errorf("proxy reserve: unknown option %s", flag)
		}
	}
	if action == "set" {
		if window == "" || !seen["--percent"] {
			return proxyReserveOptions{}, errors.New("usage: cq proxy reserve set --window WINDOW --percent PERCENT")
		}
	} else if seen["--window"] || seen["--percent"] {
		return proxyReserveOptions{}, errors.New("proxy reserve: --window and --percent require set")
	}
	return proxyReserveOptions{action: action, window: window, percent: percent, port: port, asJSON: asJSON}, nil
}

func runProxyReserveWithDependencies(ctx context.Context, args []string, output io.Writer, deps proxyPolicyDependencies) error {
	options, err := parseProxyReserveOptions(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("proxy reserve control unavailable")
	}
	status, err := requestProxyReserve(ctx, options, deps)
	if err != nil {
		return err
	}
	action, asJSON := options.action, options.asJSON
	if asJSON {
		return json.NewEncoder(output).Encode(status)
	}
	if action == "windows" {
		names := make([]string, 0, len(status.Windows))
		for name := range status.Windows {
			names = append(names, string(name))
		}
		sort.Strings(names)
		if len(names) == 0 {
			_, err = fmt.Fprintln(output, "No system-account quota windows available yet.")
			return err
		}
		for _, name := range names {
			if _, err = fmt.Fprintln(output, name); err != nil {
				return err
			}
		}
		return nil
	}
	if !status.Configured {
		_, err = fmt.Fprintln(output, "System account reserve: not configured.")
		return err
	}
	state := "enabled"
	if !status.Enabled {
		state = "disabled until reset"
	}
	_, err = fmt.Fprintf(output, "System account reserve: %g%% of %s (%s)\nAccount: %s\n", status.Percent, status.Window, state, status.Email)
	if err == nil && status.Reason != "" {
		_, err = fmt.Fprintf(output, "Availability: %s\n", status.Reason)
	}
	return err
}

// ProxyReserveCmd exposes reserve commands in the CLI help tree.
type ProxyReserveCmd struct {
	Set     ProxyReserveSetCmd  `cmd:"" help:"Set the system account reserve"`
	Disable ProxyReserveReadCmd `cmd:"" help:"Release the reserve until the selected window resets"`
	Enable  ProxyReserveReadCmd `cmd:"" help:"Re-enable the reserve"`
	Clear   ProxyReserveReadCmd `cmd:"" help:"Remove the reserve configuration"`
	Status  ProxyReserveReadCmd `cmd:"" help:"Show reserve state"`
	Windows ProxyReserveReadCmd `cmd:"" help:"List available system account quota window selectors"`
}

type ProxyReserveReadCmd struct {
	Port int `help:"Proxy port"`
}

type ProxyReserveSetCmd struct {
	Window  string  `required:"" help:"Exact quota window selector from reserve windows"`
	Percent float64 `required:"" help:"Percentage of remaining quota to protect"`
	Port    int     `help:"Proxy port"`
}

// proxyReserveError retains legacy diagnostics while exposing transport failures
// independently of printed output or a second, racy status request.
type proxyReserveError struct {
	kind   string
	status int
	code   string
	err    error
}

func (e *proxyReserveError) Error() string { return e.err.Error() }
func (e *proxyReserveError) Unwrap() error { return e.err }

func requestProxyReserve(ctx context.Context, options proxyReserveOptions, deps proxyPolicyDependencies) (proxy.CodexReserveStatus, error) {
	action, window, percent, port := options.action, options.window, options.percent, options.port
	if ctx == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "unavailable", err: errors.New("proxy reserve control unavailable")}
	}
	if err := ctx.Err(); err != nil {
		return proxy.CodexReserveStatus{}, err
	}
	cfg, err := deps.LoadConfig()
	if stopped := ctx.Err(); stopped != nil {
		return proxy.CodexReserveStatus{}, stopped
	}
	if err != nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "io", err: err}
	}
	if cfg == nil || cfg.LocalToken == "" {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "auth", err: errors.New("proxy reserve: local proxy credentials unavailable")}
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	method := http.MethodGet
	var body io.Reader = http.NoBody
	if action != "status" && action != "windows" {
		method = http.MethodPost
		encoded, err := json.Marshal(proxy.CodexReserveControlRequest{Action: action, Window: window, Percent: percent})
		if err != nil {
			return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "io", err: err}
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, proxy.RuntimeReservePath), body)
	if err != nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "io", err: err}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := deps.Doer.Do(req)
	if err != nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "unavailable", err: fmt.Errorf("proxy reserve: running CQ service required: %w", err)}
	}
	if response == nil || response.Body == nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "unavailable", err: errors.New("proxy reserve response unavailable")}
	}
	defer response.Body.Close()
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "io", err: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return proxy.CodexReserveStatus{}, &proxyReserveError{status: response.StatusCode, code: response.Header.Get("X-CQ-Reserve-Error"), err: fmt.Errorf("proxy reserve control failed: HTTP %d", response.StatusCode)}
	}
	var status proxy.CodexReserveStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return proxy.CodexReserveStatus{}, &proxyReserveError{kind: "io", err: errors.New("proxy reserve response invalid")}
	}
	return status, nil
}
