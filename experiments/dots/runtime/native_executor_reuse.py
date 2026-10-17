"""Bind isolated native singleton to existing authorised secondary research registration."""
import hashlib,hmac,json,time,uuid
import local_executor as le
import catalog,settings

def validate_native(headers,body,system,proof,current_root):
    lower={k.lower():v for k,v in headers.items()}
    if (system.get('alias')!=settings.SYSTEM_ALIAS or lower.get('chatgpt-account-id')!=system['account_id']
            or not hmac.compare_digest(lower.get('authorization',''),'Bearer '+system['token'])
            or proof.get('owner_alias')!=le.ALIAS or proof.get('registration_accepted') is not True
            or proof.get('root_thread_id')!=current_root or not proof.get('environment_id')):
        raise ValueError('Isolated native executor identity mismatch')
    value=json.loads(body)
    uuid.UUID(value['installation_id'])
    if (set(value)!={'displayName','installation_id','selected_capability_roots'}
            or not isinstance(value['displayName'],str) or not isinstance(value['selected_capability_roots'],list)):
        raise ValueError('Native registration body invalid')
    return value

def reuse(headers,body):
    system,_=catalog.inventory()
    incoming={k.lower():v for k,v in headers.items()}
    authorization=incoming.get('authorization','')
    if incoming.get('chatgpt-account-id')!=system['account_id'] or not authorization.startswith('Bearer '):
        raise ValueError('Isolated native executor identity mismatch')
    native={**system,'token':authorization[7:]}
    selection=catalog.get(native,'/backend-api/tbo/primary',headers)
    profile=selection.get('profile',{})
    if profile.get('id')!=settings.PROOF_PROFILE_IDS[settings.SYSTEM_ALIAS] or selection.get('selection',{}).get('thread_id')!=next(v[1] for v in settings.DOTS.values() if v[0]==profile.get('id')):
        raise ValueError('Isolated native executor identity mismatch')
    system=native
    owner=le.null_credential();root=le.current_root(owner)
    proof=json.loads((le.ROOT/'proof.json').read_text())
    request=validate_native(headers,body,system,proof,root)
    # No account registration or capability expansion. Return genuine existing ID.
    status,content_type,data=le.owner_request(owner,le.REGISTRY,'GET',
        '/api/cloud/environment/'+proof['environment_id']+'/status')
    if status!=200 or not content_type.startswith('application/json'):
        raise ValueError('Existing research registration unavailable')
    if json.loads(data).get('environment_id')!=proof['environment_id']:
        raise ValueError('Research registration status identity mismatch')
    return {'executor_id':proof['environment_id']}, {'native_executor_reused':True,
        'owner_alias':le.ALIAS,'new_registration_sent':False,'capability_roots_granted':0,
        'native_proposed_roots':len(request['selected_capability_roots'])}
