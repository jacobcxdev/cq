import http.server, http.client, json, threading, time, urllib.parse, ssl, re
import datetime
import hmac
import hashlib
import catalog
import settings
import self_projection
import celsius
from native_gate import NativeGate
NATIVE_GATE=NativeGate()

ROOT = settings.ROOT
STATE = {'rewrite': True, 'messages': True, 'secondary_messages': True, 'secondary_challenges': True,
         'primary_probe_owner': None, 'reboot_armed': False}
REBOOT_LOCK = threading.Lock()
NATIVE_REBOOT_AUTH = None
LOCK = threading.Lock()
CATALOG_LOCK = threading.Lock()
CATALOG_FIXTURE = None
CELSIUS = None
CHALLENGE_LOCK = threading.Lock()
CHALLENGES = {}


def owner_headers(headers, owner):
    result = {k:v for k,v in headers.items() if k.lower() not in [
        'authorization', 'chatgpt-account-id', 'x-openai-account-routing-override',
        'x-openai-expected-account-id', 'cookie', 'x-openai-orbit-selection']}
    result['Authorization'] = 'Bearer ' + owner['token']
    result['ChatGPT-Account-Id'] = owner['account_id']
    return result

def primary_probe_response(headers, fixture, alias):
    if fixture is None or alias != settings.SECONDARY_ALIAS:
        raise ValueError('Unknown primary proof owner')
    _, profiles, owners, _ = fixture
    candidates = [p for p in profiles if owners.get(p['id'],{}).get('alias') == alias]
    if len(candidates) != 1:
        raise ValueError('Primary proof requires one known owner profile')
    profile = candidates[0]
    owner = owners[profile['id']]
    value = catalog.get(owner,'/backend-api/tbo/primary',owner_headers(headers,owner))
    selection = value.get('selection')
    returned = value.get('profile')
    root = profile.get('active_root_thread_id')
    if (not isinstance(selection,dict) or selection.get('available') is not True
            or not isinstance(root,str) or selection.get('thread_id') != root
            or not isinstance(returned,dict) or returned.get('id') != profile['id']
            or returned.get('active_root_thread_id') != root
            or owners.get(root,{}).get('account_id') != owner['account_id']):
        raise ValueError('Owner primary selection outside proof catalog')
    return value

def native_reboot_authority(headers, fixture, primary, now=None):
    # Called only after an unmodified main-account primary read succeeds upstream.
    system, profiles, owners, _ = fixture
    incoming={k.lower():v for k,v in headers.items()}
    authorization=incoming.get('authorization','')
    selection=primary.get('selection')
    returned=primary.get('profile')
    if (not authorization.startswith('Bearer ') or len(authorization)<=7
            or incoming.get('chatgpt-account-id')!=system['account_id']
            or not isinstance(selection,dict) or selection.get('available') is not True
            or not isinstance(returned,dict)):
        raise ValueError('reboot_identity_unverified')
    known=next((p for p in profiles if p['id']==returned.get('id')),None)
    root=selection.get('thread_id')
    if (known is None or not isinstance(root,str)
            or known.get('active_root_thread_id')!=root or returned.get('active_root_thread_id')!=root
            or owners.get(known['id'],{}).get('account_id')!=system['account_id']
            or owners.get(root,{}).get('account_id')!=system['account_id']):
        raise ValueError('reboot_identity_unverified')
    return hashlib.sha256(authorization.encode()).digest(), time.monotonic() if now is None else now

def verify_native_control_identity(headers, system, native_authority, now=None):
    incoming = {k.lower():v for k,v in headers.items()}
    now=time.monotonic() if now is None else now
    if native_authority is None or not 0<=now-native_authority[1]<=300:
        raise ValueError('reboot_identity_unverified')
    if not hmac.compare_digest(hashlib.sha256(incoming.get('authorization','').encode()).digest(),native_authority[0]):
        raise ValueError('reboot_bearer_mismatch')
    if incoming.get('chatgpt-account-id',system['account_id']) != system['account_id']:
        raise ValueError('reboot_account_mismatch')

