#!/usr/bin/env python3
"""Task-only read-only adapter for stock Codex's explicit CLI override seam."""
import http.client, json, os, pathlib, ssl, subprocess, sys, threading, time, urllib.parse
from websockets.sync.client import connect
import catalog
import settings

ROOT = settings.ROOT
REAL = str(settings.APP / 'Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex')
APP_MARKER = 'plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true'
THREAD_OWNERS = {root: settings.DOT_OWNERS[name] for name, (_, root) in settings.DOTS.items()}
THREAD_OWNER_LOCK=threading.Lock()
LOCAL_READS = {'account/read','getAuthStatus','configRequirements/read','config/read'}
LOCAL_READS |= {'project/list','experimentalFeature/list','remoteControl/status/read','fs/readFile',
                'plugin/installed','skills/list','threadSection/list','account/gatewayOAuth/read',
                'mcpServerStatus/list','plugin/list'}
CLOUD_READS = {'thread/resume','thread/read','thread/list','thread/loaded/list',
               'thread/turns/list','thread/items/list','model/list','collaborationMode/list',
               'account/rateLimits/read','thread/realtime/listVoices'}
WRITE_LOCK = threading.Lock()
EVENT_LOCK = threading.Lock()
PENDING_LOCK = threading.Lock()
PENDING = {}

def event(value):
    with EVENT_LOCK:
        with (ROOT/'cloud-stdio-events.jsonl').open('a') as f:
            f.write(json.dumps({'at':time.time(),'pid':os.getpid(),**value})+'\n')

def emit(value):
    with WRITE_LOCK:
        sys.stdout.write(json.dumps(value)+'\n'); sys.stdout.flush()

def thread_id(value):
    if not isinstance(value,dict): return None
    if isinstance(value.get('threadId'),str): return value['threadId']
    thread=value.get('thread')
    return thread.get('id') if isinstance(thread,dict) else None

def owner_http_headers(owner):
    return {'Authorization':'Bearer '+owner['token'],
            'ChatGPT-Account-Id':owner['account_id'],'X-OpenAI-Product-Sku':'aeon',
            'Content-Type':'application/json','Accept':'application/json',
            'User-Agent':f'Codex Desktop/{settings.APP_VERSION} (Mac OS; arm64)',
            'originator':'codex_desktop'}

def refresh_thread_owners(owners):
    roots=catalog.primary_roots(owners,owner_http_headers(owners[settings.SYSTEM_ALIAS]))
    with THREAD_OWNER_LOCK:
        if any(root in THREAD_OWNERS and THREAD_OWNERS[root]!=pair[0] for root,pair in roots.items()):
            raise ValueError('Thread owner changed during refresh')
        THREAD_OWNERS.update({root:pair[0] for root,pair in roots.items()})
    event({'owner_primary_roots_refreshed':True,'verified_current_roots':len(roots)})

def resolve_thread_owner(resource,owners):
    if resource is None:return None
    with THREAD_OWNER_LOCK:alias=THREAD_OWNERS.get(resource)
    if alias is None:
        refresh_thread_owners(owners)
        with THREAD_OWNER_LOCK:alias=THREAD_OWNERS.get(resource)
    return alias

def owner_thread_status(owner, resource, public_tls):
    with THREAD_OWNER_LOCK:
        if THREAD_OWNERS.get(resource) != owner['alias']:
            raise ValueError('Status resource owner mismatch')
    if catalog.claims(owner['token'])['exp'] <= time.time() + 60:
        raise ValueError('Status owner credential expired')
    connection=http.client.HTTPSConnection('codex-cloud-backend.chatgpt.com',context=public_tls,timeout=10)
    try:
        connection.request('GET','/v1/threads/'+urllib.parse.quote(resource,safe=''),
                           headers=owner_http_headers(owner))
        response=connection.getresponse()
        if response.status != 200 or not response.getheader('content-type','').startswith('application/json'):
            raise ValueError('Owner metadata HTTP read failed')
        data=response.read(1024*1024+1)
        if len(data)>1024*1024: raise ValueError('Owner metadata exceeds bound')
        value=json.loads(data)
        if thread_id(value) != resource: raise ValueError('Owner metadata resource mismatch')
        status=value['thread']['status']
        if not isinstance(status,dict) or status.get('type') not in {'notLoaded','idle','active','systemError'}:
            raise ValueError('Owner metadata status invalid')
        if status['type']=='active' and (not isinstance(status.get('activeFlags'),list) or
                                       not all(isinstance(flag,str) for flag in status['activeFlags'])):
            raise ValueError('Owner metadata active flags invalid')
        return status
    finally: connection.close()

