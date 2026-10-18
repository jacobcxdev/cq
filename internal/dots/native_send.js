(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const api=m.Ymr;const expected=%s;
const primary=await api.safeGet('/tbo/primary');
if(primary.profile?.id!==%s||typeof primary.profile?.active_root_thread_id!=='string'||!primary.profile.active_root_thread_id||primary.selection?.thread_id!==primary.profile.active_root_thread_id)throw Error('Native principal changed');
const p=await api.safeGet('/tbo/{tbo_id}',{parameters:{path:{tbo_id:expected.profile}}});
if(p.id!==expected.profile||typeof p.active_root_thread_id!=='string'||!p.active_root_thread_id||typeof p.messaging_room_id!=='string'||!p.messaging_room_id)throw Error('Target changed');
const headers={'X-OpenAI-Attach-DeviceCheck-Token':'1'};
const challenge=await api.safeGet('/ios/attestation_challenge',{additionalHeaders:headers});
if(typeof challenge.attestation_challenge!=='string'||!challenge.attestation_challenge)throw Error('Native proof missing');
try{const sent=await api.safePost('/messaging/rooms/{room_id}/messages',{parameters:{path:{room_id:p.messaging_room_id}},additionalHeaders:headers,retry:false,requestBody:{app_attest_challenge:challenge.attestation_challenge,content:{text:expected.text},request_id:expected.request,idempotency_token:expected.request,timezone_offset_min:new Date().getTimezoneOffset()}});
return {accepted:true,message_id:sent.id,text_matches:sent.content?.text===expected.text};}
catch(e){return {accepted:false,status:e.status??null,error_code:e.code??null};}})()
