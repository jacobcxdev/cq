
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
