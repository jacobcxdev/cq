(async()=>{const bus=(await import(BUS_ASSET)).BUS_EXPORT;
const host=window.electronBridge?.getSharedObjectSnapshotValue?.('host_config')?.id??'local';
if(host!=='local')throw Error('Board requires local app-server');
const method=BOARD_METHOD,params=BOARD_PARAMS,id='cq-board-'+crypto.randomUUID();
return await new Promise(resolve=>{let off;const timer=setTimeout(()=>{off?.();resolve({timeout:true})},15000);
off=bus.subscribe('mcp-response',e=>{if(e.hostId!==host||e.message?.id!==id)return;
clearTimeout(timer);off();if(e.message.error){resolve({error:true});return;}
const r=e.message.result;
if(method==='thread/read'||method==='thread/resume'){resolve({result:{thread:r?.thread?{id:r.thread.id,status:r.thread.status}:null}});return;}
if(method==='turn/start'){resolve({result:{turn:r?.turn?{id:r.turn.id,status:r.turn.status}:null}});return;}
resolve({result:{data:(r?.data??[]).filter(x=>['userMessage','agentMessage'].includes(x.item?.type)).map(x=>({item:{type:x.item.type,text:x.item.text?.slice(0,1000),content:x.item.content?.filter(c=>c.type==='text').map(c=>({type:'text',text:c.text?.slice(0,1000)}))}})),nextCursor:r?.nextCursor}});
});bus.dispatchMessage('mcp-request',{hostId:host,request:{id,method,params},priority:'background',source:'thread',timeoutMs:15000});});})()
