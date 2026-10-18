(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');const p=await m.Ymr.safeGet('/tbo/by-thread/'+ROOT_THREAD);
if(p.id!==PROFILE_ID||p.active_root_thread_id!==ROOT_THREAD)throw Error('Target changed');
const room=await m.Ymr.safeGet('/messaging/rooms/{room_id}',{parameters:{path:{room_id:p.messaging_room_id}}});
const actors=room.members.filter(x=>x.aeon_id===p.id);if(actors.length!==1)throw Error('Dot actor ambiguous');
const v=await m.Ymr.safeGet('/messaging/rooms/{room_id}/messages',{parameters:{path:{room_id:p.messaging_room_id},query:{limit:20}}});
const pattern=new RegExp(RELAY_PATTERN);
return (v.items??[]).filter(x=>x.account_user_id===actors[0].account_user_id&&pattern.test(x.content?.text??'')).map(x=>({id:x.id,text:x.content.text}));})()
