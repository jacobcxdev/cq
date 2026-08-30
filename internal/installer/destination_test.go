package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestGoDestinationUsesAbsoluteGOBIN(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := goDestinationHarness(map[string]string{"GOBIN": bin, "PATH": bin})

	destination, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if destination != filepath.Join(bin, goDestinationExecutableName(runtime.GOOS)) {
		t.Fatalf("destination = %q", destination)
	}
}

func TestGoDestinationUsesFirstAbsoluteGOPATHEntry(t *testing.T) {
	gopath := filepath.Join(t.TempDir(), "go")
	bin := filepath.Join(gopath, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	separator := string(os.PathListSeparator)
	resolver := goDestinationHarness(map[string]string{
		"GOPATH": "relative" + separator + gopath,
		"PATH":   bin,
	})

	destination, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if destination != filepath.Join(bin, goDestinationExecutableName(runtime.GOOS)) {
		t.Fatalf("destination = %q", destination)
	}
}

func TestGoDestinationFallsBackToGoEnvGOPATH(t *testing.T) {
	gopath := filepath.Join(t.TempDir(), "go")
	bin := filepath.Join(gopath, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := goDestinationHarness(map[string]string{"PATH": bin})
	resolver.GoEnvGOPATH = func(context.Context) (string, error) { return gopath + "\n", nil }

	destination, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(destination) != bin {
		t.Fatalf("destination = %q", destination)
	}
}

func TestGoDestinationRejectsInvalidOrUnusableDestinations(t *testing.T) {
	root := t.TempDir()
	absolute := filepath.Join(root, "bin")
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		environment map[string]string
		goEnv       func(context.Context) (string, error)
		writable    func(string) error
	}{
		{name: "relative GOBIN", environment: map[string]string{"GOBIN": "bin", "PATH": "bin"}},
		{name: "empty GOPATH", environment: map[string]string{"PATH": absolute}, goEnv: func(context.Context) (string, error) { return "", nil }},
		{name: "relative GOPATH", environment: map[string]string{"GOPATH": "relative", "PATH": absolute}},
		{name: "not writable", environment: map[string]string{"GOBIN": absolute, "PATH": absolute}, writable: func(string) error { return errors.New("read only") }},
		{name: "absent from PATH", environment: map[string]string{"GOBIN": absolute, "PATH": filepath.Join(root, "other")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := goDestinationHarness(test.environment)
			if test.goEnv != nil {
				resolver.GoEnvGOPATH = test.goEnv
			}
			if test.writable != nil {
				resolver.Writable = test.writable
			}
			if _, err := resolver.Resolve(context.Background()); err == nil {
				t.Fatal("Resolve() succeeded")
			}
		})
	}
}

func TestGoDestinationPATHComparisonMatchesPlatformRules(t *testing.T) {
	if !pathContainsDirectory(`C:\Tools;C:\Users\Test\Go\Bin`, `c:/users/test/go/bin`, "windows") {
		t.Fatal("Windows PATH comparison was not case-insensitive")
	}
	if pathContainsDirectory("/Users/Test/go/bin:/usr/bin", "/users/test/go/bin", "darwin") {
		t.Fatal("Unix PATH comparison was case-insensitive")
	}
	if !pathContainsDirectory("/Users/Test/go/bin:/usr/bin", "/Users/Test/go/bin", "darwin") {
		t.Fatal("exact Unix PATH entry was not found")
	}
}

func TestAdoptClassifiesOnlyCQMainPackage(t *testing.T) {
	root := t.TempDir()
	cqBinary := filepath.Join(root, goDestinationExecutableName(runtime.GOOS))
	buildTestBinary(t, filepath.Join("..", ".."), cqBinary, "./cmd/cq")
	fixtureRoot := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixtureRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAdoptionFixture(t, fixtureRoot)
	fixtureBinary := filepath.Join(root, "fixture-"+goDestinationExecutableName(runtime.GOOS))
	buildTestBinary(t, fixtureRoot, fixtureBinary, ".")
	store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(root, "state")}}
	classifier := BinaryClassifier{FS: fsutil.OSFileSystem{}, State: store}

	classification, err := classifier.Classify(installstate.OwnerGo, filepath.Join(root, "absent"))
	if err != nil || classification != BinaryAbsent {
		t.Fatalf("absent classification = %v, %v", classification, err)
	}
	classification, err = classifier.Classify(installstate.OwnerGo, cqBinary)
	if err != nil || classification != BinaryAdoptable {
		t.Fatalf("CQ classification = %v, %v", classification, err)
	}
	classification, err = classifier.Classify(installstate.OwnerGo, fixtureBinary)
	if err != nil || classification != BinaryForeign {
		t.Fatalf("fixture classification = %v, %v", classification, err)
	}
}

func TestAdoptClassifiesMatchingInstallStateAsOwned(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, goDestinationExecutableName(runtime.GOOS))
	if err := os.WriteFile(binary, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := installstate.DigestFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(root, "state")}}
	if err := store.Save(installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         installstate.OwnerGo,
		Version:       "0.27.0",
		Executable:    binary,
		BinaryDigest:  digest,
		Services:      []string{"proxy", "refresh"},
	}); err != nil {
		t.Fatal(err)
	}
	classifier := BinaryClassifier{FS: fsutil.OSFileSystem{}, State: store}
	classification, err := classifier.Classify(installstate.OwnerGo, binary)
	if err != nil || classification != BinaryOwned {
		t.Fatalf("owned classification = %v, %v", classification, err)
	}
	if _, err := classifier.Classify(installstate.OwnerHomebrew, binary); !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("ownership conflict = %v", err)
	}
}

func goDestinationHarness(environment map[string]string) GoDestinationResolver {
	return GoDestinationResolver{
		GOOS: runtime.GOOS,
		Getenv: func(name string) string {
			return environment[name]
		},
		GoEnvGOPATH: func(context.Context) (string, error) { return "", nil },
		Stat:        os.Stat,
		Writable:    func(string) error { return nil },
	}
}

func buildTestBinary(t *testing.T, directory, output, target string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", output, target)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", target, err, output)
	}
}

func writeAdoptionFixture(t *testing.T, directory string) {
	t.Helper()
	files := map[string]string{
		"go.mod":  "module example.com/foreign\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (classification BinaryOwnership) String() string {
	switch classification {
	case BinaryAbsent:
		return "absent"
	case BinaryOwned:
		return "owned"
	case BinaryAdoptable:
		return "adoptable"
	case BinaryForeign:
		return "foreign"
	default:
		return fmt.Sprintf("unknown(%d)", classification)
	}
}