def refresh_native_control_identity(headers,fixture):
    system=fixture[0]
    incoming={k.lower():v for k,v in headers.items()}
    authorization=incoming.get('authorization','')
    if (not authorization.startswith('Bearer ') or len(authorization)<=7
            or incoming.get('chatgpt-account-id')!=system['account_id']):
        raise ValueError('reboot_identity_unverified')
    native={**system,'token':authorization[7:]}
    value=catalog.get(native,'/backend-api/tbo/primary',owner_headers(headers,native))
    authority=native_reboot_authority(headers,fixture,value)
    event({'native_control_identity_read_revalidated':True,'status':200,'control_operation_sent':False})
    return authority

def claim_reboot_owner(headers, body, fixture, state, native_authority=None, now=None):
    if fixture is None or state.get('primary_probe_owner') != settings.SECONDARY_ALIAS or state.get('reboot_armed') is not True:
        raise ValueError('reboot_not_armed')
    system, profiles, owners, _ = fixture
    verify_native_control_identity(headers,system,native_authority,now)
    if body:raise ValueError('reboot_body_invalid')
    candidates=[owners[p['id']] for p in profiles if owners.get(p['id'],{}).get('alias')==settings.SECONDARY_ALIAS]
    if len(candidates)!=1:
        raise ValueError('reboot_owner_unavailable')
    state['reboot_armed']=False
    return candidates[0]

def runtime_control_owner(path, body, fixture):
    if fixture is None or body:return None
    parsed=urllib.parse.urlsplit(path)
    parts=parsed.path.split('/')
    if (parsed.query or len(parts)!=6 or parts[1:3]!=['backend-api','tbo']
            or parts[4]!='runtime' or parts[5] not in ['pause','resume']):return None
    _,profiles,owners,_=fixture
    identifier=urllib.parse.unquote(parts[3])
    matches=[p for p in profiles if p['id']==identifier]
    owner=owners.get(identifier)
    if (len(matches)!=1 or owner is None or owner['alias']!=settings.SECONDARY_ALIAS
            or owners.get(matches[0].get('active_root_thread_id'),{}).get('account_id')!=owner['account_id']):return None
    return owner

def challenge_value(data):
    value = json.loads(data)
    challenge = value.get('attestation_challenge')
    expiry = value.get('expire_after', 540)
    if not isinstance(challenge, str) or not challenge or len(challenge) > 65536:
        raise ValueError('invalid native challenge')
    if expiry is None:
        expiry = 540
    if not isinstance(expiry, (int, float)) or isinstance(expiry, bool) or expiry <= 0:
        raise ValueError('invalid native challenge expiry')
    return challenge, min(expiry, 540)

def remember_challenge(primary_data, owner_data, owner_alias, now=None):
    primary, first_expiry = challenge_value(primary_data)
    secondary, second_expiry = challenge_value(owner_data)
    now = time.monotonic() if now is None else now
    with CHALLENGE_LOCK:
        for key in list(CHALLENGES):
            if CHALLENGES[key][1] <= now:
                del CHALLENGES[key]
        if len(CHALLENGES) >= 16:
            raise ValueError('native challenge cache full')
        key = (primary, owner_alias)
        if key in CHALLENGES:
            raise ValueError('duplicate native challenge')
        CHALLENGES[key] = (secondary, now + min(first_expiry, second_expiry))

def owner_message_body(body, owner_alias, now=None):
    value = json.loads(body)
    original = value.get('app_attest_challenge')
    if not isinstance(original, str) or not original:
        raise ValueError('native verification challenge missing')
    now = time.monotonic() if now is None else now
    with CHALLENGE_LOCK:
        replacement = CHALLENGES.pop((original, owner_alias), None)
    if replacement is None or replacement[1] <= now:
        raise ValueError('owner verification challenge unavailable')
    value['app_attest_challenge'] = replacement[0]
    return json.dumps(value).encode()

def message_owner(path, fixture):
    match = re.fullmatch(r'/backend-api/messaging/rooms/([A-Za-z0-9_-]+)/(messages|aeon/prepare)', path)
    if match is None or fixture is None:
        return None
    _, profiles, owners, _ = fixture
    room = match[1]
    if sum(p.get('messaging_room_id') == room for p in profiles) != 1:
        return None
    return owners.get(room)

def receipt_owner(path, fixture):
    match = re.fullmatch(r'/backend-api/messaging/rooms/([A-Za-z0-9_-]+)/read', path)
    if match is None:
        return None
    return message_owner('/backend-api/messaging/rooms/' + match[1] + '/messages', fixture)

