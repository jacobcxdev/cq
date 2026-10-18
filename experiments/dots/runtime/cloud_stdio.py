#!/usr/bin/env python3
"""Owner-routed Dot reads through stock Codex's explicit CLI override seam."""
import http.client, json, os, pathlib, queue, ssl, subprocess, sys, threading, time, urllib.parse
from websockets.sync.client import connect
import catalog
import settings

ROOT = settings.ROOT
REAL = str(settings.APP / 'Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex')
APP_MARKER = 'plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true'
APP_MARKERS = {APP_MARKER, APP_MARKER.removesuffix('true')+'false'}
# Exact upstream WorkspaceRoutingError messages only; arbitrary backend errors may contain credentials.
WORKSPACE_ROUTING_ERRORS = {
    'failed to load workspace requirements':'requirements_load',
    'workspace routing requires a ChatGPT account id':'missing_account_id',
    'workspace routing discovery cancelled':'discovery_cancelled',
    'account changed during workspace routing discovery':'account_changed',
    'workspace routing discovery unauthorized (401)':'discovery_unauthorized',
    'workspace routing discovery failed':'discovery_failed',
    'selected workspace missing from routing discovery':'missing_workspace',
    'duplicate workspace in routing discovery':'duplicate_workspace',
    'failed to reload workspace requirements':'requirements_reload',
    'configuration changed during workspace routing discovery; retry account/read':'configuration_changed',
    'invalid model provider URL':'invalid_provider_url',
    'invalid ChatGPT backend URL':'invalid_bootstrap_url',
    'invalid session ChatGPT backend URL':'invalid_session_bootstrap_url',
    'ChatGPT backend changed; start a new thread before sending more content':'session_backend_changed',
    'workspace routing discovery cancelled during shutdown':'shutdown',
    'workspace routing discovery timed out':'discovery_timeout',
    'workspace routing discovery missing backend origin':'missing_backend_origin',
    'workspace routing discovery has invalid account routing override':'invalid_routing_override',
    'workspace routing discovery must return an origin':'backend_is_not_origin',
    'required ChatGPT backend conflicts with workspace routing':'backend_conflict',
    'invalid workspace backend URL':'invalid_backend_url',
    'workspace backend must use an HTTPS origin without credentials':'invalid_backend_origin',
}
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

def native_thread_owner(resource):
    if resource is None:return None
    with THREAD_OWNER_LOCK:
        alias=THREAD_OWNERS.get(resource)
    if alias is not None:return alias
    try:
        snapshot=json.loads(catalog.read_owned(ROOT/'native-roots.json',ROOT))
        expected=settings.PROOF_PROFILE_IDS
        if (not isinstance(snapshot,dict) or set(snapshot)!={'version','roots'}
                or type(snapshot['version']) is not int or snapshot['version']!=1
                or not isinstance(snapshot['roots'],list) or len(snapshot['roots'])!=len(expected)):
            raise ValueError('Native root snapshot invalid')
        roots={}; aliases=set()
        for row in snapshot['roots']:
            if not isinstance(row,dict) or set(row)!={'owner_alias','profile_id','root_thread_id'}:
                raise ValueError('Native root entry invalid')
            alias=row['owner_alias'];root=row['root_thread_id']
            if (not isinstance(alias,str) or alias not in expected or alias in aliases
                    or row['profile_id']!=expected[alias] or not isinstance(root,str)
                    or not root or len(root)>128 or root in roots):
                raise ValueError('Native root owner/profile mismatch')
            aliases.add(alias);roots[root]=alias
        with THREAD_OWNER_LOCK:
            if any(root in THREAD_OWNERS and THREAD_OWNERS[root]!=alias for root,alias in roots.items()):
                raise ValueError('Native root owner changed')
            # Keep previously verified roots for open chats; stale snapshots cannot remove them.
            THREAD_OWNERS.update(roots)
            return THREAD_OWNERS.get(resource)
    except FileNotFoundError:return None
    except Exception as error:
        event({'native_root_snapshot_failure_type':type(error).__name__})
        return None

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