def select_owner(method, params, primary):
    identifier=thread_id(params)
    if identifier is not None:
        with THREAD_OWNER_LOCK:
            if identifier not in THREAD_OWNERS:
                raise ValueError('Thread outside verified proof catalog')
            return THREAD_OWNERS[identifier],identifier
    return primary,None

def fail(request, message):
    if 'id' in request:
        emit({'id':request['id'],'error':{'code':-32000,'message':message}})

def main():
    arguments=sys.argv[1:]
    env=dict(os.environ); env['CODEX_CLI_PATH']=REAL
    if 'app-server' not in arguments or APP_MARKER not in arguments:
        os.execve(REAL,[REAL,*arguments],env)
    event({'app_stdio_adapter_started':True,'read_only':True})
    system,accounts=catalog.inventory()
    owners={a['alias']:a for a in accounts if a['alias'] in {settings.SYSTEM_ALIAS,settings.SECONDARY_ALIAS}}
    if system['alias']!=settings.SYSTEM_ALIAS or len(owners)!=2:
        raise ValueError('Frozen proof owner identity changed')
    refresh_thread_owners(owners)
    local=subprocess.Popen([REAL,*arguments],env=env,stdin=subprocess.PIPE,
                           stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True,bufsize=1)
    sessions={}
    initialized=False
    stopping=threading.Event()
    status_watches=set()
    status_seen={}
    status_lock=threading.Lock()
    public_ca_file=ssl.get_default_verify_paths().openssl_cafile
    if not pathlib.Path(public_ca_file).is_file():
        raise ValueError('Public upstream CA roots unavailable')
    public_tls=ssl.create_default_context(cafile=public_ca_file)

    def http_read(alias,request,resource):
        method=request['method']; owner=owners[alias]
        endpoint={'thread/read':f'/v1/threads/{urllib.parse.quote(resource,safe="")}',
                  'thread/turns/list':f'/v2/threads/{urllib.parse.quote(resource,safe="")}/turns',
                  'thread/items/list':f'/v2/threads/{urllib.parse.quote(resource,safe="")}/items'}[method]
        query={k:v for k,v in request.get('params',{}).items() if k not in {'threadId','includeTurns'} and v is not None}
        if query: endpoint+='?'+urllib.parse.urlencode(query)
        connection=http.client.HTTPSConnection('codex-cloud-backend.chatgpt.com',context=public_tls,timeout=25)
        try:
            connection.request('GET',endpoint,headers=owner_http_headers(owner))
            response=connection.getresponse(); data=response.read(16*1024*1024+1)
            if len(data)>16*1024*1024: raise ValueError('Cloud read response exceeds bound')
            event({'owner_alias':alias,'method':method,'http_status':response.status,
                   'json_content_type':response.getheader('content-type','').startswith('application/json')})
            if response.status!=200:
                emit({'id':request['id'],'error':{'code':-32000,'message':'Owner cloud HTTP read failed'}})
            else:
                value=json.loads(data)
                if method=='thread/read':
                    if thread_id(value)!=resource: raise ValueError('Cloud HTTP resource mismatch')
                    event({'owner_alias':alias,'method':method,
                           'root_status_type':value['thread'].get('status',{}).get('type')})
                    value['thread']['turns']=[]
                emit({'id':request['id'],'result':value})
        except Exception as error:
            event({'owner_alias':alias,'method':method,'http_failure_type':type(error).__name__})
            fail(request,'Owner cloud HTTP read unavailable')
        finally: connection.close()

    def watch_owner_status(alias, resource):
        try:
            while not stopping.is_set():
                try:
                    status=owner_thread_status(owners[alias],resource,public_tls)
                    with status_lock:
                        changed=status_seen.get(resource)!=status
                        status_seen[resource]=status
                    if changed and not stopping.is_set():
                        emit({'method':'thread/status/changed','params':{'threadId':resource,'status':status}})
                        event({'owner_alias':alias,'owner_status_refresh':True,
                               'root_status_type':status['type'],
                               'active_flags_count':len(status.get('activeFlags',[]))})
                except Exception as error:
                    event({'owner_alias':alias,'status_refresh_failure_type':type(error).__name__})
                if stopping.wait(2): break
        except Exception as error:
            event({'owner_alias':alias,'status_watch_failure_type':type(error).__name__})

    def receive(source, stream):
        try:
            while True:
                raw=stream.readline() if source=='local' else stream.recv()
                if not raw: break
                value=json.loads(raw)
                if 'id' in value:
                    identifier=str(value['id'])
                    if identifier.startswith('cq-internal-'): continue
                    with PENDING_LOCK: expected=PENDING.pop((source,identifier),None)
                    if expected is None: continue
                    method,resource=expected
                    result=value.get('result',{})
                    returned=thread_id(result)
                    if resource is not None and returned is not None and returned!=resource:
                        emit({'id':value['id'],'error':{'code':-32000,'message':'Cloud response resource mismatch'}})
                        event({'owner_alias':source,'method':method,'resource_mismatch':True})
                        continue
                    event({'owner_alias':source,'method':method,'error_code':value.get('error',{}).get('code'),
                           'resume_thread_matches':returned==resource if method=='thread/resume' else None})
                    emit(value)
                    if method=='thread/resume' and source in owners and source!=system['alias'] and resource is not None and 'error' not in value:
                        with status_lock:
                            start=resource not in status_watches
                            status_watches.add(resource)
                        if start: threading.Thread(target=watch_owner_status,args=(source,resource),daemon=True).start()
                elif source=='local' or source==system['alias']:
                    emit(value)
                elif source!='local':
                    resource=thread_id(value.get('params',{}))
                    with THREAD_OWNER_LOCK:resource_owner=THREAD_OWNERS.get(resource)
                    if resource is not None and resource_owner==source:
                        if value.get('method')=='thread/status/changed':
                            with status_lock: status_seen[resource]=value['params'].get('status')
                        emit(value)
        except Exception as error:
            event({'owner_alias':source,'receive_failure_type':type(error).__name__})

    def send(source, request, resource=None):
        if 'id' in request:
            with PENDING_LOCK: PENDING[(source,str(request['id']))]=(request.get('method'),resource)
        raw=json.dumps(request)
        if source=='local':
            local.stdin.write(raw+'\n'); local.stdin.flush()
        else: sessions[source].send(raw)

    threading.Thread(target=receive,args=('local',local.stdout),daemon=True).start()
    try:
        for raw in sys.stdin:
            if len(raw)>4*1024*1024: raise ValueError('Native request exceeds frame bound')
            request=json.loads(raw); method=request.get('method')
            if method=='initialize' and not initialized:
                send('local',request)
                for alias,owner in owners.items():
                    ws=connect('wss://codex-cloud-backend.chatgpt.com/',
                        subprotocols=['codex-app-server','codex-client.desktop','openai-bearer.'+owner['token']],
                        additional_headers={'ChatGPT-Account-Id':owner['account_id'],'X-OpenAI-Product-Sku':'codex',
                                            'X-OpenAI-Allow-Environmentless-Fallback':'true'},
                        proxy=None,ssl=public_tls,open_timeout=20,max_size=16*1024*1024,legacy=True)
                    sessions[alias]=ws
                    internal={**request,'id':'cq-internal-initialize-'+alias}
                    ws.send(json.dumps(internal))
                    while True:
                        response=json.loads(ws.recv(timeout=30))
                        if response.get('id')==internal['id']: break
                    if 'error' in response: raise ValueError('Cloud initialize rejected')
                    event({'owner_alias':alias,'upgrade_status':ws.response.status_code,'initialize_success':True})
                    threading.Thread(target=receive,args=(alias,ws),daemon=True).start()
                initialized=True
            elif method=='initialized':
                for alias in sessions: send(alias,request)
                send('local',request)
            elif method in LOCAL_READS:
                event({'routed_method':method,'owner_alias':'local'})
                send('local',request)
            elif method in CLOUD_READS:
                try:
                    resource=thread_id(request.get('params',{}))
                    if resolve_thread_owner(resource,owners) is None:
                        send('local',request)
                        continue
                    alias,resource=select_owner(method,request.get('params',{}),system['alias'])
                    event({'routed_method':method,'owner_alias':alias})
                    if method in {'thread/read','thread/turns/list','thread/items/list'}:
                        threading.Thread(target=http_read,args=(alias,request,resource),daemon=True).start()
                    else: send(alias,request,resource)
                except Exception as error:
                    event({'owner_routing_failure_type':type(error).__name__})
                    fail(request,'Owner cloud routing verification unavailable')
            else:
                event({'blocked_method':method})
                fail(request,'Read-only cloud-session proof blocks this operation')
    finally:
        stopping.set()
        for ws in sessions.values(): ws.close()
        local.terminate()
        try: local.wait(timeout=5)
        except subprocess.TimeoutExpired: local.kill()

if __name__=='__main__':
    try: main()
    except Exception as error:
        event({'adapter_failure_type':type(error).__name__})
        sys.exit(1)
