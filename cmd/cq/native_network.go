package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	nativeNetworkApplication = "/Applications/ChatGPT.app"
	nativeNetworkHostBundle  = "/Applications/CQNetwork.app"
	nativeNetworkExecutable  = nativeNetworkHostBundle + "/Contents/MacOS/CQNetwork"
	nativeNetworkIdentifier  = "dev.jacobcx.cq.network"
	nativeNetworkTeam        = "K2648T24P4"
)

// CodexNetworkCmd manages the opt-in native app network extension.
type CodexNetworkCmd struct {
	Enable  struct{} `cmd:"" help:"Route native ChatGPT traffic through configured WireGuard"`
	Status  struct{} `cmd:"" help:"Show native ChatGPT network readiness"`
	Disable struct{} `cmd:"" help:"Disable CQ native ChatGPT network routing"`
}

type nativeNetworkStatus struct {
	Ready              bool    `json:"ready"`
	ExtensionConnected bool    `json:"extension_connected"`
	BrokerRegistered   bool    `json:"broker_registered"`
	BrokerReady        bool    `json:"broker_ready"`
	TCPFlows           *uint64 `json:"tcp_flows,omitempty"`
	UDPFlows           *uint64 `json:"udp_flows,omitempty"`
	BrokerOpens        *uint64 `json:"broker_opens,omitempty"`
	BrokerFailures     *uint64 `json:"broker_failures,omitempty"`
}

type nativeNetworkRunner struct {
	validateBroker func(context.Context) error
	verifyHost     func(context.Context) error
	installAgent   func(context.Context) error
	removeAgent    func(context.Context) error
	host           func(context.Context, ...string) ([]byte, error)
	wait           func(context.Context, time.Duration) error
}

func (runner nativeNetworkRunner) execute(ctx context.Context, operation string, jsonOutput bool, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if operation != "enable" && operation != "status" && operation != "disable" {
		return fmt.Errorf("unknown native network operation: %s", operation)
	}
	if operation == "enable" {
		if err := runner.validateBroker(ctx); err != nil {
			return err
		}
	}
	if err := runner.verifyHost(ctx); err != nil {
		return err
	}
	if operation == "disable" {
		if _, err := runner.host(ctx, "disable"); err != nil {
			return fmt.Errorf("disable native network routing: %w", err)
		}
		if err := runner.removeAgent(ctx); err != nil {
			return err
		}
		if jsonOutput {
			_, err := fmt.Fprintln(output, `{"enabled":false}`)
			return err
		}
		_, err := fmt.Fprintln(output, "Native ChatGPT network routing disabled.")
		return err
	}
	if operation == "enable" {
		if err := runner.installAgent(ctx); err != nil {
			return err
		}
		if _, err := runner.host(ctx, "activate", "--application", nativeNetworkApplication); err != nil {
			return fmt.Errorf("activate native network routing (macOS approval may be required): %w", err)
		}
	}
	status, err := runner.status(ctx)
	if operation == "enable" {
		// The extension and persistent user broker connect asynchronously. Never
		// treat an activation request as proof that routing is ready.
		// A replacement can take several seconds to start on macOS.
		for attempt := 0; err != nil && attempt < 30; attempt++ {
			if waitErr := runner.wait(ctx, 500*time.Millisecond); waitErr != nil {
				break
			}
			status, err = runner.status(ctx)
		}
	}
	if jsonOutput {
		if encodeErr := json.NewEncoder(output).Encode(status); encodeErr != nil {
			return encodeErr
		}
	} else if err == nil {
		if _, writeErr := fmt.Fprintln(output, "Native ChatGPT network routing ready."); writeErr != nil {
			return writeErr
		}
	} else {
		if _, writeErr := fmt.Fprintf(output, "Native ChatGPT network routing is not ready (extension=%t, registered=%t, broker=%t).\n", status.ExtensionConnected, status.BrokerRegistered, status.BrokerReady); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func (runner nativeNetworkRunner) status(ctx context.Context) (nativeNetworkStatus, error) {
	data, commandErr := runner.host(ctx, "status")
	var status nativeNetworkStatus
	if len(data) > 16<<10 {
		return status, errors.New("native network status exceeded size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&status); err != nil {
		return status, errors.New("native network status unavailable; check CQ Network installation")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nativeNetworkStatus{}, errors.New("invalid native network status response")
	}
	if commandErr != nil || !status.Ready || !status.ExtensionConnected || !status.BrokerRegistered || !status.BrokerReady {
		status.Ready = false
		return status, errors.New("native network routing is not ready; approve CQ Network in System Settings if requested, then run cq codex network enable")
	}
	return status, nil
}

func waitNativeNetwork(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
