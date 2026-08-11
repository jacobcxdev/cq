package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func codexCanaryProtections(home, configDirectory string) ([]proxy.CodexCanaryProtection, error) {
	codexDirectory := filepath.Join(home, ".codex")
	managedDirectory := filepath.Join(codexDirectory, "accounts")
	codexBarRoot := codexprov.DefaultCodexBarRoot(home)
	codexBarSource := codexprov.NewCodexBarSource(codexBarRoot)
	if _, err := codexBarSource.ProtectionSnapshot(); err != nil && !errors.Is(err, codexprov.ErrExternalUnavailable) {
		return nil, codexCanaryProtectionSourceError(err)
	}

	return []proxy.CodexCanaryProtection{
		proxy.CodexCanaryFileProtection(proxy.CodexCanarySystemAuth, filepath.Join(codexDirectory, "auth.json")),
		proxy.CodexCanaryFileProtection(proxy.CodexCanaryRegistry, filepath.Join(managedDirectory, "registry.json")),
		proxy.CodexCanaryDirectoryProtection(proxy.CodexCanaryCQManagedAuth, managedDirectory, ".auth.json"),
		proxy.CodexCanaryOptionalSnapshotProtection(proxy.CodexCanaryCodexBarManifest, func() ([]byte, error) {
			snapshot, err := codexBarCanaryProtectionSnapshot(codexBarSource)
			if err != nil {
				return nil, err
			}
			return append([]byte(nil), snapshot.ManifestDigest[:]...), nil
		}),
		proxy.CodexCanaryOptionalSnapshotProtection(proxy.CodexCanaryCodexBarAuth, func() ([]byte, error) {
			snapshot, err := codexBarCanaryProtectionSnapshot(codexBarSource)
			if err != nil {
				return nil, err
			}
			result := make([]byte, 8+len(snapshot.AuthDigest))
			binary.BigEndian.PutUint64(result, snapshot.DeclaredAuthFiles)
			copy(result[8:], snapshot.AuthDigest[:])
			return result, nil
		}),
		proxy.CodexCanaryJSONFieldProtection(proxy.CodexCanaryRoutingDefault, filepath.Join(configDirectory, "proxy.json"), "codex_routing_default_account_key"),
	}, nil
}

func codexBarCanaryProtectionSnapshot(source *codexprov.CodexBarSource) (codexprov.CodexBarProtectionSnapshot, error) {
	snapshot, err := source.ProtectionSnapshot()
	if errors.Is(err, codexprov.ErrExternalUnavailable) {
		return codexprov.CodexBarProtectionSnapshot{}, os.ErrNotExist
	}
	if err != nil {
		return codexprov.CodexBarProtectionSnapshot{}, codexCanaryProtectionSourceError(err)
	}
	return snapshot, nil
}

func codexCanaryProtectionSourceError(err error) error {
	category := error(codexprov.ErrExternalInvalid)
	if errors.Is(err, codexprov.ErrExternalUnsafePath) {
		category = codexprov.ErrExternalUnsafePath
	}
	return fmt.Errorf("validate protected CodexBar credentials: %w", category)
}
