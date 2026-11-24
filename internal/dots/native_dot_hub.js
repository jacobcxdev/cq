(async () => {
  const resolved = await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
  const runtime = (await import('app://-/assets/MODULE_ASSET_PLACEHOLDER')).__webpack_require__;
  if (typeof runtime !== 'function' || !Object.hasOwn(runtime.m ?? {}, 'vXFM')) return {enabled: false};

  const hub = runtime('vXFM');
  const gate = hub.QO;
  const teamHub = CQ_TEAM_DOT_HUB;
  const catalogue = teamHub ? runtime('imTH').Tr : hub.M1;
  const root = document.getElementById('root');
  const key = root && Object.keys(root).find(name => name.startsWith('__reactContainer$'));
  const container = key && root[key];
  const fibres = [container?.current ?? container];
  const seen = new Set();
  let scope;

  function inspect(value, depth) {
    if (scope || !value || typeof value !== 'object' || seen.has(value)) return;
    seen.add(value);
    try {
      if (value.scope === gate?.scope && typeof value.get === 'function' &&
          typeof value.watch === 'function' && value.get(resolved.GMr) === CQ_SYSTEM_ACCOUNT &&
          typeof value.get(resolved.XMr) === 'string' && value.get(resolved.JMr)) {
        scope = value;
        return;
      }
    } catch {}
    if (depth > 0) {
      for (const field of Object.values(Object.getOwnPropertyDescriptors(value))) {
        if ('value' in field) inspect(field.value, depth - 1);
      }
    }
  }

  for (let i = 0; i < fibres.length && i < 10000 && !scope; i++) {
    const fibre = fibres[i];
    if (!fibre) continue;
    inspect(fibre.memoizedProps, 3);
    let hook = fibre.memoizedState;
    for (let j = 0; hook && j < 100 && !scope; j++, hook = hook.next) inspect(hook.memoizedState, 3);
    if (fibre.child) fibres.push(fibre.child);
    if (fibre.sibling) fibres.push(fibre.sibling);
  }
  if (!scope) return {enabled: false};

  const expected = new Set(CQ_DOT_PROFILE_IDS);
  const profiles = scope.get(catalogue);
  if (!Array.isArray(profiles) || profiles.length !== expected.size ||
      new Set(profiles.map(profile => profile.id)).size !== expected.size ||
      profiles.some(profile => profile.status !== 'active' || !expected.has(profile.id))) {
    return {enabled: false};
  }

  const enabled = scope.get(gate);
  const store = scope.node.store;
  const binding = scope.node.cachedBindings.get(gate);
  const marker = Symbol.for(teamHub ? 'cq.dot-hub.30607.v1' : 'cq.dot-hub.29803.v1');
  const reachable = () => profiles.every(profile => scope.get(hub.bb, profile.id) === true);
  if (binding?.[marker]) return {enabled: enabled === true && reachable()};
  if (!binding || enabled !== false || store.get(binding) !== false ||
      'init' in binding || 'write' in binding || !Object.isExtensible(binding)) return {enabled: false};

  const original = Object.getOwnPropertyDescriptor(binding, 'read');
  if (!original?.writable || typeof original.value !== 'function') return {enabled: false};

  let restoreMembership = () => {};
  function extendTeamMembership() {
    const cache = scope.node.cachedBindings;
    const catalogueBinding = cache.get(catalogue);
    const groups = scope.get(hub.Ur);
    const groupsBinding = cache.get(hub.Ur);
    if (!catalogueBinding || !groupsBinding || !Array.isArray(groups?.mine) || !Array.isArray(groups.shared)) return false;

    // Resolve the shared-profile selector through its sole array dependency.
    // The same selector feeds both group rendering and per-Dot navigation.
    const dependencies = new Set();
    groupsBinding.read(atom => {
      const value = store.get(atom);
      if (Array.isArray(value)) dependencies.add(atom);
      return value;
    });
    if (dependencies.size !== 1) return false;
    const [members] = dependencies;
    const read = Object.getOwnPropertyDescriptor(members, 'read');
    if (members === catalogueBinding || !read?.writable || typeof read.value !== 'function' ||
        'init' in members || 'write' in members || !Object.isExtensible(members)) return false;
    const shared = store.get(members);
    const nativeGroups = [...groups.mine, ...groups.shared];
    if (shared.length !== nativeGroups.length ||
        shared.some(profile => profile.scope !== 'shared' || !nativeGroups.includes(profile))) return false;

    let readsCatalogue = false;
    read.value.call(members, atom => {
      if (atom === catalogueBinding) readsCatalogue = true;
      return store.get(atom);
    });
    if (!readsCatalogue) return false;

    const select = values => values.filter(profile => profile.scope === 'shared' ||
      (expected.has(profile.id) && profile.status === 'active'));
    function seed(value) {
      Object.defineProperties(members, {
        init: {value, configurable: true},
        write: {value: (_get, set, next) => set(members, next), configurable: true},
      });
      try {
        store.set(members, value);
      } finally {
        delete members.init;
        delete members.write;
      }
    }
    restoreMembership = () => {
      Object.defineProperty(members, 'read', read);
      const current = store.get(catalogueBinding);
      seed(Array.isArray(current) ? current.filter(profile => profile.scope === 'shared') : shared);
    };
    members.read = get => {
      const current = get(catalogueBinding);
      return Array.isArray(current) ? select(current) : read.value.call(members, get);
    };
    seed(select(profiles));
    const updated = scope.get(hub.Ur);
    return [...updated.mine, ...updated.shared].every(profile => expected.has(profile.id)) &&
      updated.mine.length + updated.shared.length === expected.size;
  }

  // Keep the atom identity so the existing sidebar subscribers receive the change.
  // Give the renderer gate the store's writable value contract.
  function restore() {
    try {
      if (Object.hasOwn(binding, 'write')) store.set(binding, false);
    } finally {
      Object.defineProperty(binding, 'read', original);
      delete binding.init;
      delete binding.write;
      delete binding[marker];
      restoreMembership();
    }
  }
  try {
    if (teamHub && !extendTeamMembership()) {
      restoreMembership();
      return {enabled: false};
    }
    Object.defineProperties(binding, {
      init: {value: false, writable: true, configurable: true},
      write: {value: (_get, set, value) => set(binding, value), writable: true, configurable: true},
      [marker]: {value: {restore}, configurable: true},
    });
    binding.read = get => get(binding);
    store.set(binding, true);
    if (scope.get(gate) === true && reachable()) return {enabled: true};
  } catch {}
  restore();
  return {enabled: false};
})()
