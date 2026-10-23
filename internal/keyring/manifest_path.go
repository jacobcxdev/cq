package keyring

import (
	"fmt"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

var resolveCQManifestRoots = func() (userdirs.Roots, error) { return userdirs.Default(userdirs.CacheRoot) }

func defaultCQManifestPath() (string, error) {
	roots, err := resolveCQManifestRoots()
	if err != nil {
		return "", fmt.Errorf("resolve CQ manifest roots: %w", err)
	}
	home, err := cqManifestHome()
	if err != nil {
		return "", fmt.Errorf("resolve CQ manifest home: %w", err)
	}
	return CQManifestPath(roots, home), nil
}
