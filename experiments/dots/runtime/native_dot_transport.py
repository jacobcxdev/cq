"""Existing native Dot room requests, pinned to verified research resources."""
import json,uuid,time,pathlib
import isolated_native as native
import settings
ROOT=settings.ROOT
DOTS=settings.DOTS
SYSTEM_DOT=next((value for target,value in DOTS.items() if settings.DOT_OWNERS[target]==settings.SYSTEM_ALIAS),None)


def display_names():
    """Read current labels from the native catalogue; routing stays profile-bound."""
    expression = """(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const value=await m.Ymr.safeGet('/tbo');
return (value.items??[]).map(p=>({id:p.id,name:p.display_name}));})()"""
    rows = native.evaluate(expression.replace('MODULE_ASSET_PLACEHOLDER', settings.MODULE_ASSET))
    expected = {profile: handle for handle, (profile, _) in DOTS.items()}
    if (not isinstance(rows, list) or len(rows) != len(expected)
            or any(not isinstance(row, dict) or not isinstance(row.get('id'), str) for row in rows)
            or {row.get('id') for row in rows} != set(expected)):
        raise ValueError('Native catalogue identity changed')
    result = {}
    for row in rows:
        name = row.get('name')
        if not isinstance(name, str) or not 0 < len(name) <= 80 or any(ord(c) < 32 for c in name):
            raise ValueError('Native catalogue name invalid')
        result[expected[row['id']]] = name
    return result


def send(target,text,ledger):
    if SYSTEM_DOT is None: raise ValueError('System Dot unavailable')
    if target not in DOTS or not isinstance(text,str) or not 0<len(text)<=4000:
        raise ValueError('Research message outside bound')
    profile,root=DOTS[target]
    request_id=str(uuid.uuid4())
    summary={'at':time.time(),'target':target,'request_id':request_id,'outcome':'pending'}
    with ledger.open('x') as f:
        ledger.chmod(0o600);json.dump(summary,f)
    expression="""(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
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
catch(e){return {accepted:false,status:e.status??null,error_code:e.code??null};}})()"""%(
        json.dumps({'profile':profile,'root':root,'text':text,'request':request_id}),
        json.dumps(SYSTEM_DOT[0]),json.dumps(SYSTEM_DOT[1]))
    try:
        result=native.evaluate(expression.replace("MODULE_ASSET_PLACEHOLDER",settings.MODULE_ASSET))
        summary.update(result)
        summary['outcome']='accepted' if result.get('accepted') else 'rejected' if isinstance(result.get('status'),int) and 400<=result['status']<500 and result['status']!=408 else 'unknown'
    except Exception as error:
        summary.update({'outcome':'unknown','failure_type':type(error).__name__})
    ledger.write_text(json.dumps(summary,indent=2)+'\n')
    return summary

def matching_reply(target,marker,exclude_id=None):
    if SYSTEM_DOT is None: raise ValueError('System Dot unavailable')
    profile,root=DOTS[target]
    # Keep all conversation contents inside native renderer; return matching synthetic reply only.
    expression="""(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');const p=await m.Ymr.safeGet('/tbo/by-thread/'+%s);
if(p.id!==%s||p.active_root_thread_id!==%s)throw Error('Target changed');
const room=await m.Ymr.safeGet('/messaging/rooms/{room_id}',{parameters:{path:{room_id:p.messaging_room_id}}});const actors=room.members.filter(x=>x.aeon_id===p.id);if(actors.length!==1)throw Error('Dot actor ambiguous');const v=await m.Ymr.safeGet('/messaging/rooms/{room_id}/messages',{parameters:{path:{room_id:p.messaging_room_id},query:{limit:20}}});
return {keys:Object.keys(v),match:(v.items??[]).filter(x=>x.id!==%s&&x.account_user_id===actors[0].account_user_id&&(x.content?.text??'').includes(%s)).map(x=>({id:x.id,content:x.content,role:x.role,created_at:x.created_at}))};})()"""%(json.dumps(root),json.dumps(profile),json.dumps(root),json.dumps(exclude_id),json.dumps(marker))
    return native.evaluate(expression.replace("MODULE_ASSET_PLACEHOLDER",settings.MODULE_ASSET))
