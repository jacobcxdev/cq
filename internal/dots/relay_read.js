(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');const p=await m.Ymr.safeGet('/tbo/{tbo_id}',{parameters:{path:{tbo_id:PROFILE_ID}}});
if(p.id!==PROFILE_ID||typeof p.active_root_thread_id!=='string'||!p.active_root_thread_id||typeof p.messaging_room_id!=='string'||!p.messaging_room_id)throw Error('Target changed');
const room=await m.Ymr.safeGet('/messaging/rooms/{room_id}',{parameters:{path:{room_id:p.messaging_room_id}}});
const actors=room.members.filter(x=>x.aeon_id===p.id);if(actors.length!==1)throw Error('Dot actor ambiguous');
const v=await m.Ymr.safeGet('/messaging/rooms/{room_id}/messages',{parameters:{path:{room_id:p.messaging_room_id},query:{limit:20}}});
const pattern=new RegExp(RELAY_PATTERN);
return (v.items??[]).filter(x=>x.account_user_id===actors[0].account_user_id&&pattern.test(x.content?.text??'')).map(x=>({id:x.id,text:x.content.text}));})()
