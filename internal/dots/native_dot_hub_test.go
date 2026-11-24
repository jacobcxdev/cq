package dots

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

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