def live_owner(path, fixture):
    match = re.fullmatch(r'/backend-api/messaging/rooms/([A-Za-z0-9_-]+)/(live|responding_heartbeat)', path)
    if match is None:
        return None
    return message_owner('/backend-api/messaging/rooms/' + match[1] + '/messages', fixture)

def valid_read_receipt(body):
    try:
        value = json.loads(body)
        if not isinstance(value, dict) or set(value) != {'last_read_at'}:
            return False
        timestamp = value['last_read_at']
        if not isinstance(timestamp, str) or len(timestamp) > 64:
            return False
        return datetime.datetime.fromisoformat(timestamp.replace('Z', '+00:00')).tzinfo is not None
    except (ValueError, TypeError):
        return False

def valid_text_message(body):
    try:
        value = json.loads(body)
        content = value['content']
        return (isinstance(value, dict) and isinstance(content, dict)
                and isinstance(content.get('text'), str) and bool(content['text'].strip())
                and not content.get('attachments')
                and isinstance(value.get('request_id'), str) and bool(value['request_id'])
                and value.get('idempotency_token') == value['request_id'])
    except (ValueError, TypeError, KeyError):
        return False

def error_summary(data, headers):
    summary = {'response_content_type': headers.get('Content-Type'),
               'response_challenge': headers.get('cf-mitigated') == 'challenge'}
    try:
        value = json.loads(data)
        if isinstance(value, dict):
            summary['error_keys'] = sorted(value.keys())
            for key in ['detail', 'error', 'code', 'message']:
                text = value.get(key)
                if isinstance(text, str) and re.fullmatch(r'[A-Za-z0-9 :_.,;()/-]{1,160}', text) and not re.search(r'[A-Za-z0-9_-]{25,}', text):
                    summary['safe_error_' + key] = text
                elif isinstance(text, dict):
                    summary['error_' + key + '_keys'] = sorted(text.keys())
    except (ValueError, TypeError):
        summary['non_json_error'] = True
    return summary

