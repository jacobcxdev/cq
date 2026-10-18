import test_support
import json
import shutil
import subprocess
import unittest
from unittest.mock import patch

import native_room_cache as recovery


class NativeRoomCacheTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which('node'), 'Node required for native JavaScript regression')
    def test_shared_cancellation_recovers_only_current_bound_reads_once(self):
        script = r"""
const assert = require('node:assert/strict');
const install = GUARD;
const abort = () => new DOMException('signal is aborted without reason', 'AbortError');
class Cache {
    constructor() {
        this.accountId='account';this.userId='user';this.abort=new AbortController();
        this.current=true;this.calls=0;
    }
    get disposed() {return this.abort.signal.aborted;}
    isCurrentAccount() {return this.current;}
    assertCurrent() {this.abort.signal.throwIfAborted();if(!this.current)throw abort();}
    read(map,key,fetch) {
        let entry=map.get(key);
        if(!entry) {entry={aborted:false};entry.promise=fetch(this.abort.signal);map.set(key,entry);}
        return entry.promise;
    }
}
(async()=>{
    const original=Cache.prototype.read, cache=new Cache(), map=new Map();
    const a=new AbortController(), b=new AbortController();
    let release;const gate=new Promise(r=>release=r);
    const producer=async()=>{await gate;a.signal.throwIfAborted();return 'producer';};
    const current=async()=>{cache.calls++;b.signal.throwIfAborted();return 'current';};
    const state=install(cache,['room']);
    assert.equal(install(cache,['room']),state);
    const old=cache.read(map,'room',producer,a.signal).catch(e=>e);
    const active=cache.read(map,'room',current,b.signal);
    a.abort();release();
    assert.equal((await old).name,'AbortError');
    assert.equal(await active,'current');assert.equal(cache.calls,1);assert.equal(state.recovered,1);
    for(const variant of ['foreign-room','foreign-account','foreign-user','caller-aborted',
                          'cache-aborted','account-changed','entry-aborted','ordinary-error','retry-aborted']) {
        const c=new Cache(), signal=new AbortController();let attempts=0;
        const error=variant==='ordinary-error'?new Error('HTTP failure'):abort();
        const key=variant==='foreign-room'?'other':'room';
        const entry={aborted:variant==='entry-aborted',promise:Promise.reject(error)};
        const entries=new Map([[key,entry]]);
        if(variant==='foreign-account')c.accountId='other';
        if(variant==='foreign-user')c.userId='other';
        if(variant==='caller-aborted')signal.abort();
        if(variant==='cache-aborted')c.abort.abort();
        if(variant==='account-changed')c.current=false;
        await assert.rejects(c.read(entries,key,async()=>{attempts++;throw error;},signal.signal),e=>e===error);
        assert.equal(attempts,variant==='retry-aborted'?1:0,variant);
    }
    const success=new Cache(), entries=new Map([['room',{aborted:false,promise:Promise.resolve('cached')}]]);
    assert.equal(await success.read(entries,'room',()=>assert.fail('unneeded fetch')),'cached');
    state.restore();assert.equal(Cache.prototype.read,original);
})().catch(e=>{console.error(e);process.exitCode=1;});
""".replace('GUARD', recovery.GUARD)
        result = subprocess.run(['node', '-e', script], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_install_uses_only_configured_profiles_and_pinned_module(self):
        config = recovery.settings
        with patch.object(config, 'INTEGRATION_MODE', 'native', create=True), \
             patch.object(recovery.isolated_native, 'evaluate_all', return_value=[{'installed': True, 'recovered': 0}, {'installed':False}]) as evaluate:
            self.assertEqual(recovery.install(), {'installed': True, 'windows': 1, 'recovered': 0})
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
