package dots

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Execute the installed application's host-routing function with CQ's launch
// environment. This never launches the desktop or connects to a real host.
func TestInstalledNativeTransportSelection(t *testing.T) {
	app := os.Getenv("CQ_TEST_NATIVE_DISCOVERY_APP")
	if app == "" {
		t.Skip("set CQ_TEST_NATIVE_DISCOVERY_APP for read-only installed transport qualification")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for installed transport qualification")
	}
	// Resolve version-manager shims before switching to the fixture HOME.
	nativeNode, err := exec.Command(node, "-p", "process.execPath").Output()
	if err != nil {
		t.Fatal("resolve native Node executable")
	}
	node = strings.TrimSpace(string(nativeNode))
	// The imported predicates are host classification and the local host's
	// optional websocket route. Keep their inputs explicit; the production
	// function itself decides whether the environment overrides those routes.
	pattern := regexp.MustCompile(`function (` + jsIdentifier + `)\((` + jsIdentifier + `)\)\{return"1"===process\.env\.CODEX_APP_SERVER_FORCE_CLI\?null:\(0,(` + jsIdentifier + `)\.(` + jsIdentifier + `)\)\(` + jsIdentifier + `\.id\)\?` + jsIdentifier + `\.websocket_url\?\?null:\(0,(` + jsIdentifier + `)\.(` + jsIdentifier + `)\)\(` + jsIdentifier + `\)\}`)
	var match []string
	var localConnect string
	for _, source := range installedBootstrapSourcesForTest(t, app) {
		if found := pattern.FindStringSubmatch(source); found != nil {
			if match != nil {
				t.Fatal("installed host routing function ambiguous")
			}
			match = found
		}
		if index := strings.Index(source, "process.env.CODEX_APP_SERVER_USE_LOCAL_DAEMON"); index >= 0 {
			start := strings.LastIndex(source[:index], "async connect(){")
			end := strings.Index(source[index:], "getIoStatsSnapshot(){")
			if start < 0 || end < 0 || localConnect != "" {
				t.Fatal("installed local transport function ambiguous")
			}
			localConnect = source[start : index+end]
		}
	}
	if match == nil || localConnect == "" {
		t.Fatal("installed host routing contract changed")
	}
	_, settings := preparedInstallationFixture(t)
	input, _, _ := launchFixtureEnvironment(t, settings)
	input = append(input, "CODEX_APP_SERVER_FORCE_CLI=1")
	_, env, err := prepareLaunch(settings, "http://127.0.0.1:12345", "/stable/cq", input)
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(env, "CODEX_CLI_PATH") == "" {
		t.Fatal("local CQ bridge missing")
	}
	script := `const assert=require('node:assert/strict');` +
		`const ` + match[3] + `={` + match[4] + `:id=>id==='durable'};` +
		`const ` + match[5] + `={` + match[6] + `:host=>host.websocket_url??null};` +
		match[0] +
		`assert.equal(` + match[1] + `({id:'durable',websocket_url:'wss://fixture.invalid/rpc'}),'wss://fixture.invalid/rpc','CQ must preserve durable websocket ownership');` +
		`assert.equal(` + match[1] + `({id:'local'}),null,'local host must retain CLI bridge');` +
		`process.env.CODEX_APP_SERVER_USE_LOCAL_DAEMON='1';` +
		`const G=async()=>({executablePath:process.env.CODEX_CLI_PATH}),o=require('node:path'),p={D:()=>'/fixture'},L=()=>{throw Error('local daemon probed despite CQ CLI override')},T={parse:JSON.parse},u={dlm:()=>true},i={createConnection:()=>{throw Error('local daemon connected')}},C={b:class{constructor(){throw Error('local daemon transport selected')}}},H=class{constructor(command){this.executablePath=command.executablePath}};` +
		`const local={options:{hostConfig:{kind:'local'}},ioStatsTracker:{},` + localConnect + `};` +
		`local.connect().then(transport=>{assert.equal(local.kind,'stdio');assert.equal(transport.executablePath,process.env.CODEX_CLI_PATH);}).catch(error=>{console.error(error);process.exitCode=1});`
	command := exec.Command(node, "-e", script)
	command.Env = env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installed host transport selection: %v\n%s", err, output)
	}
}

// Read only the bounded bootstrap entries needed for installed compatibility
// checks. The signed archive remains untouched.
func installedBootstrapSourcesForTest(t *testing.T, app string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(app, "Contents", "Resources", "app.asar"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var prefix [16]byte
	if _, err := f.ReadAt(prefix[:], 0); err != nil {
		t.Fatal(err)
	}
	headerSize := int64(binary.LittleEndian.Uint32(prefix[12:]))
	dataStart := 8 + int64(binary.LittleEndian.Uint32(prefix[4:]))
	if headerSize < 2 || headerSize > 16<<20 {
		t.Fatal("installed archive header out of bounds")
	}
	header := make([]byte, headerSize)
	if _, err := f.ReadAt(header, 16); err != nil {
		t.Fatal(err)
	}
	var root nativeArchiveEntry
	if err := json.Unmarshal(header, &root); err != nil {
		t.Fatal(err)
	}
	var sources []string
	for name, entry := range root.Files[".vite"].Files["build"].Files {
		if !strings.HasPrefix(name, "early-bootstrap-") || !strings.HasSuffix(name, ".js") {
			continue
		}
		if entry.Unpacked || entry.Link != "" || entry.Size < 1 || entry.Size > 32<<20 {
			t.Fatal("installed bootstrap bounds invalid")
		}
		offset, err := strconv.ParseInt(entry.Offset, 10, 64)
		if err != nil || offset < 0 {
			t.Fatal("installed bootstrap offset invalid")
		}
		data := make([]byte, entry.Size)
		if _, err := f.ReadAt(data, dataStart+offset); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, string(data))
	}
	return sources
}
