import test_support
import json
import shutil
import subprocess
import unittest
from unittest.mock import patch

import native_room_cache as recovery


class NativeRoomCacheTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which('node'), 'Node required for native JavaScript regression')
    def test_shared_producer_outlives_first_chat_without_retry(self):
        script = r"""
const assert = require('node:assert/strict');
const install = GUARD;
class Cache {
    constructor() {
        this.accountId='account';this.userId='user';this.abort=new AbortController();
        this.current=true;this.rooms=new Map();this.history=new Map();
    }
    get disposed() {return this.abort.signal.aborted;}
    isCurrentAccount() {return this.current;}
    assertCurrent() {
        this.abort.signal.throwIfAborted();
        if (!this.current) throw new DOMException('Account changed','AbortError');
    }
    read(map,key,fetch,signal) {
        this.assertCurrent();signal?.throwIfAborted();
        let entry=map.get(key);
        if (!entry || entry.invalidated) {
            entry={promise:fetch(this.abort.signal)};map.set(key,entry);
        }
        return entry.promise;
    }
}
async function shared(cache, map, setup) {
    const first=new AbortController(), current=new AbortController();
    let release;const gate=new Promise(r=>release=r);
    let requests=0;
    const api={safeGet:async(path, options)=>{
        requests++;await gate;options.signal.throwIfAborted();
        assert.equal(options.additionalHeaders['ChatGPT-Account-Id'],'account');
        assert.equal(options.retry,false);
        assert.equal(options.parameters.path.room_id,'room');
        assert.equal(path.endsWith('/messages'),map===cache.history);
        assert.deepEqual(options.parameters.query,map===cache.history?{limit:20}:undefined);
        return 'shared';
    }};
    const state=setup?.(cache,['room'],api);
    const producer=async signal=>{
        const value=await api.safeGet('/unused',{signal});
        first.signal.throwIfAborted();return value;
    };
    // Baseline mimics the native client's controller-bound request closure.
    const stale=async signal=>{requests++;await gate;signal.throwIfAborted();
        first.signal.throwIfAborted();return 'shared';};
    const old=cache.read(map,'room',stale,first.signal).catch(e=>e);
    const active=cache.read(map,'room',()=>assert.fail('shared cache missed'),current.signal).catch(e=>e);
    first.abort();release();
    const results=await Promise.all([old,active]);
    assert.equal(requests,1,'must not retry shared GET');
    if (state) {
        assert.deepEqual(results,['shared','shared']);
        assert.equal(state.ownedReads,1);
        assert.equal(await cache.read(map,'room',producer),'shared');
        assert.equal(requests,1,'cached result must stay cached');
    } else assert.equal(results[1].name,'AbortError','baseline must reproduce shared cancellation');
    return state;
}
(async()=>{
    const original=Cache.prototype.read;
    let c=new Cache();await shared(c,c.rooms);
    for (const kind of ['rooms','history']) {
        c=new Cache();const state=await shared(c,c[kind],install);
        assert.equal(install(c,['room'],{}),state);state.restore();
    }
    for (const variant of ['foreign-room','foreign-account','foreign-user','other-map']) {
        c=new Cache();let attempts=0;
        const state=install(c,['room'],{safeGet:()=>assert.fail('unbound read')});
        if(variant==='foreign-account')c.accountId='other';
        if(variant==='foreign-user')c.userId='other';
        const key=variant==='foreign-room'?'other':'room';
        assert.equal(await c.read(variant==='other-map'?new Map():c.rooms,key,async()=>{attempts++;return 'original';}),'original');
        assert.equal(attempts,1);state.restore();
    }
    for(const variant of ['cache-aborted','account-changed','caller-aborted','HTTP-error','network-abort']) {
        c=new Cache();let attempts=0;const signal=new AbortController();
        const failure=variant==='HTTP-error'?new Error('HTTP failure'):new DOMException('cancelled','AbortError');
        const state=install(c,['room'],{safeGet:async()=>{attempts++;throw failure;}});
        if(variant==='cache-aborted')c.abort.abort();
        if(variant==='account-changed')c.current=false;
        if(variant==='caller-aborted')signal.abort();
        await assert.rejects(async()=>c.read(c.rooms,'room',()=>assert.fail('controller closure'),signal.signal));
        assert.equal(attempts,['HTTP-error','network-abort'].includes(variant)?1:0);
        state.restore();
    }
    assert.equal(Cache.prototype.read,original);
})().catch(e=>{console.error(e);process.exitCode=1;});
""".replace('GUARD', recovery.GUARD)
        result = subprocess.run(['node', '-e', script], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_install_uses_only_configured_profiles_and_pinned_module(self):
        config = recovery.settings
        with patch.object(config, 'INTEGRATION_MODE', 'native', create=True), \
             patch.object(recovery.isolated_native, 'evaluate_all', return_value=[{'installed': True, 'ownedReads': 0}, {'installed':False}]) as evaluate:
            self.assertEqual(recovery.install(), {'installed': True, 'windows': 1, 'ownedReads': 0})
        expression = evaluate.call_args.args[0]
        self.assertIn(config.MODULE_ASSET, expression)
        self.assertIn(json.dumps([p for p, _ in config.DOTS.values()]), expression)
        self.assertNotIn('safePost', expression)
        self.assertNotIn('safePatch', expression)

    def test_unavailable_install_fails_and_isolated_mode_does_not_inject(self):
        with patch.object(recovery.settings, 'INTEGRATION_MODE', 'native', create=True), \
             patch.object(recovery.isolated_native, 'evaluate_all', return_value=None), self.assertRaises(ValueError):
            recovery.install()
        with patch.object(recovery.settings, 'INTEGRATION_MODE', 'isolated', create=True), \
             patch.object(recovery.isolated_native, 'evaluate_all') as evaluate:
            self.assertEqual(recovery.install(), {'installed': False})
            evaluate.assert_not_called()


if __name__ == '__main__':
    unittest.main()