class LocalBootstrap:
    """Seed only this app-server's process-local auth before native initialization."""
    def __init__(self, stream):
        self.stream=stream
        self.write_lock=threading.Lock()
        self.lock=threading.Lock()
        self.waiting=None
        self.responses=queue.Queue(maxsize=1)
        self.sequence=0

    def send(self, value):
        with self.write_lock:
            self.stream.write(json.dumps(value)+'\n')
            self.stream.flush()

    def accept(self, value):
        if value.get('method')=='account/chatgptAuthTokens/refresh':
            if 'id' in value:
                self.send({'id':value['id'],'error':{'code':-32000,
                           'message':'Credential refresh is disabled for this read-only session'}})
            return True
        with self.lock:
            if value.get('method') is None and self.waiting is not None and value.get('id')==self.waiting:
                if self.responses.empty(): self.responses.put_nowait(value)
                return True
        return False

    def closed(self):
        with self.lock:
            if self.waiting is not None and self.responses.empty():
                self.responses.put_nowait(None)

    def rpc(self, method, params, deadline):
        self.sequence+=1
        identifier='cq-internal-bootstrap-'+str(self.sequence)
        with self.lock: self.waiting=identifier
        try:
            self.send({'id':identifier,'method':method,'params':params})
            try: response=self.responses.get(timeout=max(0,deadline-time.monotonic()))
            except queue.Empty: raise ValueError('Local authentication bootstrap timed out') from None
            if response is None or 'error' in response or not isinstance(response.get('result'),dict):
                error = (response or {}).get('error', {})
                code = error.get('code') if isinstance(error,dict) else None
                message = error.get('message') if isinstance(error,dict) else None
                category = (WORKSPACE_ROUTING_ERRORS.get(message,'unclassified')
                            if method=='account/read' and isinstance(message,str) else 'unclassified')
                event({'bootstrap_rejected_method':method,
                       'bootstrap_error_code':code if type(code) is int else None,
                       'bootstrap_error_category':category})
                raise ValueError('Local authentication bootstrap failed')
            return response['result']
        finally:
            with self.lock: self.waiting=None

    def initialize(self, request, owner, timeout=60):
        deadline=time.monotonic()+timeout
        result=self.rpc('initialize',request.get('params',{}),deadline)
        self.send({'method':'initialized'})
        self.rpc('account/login/start',{'type':'chatgptAuthTokens',
                 'accessToken':owner['token'],'chatgptAccountId':owner['account_id']},deadline)
        account=self.rpc('account/read',{'refreshToken':False},deadline)
        routing=account.get('workspaceRouting')
        if (not isinstance(routing,dict) or routing.get('chatgptAccountId')!=owner['account_id']
                or routing.get('accountRoutingOverride') not in {'us','us_cr','NO_CONSTRAINT'}):
            raise ValueError('Local workspace routing verification failed')
        origin=urllib.parse.urlsplit(routing.get('backendOrigin',''))
        if (origin.scheme!='https' or not origin.hostname or origin.username is not None
                or origin.password is not None or origin.path not in {'','/'} or origin.query or origin.fragment):
            raise ValueError('Local workspace routing verification failed')
        policy=self.rpc('configRequirements/read',{},deadline)
        if 'requirements' not in policy or (policy['requirements'] is not None and not isinstance(policy['requirements'],dict)):
            raise ValueError('Local workspace policy verification failed')
        return result


def local_arguments(arguments):
    """CLI overrides win over file/profile config; remove conflicting CLI values."""
    clean=[]
    index=0
    while index<len(arguments):
        argument=arguments[index]
        if argument in {'-c','--config'} and index+1<len(arguments):
            value=arguments[index+1]
            if value.split('=',1)[0].strip()=='cli_auth_credentials_store':
                index+=2
                continue
        if any(argument.startswith(prefix) and argument[len(prefix):].split('=',1)[0].strip()=='cli_auth_credentials_store'
               for prefix in ['-c=','--config=','-c']):
            index+=1
            continue
        clean.append(argument)
        index+=1
    return [REAL,'-c','cli_auth_credentials_store="ephemeral"',*clean]


