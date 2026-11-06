(async()=>{
const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const root=document.getElementById('root'),key=root&&Object.keys(root).find(k=>k.startsWith('__reactContainer$'));
const container=key&&root[key],fibres=[container?.current??container],seen=new Set();let scope;
function inspect(value,depth){
if(scope||!value||typeof value!=='object'||seen.has(value))return;seen.add(value);
try{if(typeof value.get==='function'&&typeof value.watch==='function'&&typeof value.get(m.GMr)==='string'&&typeof value.get(m.XMr)==='string'&&value.get(m.JMr)){scope=value;return;}
if(depth>0)for(const field of Object.values(Object.getOwnPropertyDescriptors(value)))if('value'in field)inspect(field.value,depth-1);}catch{}
}
for(let i=0;i<fibres.length&&i<10000&&!scope;i++){const fibre=fibres[i];if(!fibre)continue;inspect(fibre.memoizedProps,3);let hook=fibre.memoizedState;for(let j=0;hook&&j<100&&!scope;j++,hook=hook.next)inspect(hook.memoizedState,3);if(fibre.child)fibres.push(fibre.child);if(fibre.sibling)fibres.push(fibre.sibling);}
const client=scope?.queryClient;
if(!scope||typeof client?.getQueryData!=='function'||typeof client?.setQueryData!=='function'||typeof client?.getQueryCache!=='function')return{refreshed:false};
const cache=client.getQueryCache();if(typeof cache?.subscribe!=='function')return{refreshed:false};
const account=scope.get(m.GMr),user=scope.get(m.XMr),queryKey=['cloud-aeon-profiles',account,user];
const symbol=Symbol.for('cq.dot-sidebar-order.v1');let state=globalThis[symbol];
if(state&&(state.client!==client||state.account!==account||state.user!==user)){state.stop?.();delete globalThis[symbol];state=null;}
if(!state){
state={client,account,user,expected:[],applying:false};
state.apply=()=>{
if(scope.get(m.GMr)!==account||scope.get(m.XMr)!==user){state.stop?.();return false;}
const rows=client.getQueryData(queryKey);if(!Array.isArray(rows)||rows.some(p=>!p||typeof p.id!=='string'))return false;
const rank=new Map(state.expected.map((id,index)=>[id,index]));
const sorted=[...rows].sort((a,b)=>(rank.get(a.id)??Number.MAX_SAFE_INTEGER)-(rank.get(b.id)??Number.MAX_SAFE_INTEGER));
if(sorted.some((p,index)=>p!==rows[index])&&!state.applying){state.applying=true;try{client.setQueryData(queryKey,sorted);}finally{state.applying=false;}}
return state.expected.every((id,index)=>sorted[index]?.id===id);
};
state.stop=cache.subscribe(event=>{const key=event?.query?.queryKey;if(!state.applying&&Array.isArray(key)&&key.length===3&&key.every((part,index)=>part===queryKey[index]))state.apply();});
globalThis[symbol]=state;
}
state.expected=ORDERED_PROFILE_IDS;
return{refreshed:true,order_applied:state.apply()};
})()
