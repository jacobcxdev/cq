package dots

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type dotHubTestEvaluator func(string) ([]any, error)

func (evaluate dotHubTestEvaluator) EvaluateAll(_ context.Context, expression string) ([]any, error) {
	return evaluate(expression)
}

func TestNativeDotHubPinsAndTeamModes(t *testing.T) {
	const windowsHash = "7b5175d7932e26f8fca91fb0b25bc85e424304ce368c8ce21a68b1be2c174b28"
	const windows6357Hash = "c524092e7df40f372081771fe2afe1b2fdf4920c6df014c5432b4517a5330ba0"
	const mac30866Hash = "c6488597167abbbeff5a2422cdbe63f7466143b9770a224c9bc6ac2168631f13"
	base := Settings{
		IntegrationMode: "native",
		Dots: map[string]Dot{
			"one": {ProfileID: "profile-one"},
			"two": {ProfileID: "profile-two"},
		},
		NativeBindings: &NativeBindings{Rspack: &NativeRspackBindings{}},
		AppVersion:     "26.1120.5083.0", AppBuild: "5083", ModuleAsset: "348410.477a16d4d9.js", ASARSHA256: windowsHash,
	}
	windows6357 := func(s *Settings) {
		s.AppVersion, s.AppBuild = "26.1120.6357.0", "6357"
		s.ModuleAsset, s.ASARSHA256 = "348410.f0081c7d18.js", windows6357Hash
	}
	for _, test := range []struct {
		name            string
		edit            func(*Settings)
		supported, team bool
	}{
		{"qualified Windows", func(*Settings) {}, runtime.GOOS == "windows", true},
		{"qualified Windows build 6357", windows6357, runtime.GOOS == "windows", true},
		{"Windows build 6357 with old archive", func(s *Settings) { windows6357(s); s.ASARSHA256 = windowsHash }, false, false},
		{"Windows build 6357 with old entry asset", func(s *Settings) { windows6357(s); s.ModuleAsset = base.ModuleAsset }, false, false},
		{"unqualified Windows hash", func(s *Settings) { s.ASARSHA256 = strings.Repeat("0", 64) }, false, false},
		{"unqualified Windows build", func(s *Settings) { s.AppBuild = "5084" }, false, false},
		{"unqualified Windows version", func(s *Settings) { s.AppVersion = "26.1121.5083.0" }, false, false},
		{"unqualified Windows asset", func(s *Settings) { s.ModuleAsset = "348410.abcdef0123.js" }, false, false},
		{"no native bindings", func(s *Settings) { s.NativeBindings = nil }, false, false},
		{"isolated mode", func(s *Settings) { s.IntegrationMode = "isolated" }, false, false},
		{"single Dot", func(s *Settings) { s.Dots = map[string]Dot{"one": {ProfileID: "profile-one"}} }, false, false},
		{"legacy Mac", func(s *Settings) { s.AppBuild, s.ASARSHA256 = "29803", dotHubArchive29803 }, true, false},
		{"Team Hub Mac", func(s *Settings) { s.AppBuild, s.ASARSHA256 = "30607", dotHubArchive30607 }, true, true},
		{"Team Hub Mac build 30866", func(s *Settings) { s.AppBuild, s.ASARSHA256 = "30866", mac30866Hash }, true, true},
		{"unqualified Mac build 30866", func(s *Settings) { s.AppBuild, s.ASARSHA256 = "30866", dotHubArchive30607 }, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.edit(&cfg)
			calls := 0
			evaluator := dotHubTestEvaluator(func(expression string) ([]any, error) {
				calls++
				if !strings.Contains(expression, "const teamHub = "+strconv.FormatBool(test.team)+";") {
					t.Fatalf("wrong Team Hub mode for %s", test.name)
				}
				return []any{map[string]any{"enabled": true}}, nil
			})
			if got := enableNativeDotHub(t.Context(), &cfg, evaluator); got != test.supported || (calls == 1) != test.supported {
				t.Fatalf("enable=%t, evaluations=%d, want support %t", got, calls, test.supported)
			}
		})
	}
}

func TestNativeDotHubRendererOverride(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the renderer fixture")
	}
	for _, teamHub := range []bool{false, true} {
		t.Run(strconv.FormatBool(teamHub), func(t *testing.T) {
			testNativeDotHubRendererOverride(t, node, teamHub)
		})
	}
}

