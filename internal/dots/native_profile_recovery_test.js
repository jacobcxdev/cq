const assert=require('node:assert/strict');
const source=require('node:fs').readFileSync(0,'utf8');
const profileQuery=Symbol('profile-query');
const storageKey='cloud-aeon-profiles-v1:["account-one","user-one"]';
function fixture() {
    const auth={account:'account-one',user:'user-one'};
    const profiles=['one','two'].map(id=>({id:'profile-'+id,status:'active',messaging_room_id:'room-'+id}));
    const records=Object.fromEntries(profiles.map(p=>[p.id,{status:'available',profile:p}]));
    const reads=[],invalidated=[];
    const cache={read(a,b,c,d,e,f){},readRoom(){},readHistory(){}};
    const c={auth,profiles,records,reads,invalidated,fail:new Set(),missing:new Set(),cached:new Map(),queryStates:new Map()};
    const scope={watch(){},get(atom){
        if(atom==='account')return auth.account;
        if(atom==='user')return auth.user;
        if(atom==='ready')return true;
        if(atom===profileQuery)return c.queryStates.get(arguments[1])??{};
    },query:{
        async invalidate(query,id,filters){
            assert.equal(query,profileQuery);
            assert.equal(filters.refetchType,'none','invalidation must not start or cancel foreground reads');
            invalidated.push(id);c.cached.delete(id);
        },
        async getOrFetch(query,id){
            assert.equal(query,profileQuery);
            if(c.cached.has(id))return c.cached.get(id);
            reads.push(id);
            if(c.fail.has(id)){
                c.queryStates.set(id,{errorUpdatedAt:Date.now()});
                throw Error('owner rejected access');
            }
            if(c.missing.has(id)){
                c.queryStates.set(id,{data:null,dataUpdatedAt:Date.now()});
                return null;
            }
            if(c.beforeRead)await c.beforeRead(id);
            // The native query owns persistence and account-switch protection.
            if(auth.account!=='account-one'||auth.user!=='user-one')throw Error('account changed');
            const profile=profiles.find(p=>p.id===id);
            records[id]={status:'available',profile};
            return profile;
        }
    }};
    c.namespace={_lr:{safeGet:async path=>{assert.equal(path,'/tbo');return{items:profiles};}},
        Hlr:'account',Ulr:'user',mcr:'ready',g2n:()=>cache,Nen:()=>null,
        Rcr:key=>{assert.equal(key,storageKey);return records;},K0n:profileQuery};
    globalThis.document={getElementById:()=>({__reactContainer$fixture:{current:{memoizedProps:scope}}})};
    c.run=()=>eval(source.replace(/import\([^)]*\)/,'Promise.resolve(c.namespace)'));
    return c;
}
(async()=>{
    // Healthy profiles need no additional owner requests.
    let c=fixture();await c.run();assert.deepEqual(c.reads,[]);

    // A catalogue hit alone cannot repair a sticky denial. Only the owner's
    // native detail query may replace it, including a fresh-but-stale query hit.
    c=fixture();c.records['profile-two']={status:'denied',revision:'old'};
    c.cached.set('profile-two',c.profiles[1]);
    await c.run();
    assert.deepEqual(c.reads,['profile-two']);
    assert.equal(c.records['profile-two'].status,'available');
    await c.run();assert.equal(c.reads.length,1,'healthy recovery was repeated');

    // A real access denial stays denied; one failure must not block another Dot.
    c=fixture();for(const p of c.profiles)c.records[p.id]={status:'denied'};
    c.fail.add('profile-one');await c.run();
    assert.equal(c.records['profile-one'].status,'denied');
    assert.equal(c.records['profile-two'].status,'available');
    await c.run();assert.equal(c.reads.length,2,'genuine denials must not retry every health-loop pass');
    c.queryStates.set('profile-one',{errorUpdatedAt:Date.now()-60001});c.fail.clear();
    await c.run();assert.equal(c.records['profile-one'].status,'available');

    // Native 404s become null successes, but need the same failure cooldown.
    c=fixture();c.records['profile-two']={status:'denied'};c.missing.add('profile-two');
    await c.run();await c.run();assert.equal(c.reads.length,1);
    assert.equal(c.records['profile-two'].status,'denied');
    c.queryStates.set('profile-two',{data:null,dataUpdatedAt:Date.now()-60001});c.missing.clear();
    await c.run();assert.equal(c.records['profile-two'].status,'available');

    // Explicit deletion, deletion in progress and foreign cached records stay put.
    for(const accepted of [false,true]){
        c=fixture();const deleted={status:'deleted',accepted,revision:'keep'};
        c.records['profile-two']=deleted;c.records.foreign={status:'denied'};
        await c.run();assert.deepEqual(c.reads,[]);assert.equal(c.records['profile-two'],deleted);
    }
    c=fixture();c.profiles[1].status='deleting';c.records['profile-two']={status:'denied'};
    await c.run();assert.deepEqual(c.reads,[]);

    // A changed identity must not clear the old account's denial.
    c=fixture();c.records['profile-two']={status:'denied'};
    c.beforeRead=async()=>{c.auth.account='other-account';};
    await c.run();assert.equal(c.records['profile-two'].status,'denied');

    // A foreign or incomplete aggregate never authorises recovery.
    c=fixture();c.records['profile-two']={status:'denied'};c.profiles[1].id='foreign';
    await assert.rejects(c.run,/catalogue identity/);assert.deepEqual(c.reads,[]);
})().catch(error=>{console.error(error);process.exitCode=1;});
