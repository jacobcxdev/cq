package dots

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNativeDotHubRendererOverride(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the renderer fixture")
	}
	expression := strings.NewReplacer(
		"import('app://-/assets/MODULE_ASSET_PLACEHOLDER')", "Promise.resolve(fixtureExports)",
		"CQ_DOT_PROFILE_IDS", "[\"one\",\"two\"]",
		"CQ_SYSTEM_ACCOUNT", "\"local-account\"",
	).Replace(nativeDotHubJS)
	script := `
const assert = require('node:assert/strict');
const gate = {scope: {}};
const fixtureExports = {GMr: 'account', XMr: 'user', JMr: 'ready', __webpack_require__: Object.assign(() => hub, {m: {vXFM: true}})};
const hub = {QO: gate, M1: 'profiles', bb: 'reachable'};
const primary = {id: 'one', status: 'active'};
const secondary = {id: 'two', status: 'active'};
const original = () => false;
const binding = {read: original};
const marker = Symbol.for('cq.dot-hub.29803.v1');
let value = false;
let writes = 0;
let owner = 'local-account';
let profiles = [primary, secondary];
let routeAvailable = true;
const store = {
  get: () => value,
  set(atom, next) {
    atom.write(null, (target, updated) => {
      assert.equal(target, binding);
      assert.equal(typeof target.init, 'boolean');
      value = updated;
      writes++;
    }, next);
  },
};
const scope = {
  scope: gate.scope,
  node: {store, cachedBindings: new Map([[gate, binding]])},
  watch() {},
  get(atom, id) {
    switch (atom) {
      case gate: return value;
      case 'account': return owner;
      case 'user': return 'local-user';
      case 'ready': return true;
      case 'profiles': return profiles;
      case 'reachable': return routeAvailable && value && profiles.some(p => p.id === id);
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

  routeAvailable = true;
  const baseline = writes;
  assert.deepEqual(await enable(), {enabled: true});
  assert.deepEqual(await enable(), {enabled: true});
  assert.equal(writes, baseline + 1);
  assert.equal(scope.get(hub.bb, 'one'), true);
  assert.equal(scope.get(hub.bb, 'two'), true);
  binding[marker].restore();
  assert.equal(value, false);
  assert.equal(binding.read, original);
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	if output, err := exec.CommandContext(t.Context(), node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("Dot Hub fixture: %v\n%s", err, output)
	}
}
