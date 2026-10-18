(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const api=m.Ymr;const expected=%s;
const primary=await api.safeGet('/tbo/primary');
if(primary.profile?.id!==%s||primary.selection?.thread_id!==%s)throw Error('Native principal changed');
const p=await api.safeGet('/tbo/by-thread/'+expected.root);
if(p.id!==expected.profile||p.active_root_thread_id!==expected.root||typeof p.messaging_room_id!=='string')throw Error('Target changed');
const headers={'X-OpenAI-Attach-DeviceCheck-Token':'1'};
const challenge=await api.safeGet('/ios/attestation_challenge',{additionalHeaders:headers});
if(typeof challenge.attestation_challenge!=='string'||!challenge.attestation_challenge)throw Error('Native proof missing');
try{const sent=await api.safePost('/messaging/rooms/{room_id}/messages',{parameters:{path:{room_id:p.messaging_room_id}},additionalHeaders:headers,retry:false,requestBody:{app_attest_challenge:challenge.attestation_challenge,content:{text:expected.text},request_id:expected.request,idempotency_token:expected.request,timezone_offset_min:new Date().getTimezoneOffset()}});
return {accepted:true,message_id:sent.id,text_matches:sent.content?.text===expected.text};}
catch(e){return {accepted:false,status:e.status??null,error_code:e.code??null};}})()