def event(data):
    with LOCK:
        with (ROOT / 'events.jsonl').open('a') as f:
            f.write(json.dumps({'at': time.time(), **data}) + '\n')

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.handle_request()

    def do_POST(self):
        self.handle_request()

    def handle_request(self):
        global CATALOG_FIXTURE, NATIVE_REBOOT_AUTH
        parsed = urllib.parse.urlsplit(self.path)
        path = parsed.path
        if parsed.netloc or not path.startswith('/') or self.headers.get('Transfer-Encoding'):
            self.send_response(400);self.end_headers();return
        try:
            size=int(self.headers.get('Content-Length','0'))
            if not 0<=size<=1024*1024:raise ValueError('Request exceeds bound')
        except ValueError:
            self.send_response(413);self.end_headers();return
        self.connection.settimeout(30)
        body=self.rfile.read(size)
        if len(body)!=size:self.send_response(400);self.end_headers();return
        if path == '/probe/state':
            if self.command != 'GET':
                self.send_response(405);self.end_headers();return
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps(STATE).encode())
            return
        try:NATIVE_GATE.require(dict(self.headers.items()))
        except Exception:
            event({'native_request_rejected':True,'method':self.command})
            self.send_response(401);self.end_headers();self.wfile.write(b'{"error":"Native authentication required"}');return
        if self.command == 'POST' and path == '/backend-api/flora/cca/executor' and (ROOT/'native-secondary-executor.enabled').exists():
            try:
                if parsed.query:raise ValueError('Unexpected registration query')
                import native_executor_reuse
                result,audit=native_executor_reuse.reuse(dict(self.headers.items()),body)
                event(audit);payload=json.dumps(result).encode();self.send_response(200)
            except Exception as error:
                event({'operation':'native_executor_reuse','error_type':type(error).__name__,'reason':str(error) if str(error) in ['Isolated native executor identity mismatch','Native registration body invalid','Existing research registration unavailable','Research registration status identity mismatch'] else 'other_validation','new_registration_sent':False})
                payload=b'{"error":"Isolated research executor reuse unavailable"}';self.send_response(409)
            self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(payload)));self.end_headers();self.wfile.write(payload)
            return
        if self.command == 'POST' and path == '/backend-api/cloud-aeons/primary/reboot':
            self.reboot_probe(parsed,body)
            return
        with CATALOG_LOCK:
            runtime_fixture=CATALOG_FIXTURE
            runtime_owner=runtime_control_owner(self.path,body,runtime_fixture) if self.command=='POST' else None
        if runtime_owner is not None:
            try:
                with REBOOT_LOCK:
                    authority=NATIVE_REBOOT_AUTH
                try:
                    verify_native_control_identity(dict(self.headers.items()),runtime_fixture[0],authority)
                except ValueError:
                    authority=refresh_native_control_identity(dict(self.headers.items()),runtime_fixture)
                    with REBOOT_LOCK:NATIVE_REBOOT_AUTH=authority
                    verify_native_control_identity(dict(self.headers.items()),runtime_fixture[0],authority)
            except Exception as error:
                event({'operation':'owner_runtime_control','blocked':True,'reason':'native_identity_unverified','error_type':type(error).__name__,'upstream_request_sent':False})
                self.send_response(409);self.end_headers()
                self.wfile.write(b'{"error":"Native control identity verification unavailable; no operation submitted"}')
                return
        with CATALOG_LOCK:
            send_owner = message_owner(path, CATALOG_FIXTURE) if self.command == 'POST' and STATE['messages'] else None
            if send_owner is not None and send_owner['alias'] == settings.SECONDARY_ALIAS and not STATE['secondary_messages']:
                send_owner = None
        with CATALOG_LOCK:
            read_owner = receipt_owner(path, CATALOG_FIXTURE) if self.command == 'POST' else None
        receipt_write = read_owner is not None and valid_read_receipt(body)
        with CATALOG_LOCK:
            watch_owner = live_owner(path, CATALOG_FIXTURE) if self.command == 'POST' else None
        watch_request = watch_owner is not None and not body
        message_write = send_owner is not None and (
            path.endswith('/aeon/prepare') and not body
            or path.endswith('/messages') and valid_text_message(body))
        if STATE['primary_probe_owner'] is not None:
            message_write = False
        allowed = self.command == 'GET' or runtime_owner is not None or message_write or receipt_write or watch_request or (
            self.command == 'POST' and (path.endswith('/wham/statsig/bootstrap') or path == '/backend-api/devicecheck'))
        if not allowed:
            event({'method': self.command, 'path': path, 'blocked': True})
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b'{"error":"Read-only probe blocks this operation"}')
            return
        headers = {k: v for k, v in self.headers.items()
                   if k.lower() not in ['host', 'connection', 'content-length', 'accept-encoding']}
        headers['Accept-Encoding'] = 'identity'
        owner = None
        with CATALOG_LOCK:
            if CATALOG_FIXTURE is not None:
                _, _, owners, _ = CATALOG_FIXTURE
                parts = path.split('/')
                if path.startswith('/backend-api/tbo/by-thread/'):
                    owner = owners.get(urllib.parse.unquote(parts[4]))
                elif path.startswith('/backend-api/tbo/') and parts[3] != 'primary':
                    owner = owners.get(urllib.parse.unquote(parts[3]))
                elif path.startswith('/backend-api/messaging/rooms/'):
                    owner = owners.get(urllib.parse.unquote(parts[4]))
        if owner is not None:
            headers = {k: v for k, v in headers.items()
                       if k.lower() not in ['authorization', 'chatgpt-account-id', 'x-openai-account-routing-override',
                                            'x-openai-expected-account-id', 'cookie']}
            headers['Authorization'] = 'Bearer ' + owner['token']
            headers['ChatGPT-Account-Id'] = owner['account_id']
            if runtime_owner is not None and owner!=runtime_owner:
                raise ValueError('Runtime operation owner changed before forwarding')
            if (message_write and owner != send_owner or receipt_write and owner != read_owner
                    or watch_request and owner != watch_owner):
                raise ValueError('Room operation owner changed before forwarding')
            if owner['alias'] == settings.SECONDARY_ALIAS:
                headers = {k:v for k,v in headers.items() if k.lower() != 'x-openai-orbit-selection'}
        if message_write and path.endswith('/messages') and owner['alias'] == settings.SECONDARY_ALIAS:
            try:
                body = owner_message_body(body, owner['alias'])
            except (ValueError, TypeError):
                event({'method': self.command, 'path': path, 'blocked': True,
                       'reason': 'owner_native_verification_unavailable'})
                self.send_response(409)
                self.end_headers()
                self.wfile.write(b'{"error":"Owner native verification unavailable; no message submitted"}')
                return
        connection = http.client.HTTPSConnection('chatgpt.com', timeout=25)
        try:
            connection.request(self.command, self.path, body=body or None, headers=headers)
            response = connection.getresponse()
            if watch_request and path.endswith('/live') and response.status == 200:
                if not response.getheader('Content-Type','').startswith('text/event-stream'):
                    raise ValueError('Unexpected native room live response type')
                event({'operation':'owner_room_live','routed_owner_alias':owner['alias'],
                       'status':response.status,'streaming':True})
                self.send_response(response.status)
                for key,value in response.getheaders():
                    if key.lower() not in ['content-length','transfer-encoding','content-encoding','connection','set-cookie']:
                        self.send_header(key,value)
                self.send_header('Connection','close')
                self.end_headers()
                self.wfile.flush()
                self.close_connection=True
                while True:
                    chunk=response.read1(65536)
                    if not chunk:break
                    self.wfile.write(chunk)
                    self.wfile.flush()
                return
            data = response.read(16 * 1024 * 1024 + 1)
            if len(data) > 16 * 1024 * 1024:
                raise ValueError('response too large')
            summary = {'method': self.command, 'path': path, 'status': response.status,
                       'has_authorization': bool(self.headers.get('Authorization'))}
            if owner is not None:
                summary['routed_owner_alias'] = owner['alias']
            if runtime_owner is not None:
                summary['owner_runtime_operation']=path.rsplit('/',1)[1]
                if response.status>=400:summary.update(error_summary(data,dict(response.getheaders())))
                elif response.status==200:
                    runtime_value=json.loads(data)
                    summary['runtime_result_status']=runtime_value.get('status')
                    summary['runtime_result_known_root']=any(p.get('active_root_thread_id')==runtime_value.get('thread_id') for p in runtime_fixture[1] if p['id']==urllib.parse.unquote(path.split('/')[3]))
            if watch_request:
                summary['owner_room_watch_operation'] = 'live' if path.endswith('/live') else 'typing'
            if receipt_write:
                summary['owner_receipt_operation'] = True
            if message_write:
                summary['owner_message_operation'] = 'prepare' if path.endswith('/aeon/prepare') else 'send'
                if path.endswith('/messages') and owner['alias'] == settings.SECONDARY_ALIAS:
                    summary['owner_native_challenge_replaced'] = True
                summary['request_header_names'] = sorted(headers.keys())
                if response.status >= 400:
                    summary.update(error_summary(data, dict(response.getheaders())))
            if self.command == 'GET' and path == '/backend-api/tbo' and response.status == 200:
                envelope = json.loads(data)
                if envelope.get('cursor') is not None or parsed.query.find('cursor=') >= 0:
                    raise ValueError('system catalog pagination exceeds isolated proof scope')
                with CATALOG_LOCK:
                    if CATALOG_FIXTURE is None:
                        CATALOG_FIXTURE = catalog.fixture(headers, envelope['items'])
                        event({'catalog_fixture': CATALOG_FIXTURE[3]})
                    system, profiles, _, _ = CATALOG_FIXTURE
                if len(profiles) != 2 or {p['display_name'] for p in profiles} != set(settings.DOTS):
                    raise ValueError('Expected exactly the two configured Dot profiles')
                envelope['items'] = profiles
                envelope['cursor'] = None
                data = json.dumps(envelope).encode()
                summary['aggregate_profile_count'] = len(profiles)
                summary['native_ui_identity_preserved'] = True
            if self.command == 'GET' and path == '/backend-api/tbo/primary' and response.status == 200:
                with CATALOG_LOCK:
                    if CATALOG_FIXTURE is None:
                        system,_ = catalog.inventory()
                        CATALOG_FIXTURE = catalog.fixture(headers,catalog.catalog(system,headers))
                    fixture = CATALOG_FIXTURE
                incoming={k.lower():v for k,v in self.headers.items()}
                summary['native_bearer_matches_cli_fixture']=hmac.compare_digest(incoming.get('authorization',''),'Bearer '+fixture[0]['token'])
                summary['native_account_header_present']='chatgpt-account-id' in incoming
                summary['native_account_header_matches_cli_fixture']=incoming.get('chatgpt-account-id',fixture[0]['account_id'])==fixture[0]['account_id']
                authority=native_reboot_authority(dict(self.headers.items()),fixture,json.loads(data))
                with REBOOT_LOCK: NATIVE_REBOOT_AUTH=authority
                summary['backend_verified_native_reboot_identity']=True
                if STATE['primary_probe_owner'] is not None:
                    data = json.dumps(primary_probe_response(headers,fixture,STATE['primary_probe_owner'])).encode()
                    summary['primary_projection_owner_alias'] = STATE['primary_probe_owner']
                    summary['genuine_owner_primary_selection'] = True
                    summary['remote_primary_mutation_sent'] = False
            if (owner is not None and owner['alias'] == settings.SECONDARY_ALIAS
                    and response.status == 200
                    and (self.command == 'GET' or message_write and path.endswith('/messages'))
                    and path.startswith('/backend-api/messaging/rooms/')):
                envelope = json.loads(data)
                count = self_projection.project_response(envelope, owner['viewer_ids'], direct=self.command == 'POST')
                if count:
                    data = json.dumps(envelope).encode()
                    summary['owner_self_hint_count'] = count
                    summary['direct_send_response_projection'] = self.command == 'POST'
            if path.endswith('/wham/statsig/bootstrap') and response.status == 200:
                envelope = json.loads(data)
                payload = json.loads(envelope['statsigPayload'])
                gate = payload.get('feature_gates', {}).get('2929219922')
                summary['catalog_before'] = gate.get('value') if gate else None
                if STATE['rewrite']:
                    before_user = envelope.get('user'), payload.get('user')
                    before_other = {k: v for k, v in payload.items() if k != 'feature_gates'}
                    gates = payload.setdefault('feature_gates', {})
                    before_gates = {k: v for k, v in gates.items() if k != '2929219922'}
                    if payload.get('response_format') == 'init-v2':
                        raise ValueError('unexpected compact format')
                    if gate is None:
                        gate = {'name': '2929219922', 'value': True, 'rule_id': 'cq-local-catalog-proof', 'secondary_exposures': [], 'id_type': 'userID'}
                        gates['2929219922'] = gate
                    else:
                        gate['value'] = True
                    summary['identity_preserved'] = before_user == (envelope.get('user'), payload.get('user'))
                    summary['other_evaluations_preserved'] = before_gates == {k: v for k, v in gates.items() if k != '2929219922'} and before_other == {k: v for k, v in payload.items() if k != 'feature_gates'}
                    envelope['statsigPayload'] = json.dumps(payload)
                    data = json.dumps(envelope).encode()
                    summary['catalog_after'] = True
            if path == '/backend-api/celsius/ws/user' and response.status == 200 and CELSIUS is not None:
                with CATALOG_LOCK:
                    if CATALOG_FIXTURE is None:
                        system,_ = catalog.inventory()
                        CATALOG_FIXTURE = catalog.fixture(headers,catalog.catalog(system,headers))
                    _,_,owners,_ = CATALOG_FIXTURE
                secondary=next(account for account in owners.values() if account['alias']==settings.SECONDARY_ALIAS)
                secondary_url=catalog.get(secondary,self.path,owner_headers(headers,secondary))['websocket_url']
                value=json.loads(data)
                value['websocket_url']=CELSIUS.issue({settings.SYSTEM_ALIAS:value['websocket_url'],settings.SECONDARY_ALIAS:secondary_url},owners)
                data=json.dumps(value).encode()
                summary['owner_celsius_fanout']=True
            if path == '/backend-api/ios/attestation_challenge':
                summary['request_header_names'] = sorted(headers.keys())
                summary['native_device_proof_present'] = any(
                    k.lower() == 'x-sentinel-dc' and bool(v) for k,v in headers.items())
                if (self.command == 'GET' and response.status == 200
                        and STATE['secondary_challenges'] and summary['native_device_proof_present']):
                    with CATALOG_LOCK:
                        candidates = [] if CATALOG_FIXTURE is None else [
                            account for account in CATALOG_FIXTURE[2].values()
                            if account['alias'] == settings.SECONDARY_ALIAS]
                    if candidates:
                        secondary = candidates[0]
                        upstream = http.client.HTTPSConnection('chatgpt.com', timeout=25)
                        try:
                            upstream.request('GET', self.path, headers=owner_headers(headers, secondary))
                            owner_response = upstream.getresponse()
                            owner_data = owner_response.read(1024 * 1024 + 1)
                            if len(owner_data) > 1024 * 1024:
                                raise ValueError('owner challenge response too large')
                            issued = False
                            if owner_response.status == 200:
                                remember_challenge(data, owner_data, secondary['alias'])
                                issued = True
                            event({'operation':'owner_native_challenge', 'routed_owner_alias':secondary['alias'],
                                   'status':owner_response.status, 'issued':issued,
                                   'native_device_proof_preserved':True})
                        except Exception as error:
                            event({'operation':'owner_native_challenge', 'error_type':type(error).__name__})
                        finally:
                            upstream.close()
            cookies = [v for k, v in response.getheaders() if k.lower() == 'set-cookie']
            if path == '/backend-api/devicecheck':
                summary['cookie_count'] = len(cookies)
                summary['cookie_domain_removed'] = any(re.search(r';\s*Domain=', v, re.I) for v in cookies)
            event(summary)
            self.send_response(response.status)
            for k, v in response.getheaders():
                if k.lower() not in ['content-length', 'transfer-encoding', 'content-encoding', 'connection']:
                    if k.lower() == 'set-cookie':
                        v = re.sub(r';\s*Domain=[^;]+', '', v, flags=re.I)
                    self.send_header(k, v)
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as e:
            event({'method': self.command, 'path': path, 'error_type': type(e).__name__})
            self.send_response(502)
            self.end_headers()
        finally:
            connection.close()

    def reboot_probe(self, parsed, body):
        connection=None
        submitted=False
        try:
            if parsed.query: raise ValueError('Reboot proof rejects query parameters')
            with CATALOG_LOCK:
                fixture=CATALOG_FIXTURE
            with REBOOT_LOCK:
                owner=claim_reboot_owner(dict(self.headers.items()),body,fixture,STATE,NATIVE_REBOOT_AUTH)
            # This read must still match the configured secondary root immediately before dispatch.
            primary_probe_response(dict(self.headers.items()),fixture,owner['alias'])
            headers={k:v for k,v in self.headers.items() if k.lower() not in
                     ['host','connection','content-length','accept-encoding']}
            headers['Accept-Encoding']='identity'
            connection=http.client.HTTPSConnection('chatgpt.com',timeout=25)
            submitted=True
            connection.request('POST',parsed.path,headers=owner_headers(headers,owner))
            response=connection.getresponse()
            data=response.read(1024*1024+1)
            if len(data)>1024*1024:raise ValueError('Reboot response exceeds bound')
            event({'operation':'owner_reboot','routed_owner_alias':owner['alias'],'status':response.status,
                   'owner_primary_revalidated':True,'upstream_request_sent':True,'automatic_retry':False})
            self.send_response(response.status)
            for key,value in response.getheaders():
                if key.lower() not in ['content-length','transfer-encoding','content-encoding','connection','set-cookie']:
                    self.send_header(key,value)
            self.send_header('Content-Length',str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except Exception as error:
            summary={'operation':'owner_reboot','blocked':not submitted,'error_type':type(error).__name__,
                     'upstream_request_sent':submitted,'automatic_retry':False}
            if str(error) in ['reboot_not_armed','reboot_identity_unverified','reboot_body_invalid',
                              'reboot_bearer_mismatch','reboot_account_mismatch','reboot_owner_unavailable']:
                summary['reason']=str(error)
            event(summary)
            self.send_response(502 if submitted else 409)
            self.end_headers()
            self.wfile.write(b'{"error":"Secondary reboot proof unavailable; no automatic retry"}')
        finally:
            if connection is not None:connection.close()

if __name__ == '__main__':
    CELSIUS = celsius.Broker(event)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 8000), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(ROOT / 'leaf-cert.pem', ROOT / 'leaf-key.pem')
    server.socket = context.wrap_socket(server.socket, server_side=True)
    (ROOT / 'port').write_text(str(server.server_port))
    event({'listening_port': server.server_port, 'verified_room_text_sends_enabled': True})
    server.serve_forever()
