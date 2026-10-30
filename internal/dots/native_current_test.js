const assert=require('node:assert/strict');
const {fixture,sources}=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{promise,resolve,reject};};
const classes=new Function('FUt','Ms','wo','mWt','UUt','Yg','HUt',`let gWt,_Wt;${fixture.cache_classes};return gWt;`);
const abortable=(promise,signal)=>new Promise((resolve,reject)=>{
    const abort=()=>reject(signal.reason);
    signal.addEventListener('abort',abort,{once:true});
    promise.then(resolve,reject).finally(()=>signal.removeEventListener('abort',abort));
});
const Cache=classes(callbacks=>{callbacks.onSubscribedChange(true);return()=>{};},deferred,abortable,class{},
    class extends DOMException{constructor(){super('superseded','AbortError');}},Error,()=>false);
const namespaceFor=new Function('Cache','api','executor','data',`
const Q={},x_=Symbol(),ZWt=Symbol(),JWt=Symbol(),IC=Symbol();
const U=(_,read)=>({read});let S_,$,iv,gWt=Cache;
${fixture.account_atoms};${fixture.ready_atom};
const Qi={tppLocalExecutor:executor};${fixture.executor_getter}
${fixture.cache_factory}
const auth={authenticatedAccountId:'account-one',userId:'user-one',authLoading:false,authMethod:'chatgpt'};
const state={cache:null,dispose(){this.cache?.dispose();this.cache=null;}};
let enabled=true;
const scope={watch:()=>()=>{},get(key){
 if(key?.read)return key.read({get:key=>scope.get(key)});
 if(key===x_)return auth;if(key===ZWt)return state;
 if(key===JWt)return{data,roomMetadata:new Map()};if(key===IC)return enabled;
}};
const M_=api;
return{namespace:{${Object.entries(fixture.exports).map(([alias,local])=>`${alias}:${local}`).join(',')}},
 scope,auth,state,disable(){enabled=false;}};
`);
const evaluate=(source,namespace)=>eval(source.replace(/import\([^)]*\)/,'Promise.resolve(namespace)'));
const mounted=scope=>{globalThis.document={getElementById:()=>({__reactContainer$fixture:{current:{memoizedProps:scope}}})};};
function context(){
    const rooms=new Map();
    const data={room:id=>rooms.get(id),retain:()=>()=>{},canEvict:()=>false,forgetRoom:id=>rooms.delete(id),reset:()=>rooms.clear()};
    const api={safeGet:async path=>{
        if(path==='/tbo')return{items:[{id:'profile-one',messaging_room_id:'room-one'}]};
        assert.equal(path,'/tbo/primary');return{profile:{id:'profile-one'}};
    }};
    const executor={getState:async()=>({ready:true,enabled:true,environmentId:'environment-one'})};
    return{...namespaceFor(Cache,api,executor,data),data,rooms,api};
}
function nativeSupplier(cache,controller,fetch,ingest){
    // Both functions below are the actual current native API implementation.
    return new Function('cache','controller','fetch','ingest',`
const e={},t='account-one',ie='ChatGPT-Account-ID',s=controller.signal,x={signal:cache.signal};
const N=()=>cache.assertCurrent(),P=()=>{s.throwIfAborted();N();},M=()=>true;
const W={safeGet:fetch},C={transaction:fn=>fn()},de=(_,value)=>ingest(value);
class Je extends Error{};const er=Error,G=Error,Be=Error,m=()=>null;
let ${fixture.request_wrapper};
{const t='room-one',r={},i={};${fixture.room_supplier};return a;}
`)(cache,controller,fetch,ingest);
}
(async()=>{
    let c=context();mounted(c.scope);
    const cache=c.namespace.g2n(c.scope),original=Cache.prototype.read;
    const installed=await evaluate(sources.cache,c.namespace);
    assert.equal(installed.nativeOwnership,true);
    assert.equal(Cache.prototype.read,original,'current cache must retain its native producer and ingestion');
    assert.deepEqual(await evaluate(sources.executor,c.namespace),{ready:true,enabled:true,environment_id:'environment-one'});
    const originalFactory=c.namespace.g2n;
    c.namespace.g2n=()=>({read(){},readRoom(){},readHistory(){}});
    await assert.rejects(()=>evaluate(sources.cache,c.namespace),/read contract changed/);
    c.namespace.g2n=originalFactory;
    // Rejected catalogue and missing authentication must not install a cache.
    c.api.safeGet=async()=>({items:[{id:'foreign',messaging_room_id:'room-one'}]});
    await assert.rejects(()=>evaluate(sources.cache,c.namespace),/catalogue identity/);
    c.auth.authenticatedAccountId=null;
    await assert.rejects(()=>evaluate(sources.cache,c.namespace),/scope unavailable/);
    assert.equal(c.namespace.g2n(c.scope),null);

    // Applying the old four-argument guard to this actual six-argument cache
    // makes each producer a distinct key and loses shared request deduplication.
    const baseline=context();mounted(baseline.scope);
    await evaluate(sources.cache.replace('CQNativeOwnedReads:true','CQNativeOwnedReads:false'),baseline.namespace);
    const brokenCache=baseline.namespace.g2n(baseline.scope),baselineGate=deferred();let baselineReads=0;
    const baselineProducer=async()=>{baselineReads++;await baselineGate.promise;};
    const baselineReadsPending=[brokenCache.readRoom('room-one',baselineProducer),brokenCache.readRoom('room-one',baselineProducer)];
    assert.equal(baselineReads,2,'legacy incompatibility must be reproduced');baselineGate.resolve();
    await Promise.all(baselineReadsPending);
    globalThis[Symbol.for('cq.dot-read-ownership.v2')].restore();
    assert.equal(Cache.prototype.read,original);

    // Actual six-argument cache deduplicates reads without borrowing the first
    // controller's lifetime, and native ingestion runs before the active read.
    c=context();const activeCache=c.namespace.g2n(c.scope);
    const first=new AbortController(),second=new AbortController(),gate=deferred();let requests=0;
    const supplier=nativeSupplier(activeCache,first,async(path,options)=>{
        requests++;assert.equal(path,'/messaging/rooms/{room_id}');
        assert.equal(options.additionalHeaders['ChatGPT-Account-ID'],'account-one');
        await gate.promise;options.signal.throwIfAborted();return{id:'room-one'};
    },value=>c.rooms.set(value.id,value));
    const stale=activeCache.readRoom('room-one',supplier,first.signal).catch(error=>error);
    const active=activeCache.readRoom('room-one',()=>assert.fail('read was not shared'),second.signal);
    first.abort();gate.resolve();
    assert.equal((await stale).name,'AbortError');await active;
    assert.equal(requests,1);assert.equal(activeCache.snapshot('room-one').room.id,'room-one');
    await activeCache.readRoom('room-one',()=>assert.fail('fresh native room was discarded'));

    // Query keys and page ingestion remain distinct. Replacing the callback
    // with the legacy raw GET would erase cursors and the stored tail page.
    let historyReads=0;const historyGate=deferred();
    const page={messageIds:['message-one'],rawMessageIds:['message-one'],before:'older',after:'newer',authoritativeCursors:true};
    c.data.messages=(_,ids)=>ids.map(id=>({id}));
    const history=async(signal,isCurrent)=>{historyReads++;await historyGate.promise;signal.throwIfAborted();assert.equal(isCurrent(),true);return page;};
    const oldHistory=new AbortController();
    const oldRead=activeCache.readHistory('room-one',{limit:20},history,{signal:oldHistory.signal,retainFresh:true}).catch(error=>error);
    const currentRead=activeCache.readHistory('room-one',{limit:20},()=>assert.fail('history was not shared'),{retainFresh:true});
    oldHistory.abort();historyGate.resolve();
    assert.equal((await oldRead).name,'AbortError');assert.deepEqual(await currentRead,page);
    assert.equal(historyReads,1);assert.equal(activeCache.snapshot('room-one').page.before,'older');
    await activeCache.readHistory('room-one',{before:'older'},async()=>{historyReads++;return{...page,before:'oldest'};});
    assert.equal(historyReads,2,'different history query reused the wrong page');
    activeCache.invalidateRoom('room-one');
    await activeCache.readRoom('room-one',async()=>{requests++;});assert.equal(requests,2);
    c.disable();assert.equal(c.namespace.g2n(c.scope),null,'changed access must dispose current cache');

    // Actual exported singleton class supplies the native request/response bus.
    let Vg;
    const Lg={postMessage:value=>{
        assert.equal(value.type,'mcp-request');assert.equal(value.hostId,'local');assert.equal(value.request.method,'thread/read');assert.equal(value.request.params.includeTurns,false);
        queueMicrotask(()=>Vg.dispatchHostMessage({type:'mcp-response',hostId:'local',message:{id:value.request.id,result:{thread:{id:value.request.params.threadId,status:{type:'idle'}}}}}));
    }};
    Vg=new Function('Lg',`let Bg;const Die=new Set(['mcp-response']);${fixture.bus_class};return Bg.getInstance();`)(Lg);
    globalThis.window={electronBridge:{getSharedObjectSnapshotValue:()=>({id:'local'})}};
    const shared=Object.fromEntries(Object.entries(fixture.bus_export).map(([alias,local])=>{assert.equal(local,'Vg');return[alias,Vg];}));
    assert.equal((await evaluate(sources.board,shared)).result.thread.status.type,'idle');
    assert.equal(Vg.handlers.has('mcp-response'),false,'board must release its native subscription');
})().catch(error=>{console.error(error);process.exitCode=1;});