def native_arguments(arguments, environment):
    # This pinned CLI does not consume OPENAI_BASE_URL itself. Apply CQ's
    # launch environment through its process-local config override instead.
    base = environment.get('OPENAI_BASE_URL')
    if base is None:
        return arguments
    parsed = urllib.parse.urlsplit(base)
    if (parsed.scheme != 'http' or parsed.hostname != '127.0.0.1'
            or parsed.username is not None or parsed.password is not None
            or parsed.path not in {'', '/'} or parsed.query or parsed.fragment
            or parsed.port is None):
        raise ValueError('Native CQ model proxy must be a loopback HTTP endpoint')
    return [*arguments, '-c', 'openai_base_url=' + json.dumps(base)]


def main():
    arguments=sys.argv[1:]
    env=dict(os.environ); env['CODEX_CLI_PATH']=REAL
    if 'app-server' not in arguments:
        os.execve(REAL,[REAL,*arguments],env)
    # Desktop supplies this override even when the app-tools plugin is unavailable.
    app_session=any(marker in arguments for marker in APP_MARKERS)
    native=getattr(settings,'INTEGRATION_MODE','isolated')=='native'
    if native:
        arguments=native_arguments(arguments,env)
    if native and not app_session:
        os.execve(REAL,[REAL,*arguments],env)
    event({'app_stdio_adapter_started':True,'read_only':not native,'app_session':app_session,
           'integration_mode':'native' if native else 'isolated'})
    system={'alias':settings.SYSTEM_ALIAS}
    owners={}
    try:
        system,accounts=catalog.inventory()
        aliases=set(settings.DOT_OWNERS.values())|{settings.SYSTEM_ALIAS}
        owners={a['alias']:a for a in accounts if a['alias'] in aliases}
        if system['alias']!=settings.SYSTEM_ALIAS or set(owners)!=aliases or system['alias'] not in owners:
            raise ValueError('Frozen proof owner identity changed')
        if app_session and not native: refresh_thread_owners(owners)
    except Exception as error:
        if not native: raise
        owners={}
        event({'owner_inventory_failure_type':type(error).__name__})
    local=subprocess.Popen([REAL,*arguments] if native else local_arguments(arguments),env=env,stdin=subprocess.PIPE,
                           stdout=subprocess.PIPE,stderr=None if native else subprocess.DEVNULL,text=True,bufsize=1)
    bootstrap=LocalBootstrap(local.stdin)
    sessions={}
    session_locks={alias:threading.Lock() for alias in owners}
    sessions_lock=threading.Lock()
    native_queues={}
    native_workers=[]
    initialized=False
    native_initialize=None
    stopping=threading.Event()
    local_closed=threading.Event()
    status_watches=set()
    status_seen={}
    status_lock=threading.Lock()
    public_tls=None
    try:
        public_ca_file=ssl.get_default_verify_paths().openssl_cafile
        if not public_ca_file or not pathlib.Path(public_ca_file).is_file():
            raise ValueError('Public upstream CA roots unavailable')
        public_tls=ssl.create_default_context(cafile=public_ca_file)
    except Exception as error:
        if not native: raise
        event({'owner_tls_failure_type':type(error).__name__})

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
                if native and source=='local':
                    # Preserve stock responses, notifications and server-initiated RPCs,
                    # including authentication refresh and approval requests.
                    emit(value)
                    continue
                if source=='local' and bootstrap.accept(value): continue
                if 'id' in value:
                    identifier=str(value['id'])
                    if identifier.startswith('cq-internal-'): continue
                    key=(source,identifier,stream) if native and source!='local' else (source,identifier)
                    with PENDING_LOCK: expected=PENDING.pop(key,None)
                    if expected is None: continue
                    method,resource=expected[:2]
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
        finally:
            if source=='local':
                bootstrap.closed()
                if native:local_closed.set()
            elif native:
                with sessions_lock:
                    if sessions.get(source) is stream:del sessions[source]
                    with PENDING_LOCK:
                        lost=[(key,value) for key,value in PENDING.items()
                              if len(key)==3 and key[0]==source and key[2] is stream]
                        for key,_ in lost:del PENDING[key]
                if not stopping.is_set():
                    for _,expected in lost:
                        fail({'id':expected[2]},'Owner cloud session closed before responding')
                try:stream.close()
                except Exception as error:
                    event({'owner_alias':source,'close_failure_type':type(error).__name__})

    def send(source, request, resource=None):
        if native and source!='local':
            key=None
            with sessions_lock:
                ws=sessions.get(source)
                if ws is None:raise ValueError('Owner cloud session closed')
                if 'id' in request:
                    key=(source,str(request['id']),ws)
                    with PENDING_LOCK:PENDING[key]=(request.get('method'),resource,request['id'])
            try:ws.send(json.dumps(request))
            except Exception:
                if key is not None:
                    with PENDING_LOCK:expected=PENDING.pop(key,None)
                    if expected is None:return # The reader already delivered its response or close error.
                raise
            return
        if 'id' in request and not (native and source=='local'):
            with PENDING_LOCK: PENDING[(source,str(request['id']))]=(request.get('method'),resource)
        raw=json.dumps(request)
        if source=='local':
            bootstrap.send(request)
        else: sessions[source].send(raw)

    def open_owner_session(alias, request):
        with session_locks[alias]:
            with sessions_lock:
                if alias in sessions: return
            if public_tls is None: raise ValueError('Public upstream CA roots unavailable')
            owner=owners[alias]
            ws=connect('wss://codex-cloud-backend.chatgpt.com/',
                subprotocols=['codex-app-server','codex-client.desktop','openai-bearer.'+owner['token']],
                additional_headers={'ChatGPT-Account-Id':owner['account_id'],'X-OpenAI-Product-Sku':'codex',
                                    'X-OpenAI-Allow-Environmentless-Fallback':'true'},
                proxy=None,ssl=public_tls,open_timeout=20,max_size=16*1024*1024,legacy=True)
            try:
                internal={**request,'id':'cq-internal-initialize-'+alias}
                ws.send(json.dumps(internal))
                while True:
                    response=json.loads(ws.recv(timeout=30))
                    if response.get('id')==internal['id']: break
                if 'error' in response: raise ValueError('Cloud initialize rejected')
                if native: ws.send(json.dumps({'method':'initialized'}))
                with sessions_lock:
                    if stopping.is_set(): raise ValueError('Owner cloud session is shutting down')
                    sessions[alias]=ws
                event({'owner_alias':alias,'upgrade_status':ws.response.status_code,'initialize_success':True})
                threading.Thread(target=receive,args=(alias,ws),daemon=True).start()
            except Exception:
                ws.close()
                raise

    def native_owner_worker(alias, requests):
        while not stopping.is_set():
            try: request,resource,initialization=requests.get(timeout=0.1)
            except queue.Empty: continue
            try:
                open_owner_session(alias,initialization)
                method=request['method']
                event({'routed_method':method,'owner_alias':alias})
                if method in {'thread/read','thread/turns/list','thread/items/list'}:
                    http_read(alias,request,resource)
                else: send(alias,request,resource)
            except Exception as error:
                event({'owner_routing_failure_type':type(error).__name__})
                if not stopping.is_set():fail(request,'Owner cloud routing verification unavailable')
            finally: requests.task_done()

    def native_requests():
        incoming=queue.Queue(maxsize=1)
        def read_stdin():
            def put(value):
                while not stopping.is_set():
                    try:incoming.put(value,timeout=0.1);return
                    except queue.Full:pass
            try:
                for raw in sys.stdin:
                    if stopping.is_set():return
                    put(('request',raw))
                put(('eof',None))
            except Exception as error:put(('error',error))
        threading.Thread(target=read_stdin,daemon=True).start()
        while True:
            if local_closed.is_set():
                try:code=local.wait(timeout=0.5)
                except subprocess.TimeoutExpired:code=1
                raise SystemExit(code if code>=0 else 128-code)
            try:kind,value=incoming.get(timeout=0.1)
            except queue.Empty:continue
            if kind=='eof':return
            if kind=='error':raise value
            yield value

    local_reader=threading.Thread(target=receive,args=('local',local.stdout),daemon=True)
    local_reader.start()
    try:
        for raw in native_requests() if native else sys.stdin:
            if not native and len(raw)>4*1024*1024: raise ValueError('Native request exceeds frame bound')
            request=json.loads(raw); method=request.get('method')
            if native:
                if method=='initialize':
                    native_initialize=request
                if method=='initialized':
                    initialized=True
                resource=thread_id(request.get('params',{}))
                alias=native_thread_owner(resource)
                # Ordinary tasks and primary auth retain the exact stock RPC path.
                # Only verified Dot roots may cross to an owner cloud session.
                if method in CLOUD_READS and alias is not None:
                    try:
                        if not initialized or native_initialize is None or alias not in owners:
                            raise ValueError('Owner cloud session unavailable')
                        if alias not in native_queues:
                            requests=queue.Queue(maxsize=32)
                            worker=threading.Thread(target=native_owner_worker,args=(alias,requests),daemon=True)
                            native_queues[alias]=requests
                            native_workers.append(worker)
                            worker.start()
                        native_queues[alias].put_nowait((request,resource,native_initialize))
                    except queue.Full:
                        fail(request,'Owner cloud request queue is full')
                    except Exception as error:
                        event({'owner_routing_failure_type':type(error).__name__})
                        fail(request,'Owner cloud routing verification unavailable')
                elif method is not None and alias is not None and alias!=settings.SYSTEM_ALIAS:
                    event({'blocked_method':method,'owner_alias':alias})
                    fail(request,'Secondary Dot operation is not supported')
                else:
                    send('local',request)
                continue
            if method=='initialize' and not initialized:
                initialization=bootstrap.initialize(request,system)
                for alias in (owners if app_session else []):
                    open_owner_session(alias,request)
                initialized=True
                emit({'id':request['id'],'result':initialization})
            elif method=='initialized':
                for alias in sessions: send(alias,request)
            elif not initialized:
                fail(request,'Local authentication bootstrap is required')
            elif method in LOCAL_READS:
                event({'routed_method':method,'owner_alias':'local'})
                send('local',request)
            elif method in CLOUD_READS:
                try:
                    resource=thread_id(request.get('params',{}))
                    if not app_session or resolve_thread_owner(resource,owners) is None:
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
        with sessions_lock: closing=list(sessions.values())
        for ws in closing: ws.close()
        local.terminate()
        try: local.wait(timeout=5)
        except subprocess.TimeoutExpired: local.kill()
        if native:
            local_reader.join(timeout=5)
            deadline=time.monotonic()+0.5
            for worker in native_workers:worker.join(timeout=max(0,deadline-time.monotonic()))

if __name__=='__main__':
    try: main()
    except Exception as error:
        reasons={
            'Frozen proof owner identity changed':'owner_inventory_changed',
            'Public upstream CA roots unavailable':'public_ca_unavailable',
            'Local authentication bootstrap timed out':'local_auth_timeout',
            'Local authentication bootstrap failed':'local_auth_rejected',
            'Local workspace routing verification failed':'workspace_routing_rejected',
            'Local workspace policy verification failed':'workspace_policy_rejected',
            'Primary profile outside proof scope':'primary_profile_changed',
            'system credential principal changed':'system_identity_changed',
        }
        event({'adapter_failure_type':type(error).__name__,
               'adapter_failure_code':reasons.get(str(error),'validation_failure')})
        sys.exit(1)
