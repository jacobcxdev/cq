package dots

import (
	"context"
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// These excerpts retain the reviewed renderer's owner atoms, cache classes,
// request wrapper and ingestion supplier, rather than imitating their API.
//
//go:embed testdata/native-22943.json
var nativeCurrentContractJSON string

//go:embed native_current_test.js
var nativeCurrentRegressionJS string

func TestCurrentNativeContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native JavaScript regression")
	}
	var fixture struct {
		Version string `json:"version"`
		Build   string `json:"build"`
		Archive string `json:"archive_sha256"`
		Initial string `json:"initial_asset"`
	}
	if err := json.Unmarshal([]byte(nativeCurrentContractJSON), &fixture); err != nil {
		t.Fatal(err)
	}
	var pin *Settings
	for i := range qualifiedAppPins {
		if qualifiedAppPins[i].AppBuild == fixture.Build {
			pin = &qualifiedAppPins[i]
		}
	}
	if pin == nil || pin.AppVersion != fixture.Version || pin.ASARSHA256 != fixture.Archive || pin.ModuleAsset != fixture.Initial {
		t.Fatal("current pin does not match reviewed renderer contract")
	}
	asset, bindings, err := resolveNativeBindings(nativeDiscoveryFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{IntegrationMode: "native", ModuleAsset: asset, NativeBindings: bindings, SystemAlias: "primary", Dots: map[string]Dot{"one": {OwnerAlias: "primary", ProfileID: "profile-one"}}}
	sources := map[string]string{}
	e := nativeEvaluationFunc(func(_ context.Context, expression string) ([]any, error) {
		switch {
		case strings.Contains(expression, "const bus="):
			sources["board"] = expression
			return boardValue(map[string]any{"thread": map[string]any{"id": testBoardThread}})
		case strings.Contains(expression, "Native messaging scope"):
			sources["cache"] = expression
			return []any{map[string]any{"installed": true}}, nil
		default:
			sources["executor"] = expression
			return []any{map[string]any{"ready": true, "enabled": true, "environment_id": "environment-one"}}, nil
		}
	})
	if _, err := InstallNativeRoomCache(context.Background(), s, e); err != nil {
		t.Fatal(err)
	}
	if _, err := primaryExecutorEnvironment(context.Background(), s, e); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBoard(s, nil, e).metadata(context.Background(), testBoardThread); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"fixture": json.RawMessage(nativeCurrentContractJSON), "sources": sources})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "-e", nativeCurrentRegressionJS)
	command.Stdin = strings.NewReader(string(data))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("current renderer contract: %v\n%s", err, output)
	}
}