func testNativeDotHubRendererOverride(t *testing.T, node string, teamHub bool) {
	t.Helper()
	mode := strconv.FormatBool(teamHub)
	expression := strings.NewReplacer(
		"import('app://-/assets/MODULE_ASSET_PLACEHOLDER')", "Promise.resolve(fixtureExports)",
		"CQ_TEAM_DOT_HUB", mode,
		"CQ_DOT_PROFILE_IDS", "[\"one\",\"two\"]",
		"CQ_SYSTEM_ACCOUNT", "\"local-account\"",
	).Replace(nativeDotHubJS)
	script := `
const assert = require('node:assert/strict');
const teamHub = ` + mode + `;
const gate = {scope: {}};
const fixtureExports = {GMr: 'account', XMr: 'user', JMr: 'ready', __webpack_require__: Object.assign(id => id === 'vXFM' ? hub : {Tr: 'profiles'}, {m: {vXFM: true}})};
const hub = {QO: gate, M1: 'profiles', bb: 'reachable', Ur: 'groups'};
const primary = {id: 'one', status: 'active', scope: 'personal'};
const secondary = {id: 'two', status: 'active', scope: 'shared'};
const original = () => false;
const binding = {read: original};
const catalogueBinding = {read: () => profiles};
const originalMembers = get => get(catalogueBinding).filter(profile => profile.scope === 'shared');
const members = {read: originalMembers};
const groups = {read: get => ({mine: [], shared: get(members)})};
const marker = Symbol.for(teamHub ? 'cq.dot-hub.30607.v1' : 'cq.dot-hub.29803.v1');
let value = false;
let writes = 0;
let owner = 'local-account';
let profiles = [primary, secondary];
let routeAvailable = true;
const store = {
  get: atom => atom === binding ? value : atom.read(store.get),
  set(atom, next) {
    atom.write(null, (target, updated) => {
      assert.equal(target, atom);
      if (target === binding) {
        assert.equal(typeof target.init, 'boolean');
        value = updated;
        writes++;
      } else {
        assert.equal(target, members);
        assert.ok(Array.isArray(updated));
      }
    }, next);
  },
};
const scope = {
  scope: gate.scope,
  node: {store, cachedBindings: new Map([[gate, binding], ['profiles', catalogueBinding], ['groups', groups]])},
  watch() {},
  get(atom, id) {
    switch (atom) {
      case gate: return value;
      case 'account': return owner;
      case 'user': return 'local-user';
      case 'ready': return true;
      case 'profiles': return profiles;
      case 'groups': return store.get(groups);
      case 'reachable': return routeAvailable && value && (teamHub ? store.get(members) : profiles).some(p => p.id === id);
    }
  },
};
const root = {'__reactContainer$fixture': {current: {memoizedProps: {scope}}}};
const document = {getElementById: () => root};
const enable = () => ` + expression + `;
(async () => {
  owner = 'foreign-account';
  assert.deepEqual(await enable(), {enabled: false});
  owner = 'local-account';
  profiles = [primary, primary];
  assert.deepEqual(await enable(), {enabled: false});
  assert.equal(writes, 0);

  profiles = [primary, secondary];
  routeAvailable = false;
  assert.deepEqual(await enable(), {enabled: false});
  assert.equal(value, false);
  assert.equal(binding.read, original);
  assert.equal('write' in binding, false);
  assert.equal(members.read, originalMembers);

  routeAvailable = true;
  const baseline = writes;
  assert.deepEqual(await enable(), {enabled: true});
  assert.deepEqual(await enable(), {enabled: true});
  assert.equal(writes, baseline + 1);
  assert.equal(scope.get(hub.bb, 'one'), true);
  assert.equal(scope.get(hub.bb, 'two'), true);
  if (teamHub) {
    assert.deepEqual(store.get(members), profiles);
    profiles = [primary, {...secondary, status: 'inactive', scope: 'personal'}];
    assert.deepEqual(store.get(members), [primary]);
    assert.equal(scope.get(hub.bb, 'two'), false);
  }
  binding[marker].restore();
  assert.equal(value, false);
  assert.equal(binding.read, original);
  assert.equal(members.read, originalMembers);
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	if output, err := exec.CommandContext(t.Context(), node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("Dot Hub fixture: %v\n%s", err, output)
	}
}
