import http.server, http.client, json, threading, time, urllib.parse, ssl, re
import datetime
import errno
import hmac
import hashlib
import os
import queue
import tempfile
import socket
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
NATIVE_OWNER_TIMEOUT = 3
NATIVE_OWNER_WORKERS = threading.BoundedSemaphore(8)
CATALOG_GENERATION = 0
NATIVE_UPLOAD_LIMIT = 20 * 1024 * 1024 + 65536  # Native file limit plus multipart framing.
FILE_LOCK = threading.Lock()
FILE_OWNERS = {}


def owner_headers(headers, owner):
    result = {k:v for k,v in headers.items() if k.lower() not in [
        'authorization', 'chatgpt-account-id', 'x-openai-account-routing-override',
        'x-openai-expected-account-id', 'cookie', 'x-openai-orbit-selection']}
    result['Authorization'] = 'Bearer ' + owner['token']
    result['ChatGPT-Account-Id'] = owner['account_id']
    return result


def validate_profiles(fixture):
    _, profiles, owners, _ = fixture
    configured = {target[0]: handle for handle, target in settings.DOTS.items()}
    if len(profiles) != len(configured) or {p.get('id') for p in profiles} != set(configured):
        raise ValueError('Catalog profiles differ from configured Dot IDs')
    for profile in profiles:
        handle = configured[profile['id']]
        root = profile.get('active_root_thread_id')
        alias = settings.DOT_OWNERS[handle]
        if (not isinstance(root, str) or not root
                or owners.get(profile['id'], {}).get('alias') != alias
                or owners.get(root, {}).get('alias') != alias
                or owners[profile['id']].get('account_id') != owners[root].get('account_id')):
            raise ValueError('Catalog profile ownership differs from configured Dot owner')


def known_owners(fixture):
    validate_profiles(fixture)
    system, profiles, owners, _ = fixture
    accounts = {system['alias']: system}
    for profile in profiles:
        account = owners[profile['id']]
        previous = accounts.get(account['alias'])
        if previous is not None and previous['account_id'] != account['account_id']:
            raise ValueError('Catalog account alias collision')
        accounts[account['alias']] = account
    if system['alias'] != settings.SYSTEM_ALIAS or set(accounts) != set(settings.DOT_OWNERS.values()):
        raise ValueError('Catalog account principals differ from configured owners')
    return accounts


def publish_native_roots(fixture):
    known_owners(fixture)
    _, profiles, owners, _ = fixture
    roots = [{'owner_alias': owners[profile['id']]['alias'], 'profile_id': profile['id'],
              'root_thread_id': profile['active_root_thread_id']} for profile in profiles]
    if len({row['root_thread_id'] for row in roots}) != len(roots):
        raise ValueError('Catalog root collision')
    target = ROOT / 'native-roots.json'
    if target.is_symlink():
        raise ValueError('Native roots must not be a symlink')
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', dir=ROOT, prefix='.native-roots-', delete=False) as output:
            temporary = output.name
            os.chmod(temporary, 0o600)
            json.dump({'version': 1, 'roots': roots}, output)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, target)
        temporary = None
    finally:
        if temporary is not None:
            os.unlink(temporary)


def observe_native_primary(headers, data):
    global CATALOG_FIXTURE, CATALOG_GENERATION
    value = json.loads(data)
    selection, returned = value.get('selection'), value.get('profile')
    if not isinstance(selection, dict) or selection.get('available') is not True:
        return
    NATIVE_GATE.require(headers)
    with CATALOG_LOCK:
        fixture = CATALOG_FIXTURE
        if fixture is None:
            return
        accounts = known_owners(fixture)
        system, profiles, owners, summary = fixture
        account = accounts[settings.SYSTEM_ALIAS]
        expected = [profile for profile in profiles if owners[profile['id']]['alias'] == settings.SYSTEM_ALIAS]
        root = selection.get('thread_id')
        if (len(expected) != 1 or not isinstance(returned, dict) or returned.get('id') != expected[0]['id']
                or not isinstance(root, str) or not 0 < len(root) <= 128
                or returned.get('active_root_thread_id') != root):
            raise ValueError('Native primary differs from configured owner profile')
        updated = {**expected[0], **returned}
        next_owners = {key: owner for key, owner in owners.items() if owner['alias'] != settings.SYSTEM_ALIAS}
        for key in ['id', 'active_root_thread_id', 'messaging_room_id']:
            identifier = updated.get(key)
            if identifier:
                if not isinstance(identifier, str) or identifier in next_owners:
                    raise ValueError('Native primary resource ownership collision')
                next_owners[identifier] = account
        next_profiles = [updated if profile['id'] == updated['id'] else profile for profile in profiles]
        refreshed = (system, next_profiles, next_owners, summary)
        publish_native_roots(refreshed)
        CATALOG_FIXTURE = refreshed
        CATALOG_GENERATION += 1


def native_owner_results(accounts, operation):
    # Workers never publish state. Late results stay in this bounded, private queue.
    results = queue.Queue(maxsize=max(1, len(accounts)))
    pending = set()
    completed = {}
    deadline = time.monotonic() + NATIVE_OWNER_TIMEOUT
    def run(account):
        try:
            try:
                result = (True, operation(account))
            except Exception as error:
                result = (False, type(error).__name__)
            results.put_nowait((account['alias'], time.monotonic(), result))
        finally:
            NATIVE_OWNER_WORKERS.release()
    for account in accounts:
        alias = account['alias']
        if NATIVE_OWNER_WORKERS.acquire(blocking=False):
            try:
                threading.Thread(target=run, args=(account,), daemon=True).start()
                pending.add(alias)
            except Exception:
                NATIVE_OWNER_WORKERS.release()
                completed[alias] = (False, 'WorkerUnavailable')
        else:
            completed[alias] = (False, 'WorkersUnavailable')
    while pending:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        try:
            alias, finished, result = results.get(timeout=remaining)
        except queue.Empty:
            break
        if finished <= deadline:
            pending.discard(alias)
            completed[alias] = result
    for alias in pending:
        completed[alias] = (False, 'DeadlineExceeded')
    return completed


def native_owner_challenge(account, path, headers):
    upstream = http.client.HTTPSConnection('chatgpt.com', timeout=NATIVE_OWNER_TIMEOUT)
    try:
        upstream.request('GET', path, headers=owner_headers(headers, account))
        response = upstream.getresponse()
        data = response.read(1024 * 1024 + 1)
        if len(data) > 1024 * 1024:
            raise ValueError('owner challenge response too large')
        return response.status, data
    finally:
        upstream.close()


def listen_port():
    path = ROOT / 'port'
    if path.is_symlink():
        raise ValueError('Proxy port must not be a symlink')
    if not path.exists():
        return 8000
    with path.open() as source:
        value = source.read(32)
    if len(value) == 32:
        raise ValueError('Proxy port file exceeds bound')
    port = int(value)
    if port != 8000:
        raise ValueError('Qualified native authentication requires loopback proxy port 8000')
    return port

def create_server():
    try:
        return http.server.ThreadingHTTPServer(('127.0.0.1', listen_port()), Handler)
    except OSError as error:
        if error.errno == errno.EADDRINUSE:
            raise ValueError('Native Dots proxy requires free loopback port 8000; existing listener was left running') from None
        raise

def primary_probe_response(headers, fixture, alias):
    if fixture is None or alias == settings.SYSTEM_ALIAS or alias not in settings.DOT_OWNERS.values():
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
    if (len(matches)!=1 or owner is None or owner['alias']==settings.SYSTEM_ALIAS
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
        key = (primary, owner_alias)
        if key in CHALLENGES:
            raise ValueError('duplicate native challenge')
        if len(CHALLENGES) >= 16 * max(1, len(set(settings.DOT_OWNERS.values())) - 1):
            # Background native checks also issue challenges; retain the newest bounded set.
            del CHALLENGES[min(CHALLENGES, key=lambda key: CHALLENGES[key][1])]
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

def resource_owner(path, fixture):
    if fixture is None:
        return None
    match = re.match(r'^/backend-api/(?:tbo/by-thread/([^/]+)|tbo/([^/]+)|messaging/rooms/([^/]+))(?:/|$)', path)
    if match is None:
        return None
    identifier = urllib.parse.unquote(next(value for value in match.groups() if value is not None))
    field = ['active_root_thread_id', 'id', 'messaging_room_id'][next(
        index for index, value in enumerate(match.groups()) if value is not None)]
    _, profiles, owners, _ = fixture
    matching = [profile for profile in profiles if profile.get(field) == identifier]
    if not matching:
        return None
    owner = owners.get(identifier)
    if (len(matching) != 1 or owner is None
            or owners.get(matching[0]['id'], {}).get('account_id') != owner['account_id']
            or owners.get(matching[0].get('active_root_thread_id'), {}).get('account_id') != owner['account_id']):
        raise ValueError('Resource ownership differs from current catalog')
    return owner

def remember_download(url, owner, now=None):
    if not isinstance(url, str) or len(url) > 8192:
        return
    parsed = urllib.parse.urlsplit(url)
    if (parsed.scheme != 'https' or parsed.netloc != 'chatgpt.com' or parsed.fragment
            or re.fullmatch(r'/backend-api/files/download/[A-Za-z0-9_-]+', parsed.path) is None):
        return
    target = parsed.path
    principal = (owner['alias'], owner['account_id'])
    now = time.monotonic() if now is None else now
    with FILE_LOCK:
        for key in list(FILE_OWNERS):
            if FILE_OWNERS[key][1] <= now:
                del FILE_OWNERS[key]
        previous = FILE_OWNERS.get(target)
        if previous is not None and previous[0] != principal:
            raise ValueError('File download owner collision')
        if len(FILE_OWNERS) >= 2048:
            del FILE_OWNERS[next(iter(FILE_OWNERS))]
        FILE_OWNERS[target] = (principal, now + 1500)

def download_owner(target, fixture, now=None):
    now = time.monotonic() if now is None else now
    target = urllib.parse.urlsplit(target).path
    with FILE_LOCK:
        grant = FILE_OWNERS.get(target)
        if grant is None:
            return None
        if grant[1] <= now:
            del FILE_OWNERS[target]
            return None
    if fixture is not None:
        # Resolve fresh credentials through the current catalogue, never the URL.
        return next((fixture[2].get(profile['id']) for profile in fixture[1]
                     if (fixture[2].get(profile['id'], {}).get('alias'),
                         fixture[2].get(profile['id'], {}).get('account_id')) == grant[0]), None)
    return None

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

def valid_name_update(path, body, fixture):
    match = re.fullmatch(r'/backend-api/tbo/([^/?]+)', path)
    if match is None or resource_owner(path, fixture) is None:
        return False
    try:
        value = json.loads(body)
        name = value.get('display_name') if isinstance(value, dict) else None
        return (set(value) == {'display_name'} and isinstance(name, str)
                and 0 < len(name) <= 24 and bool(name.strip())
                and '\\' not in name and not any(ord(char) < 32 for char in name))
    except (ValueError, TypeError):
        return False

def local_computer_target(path, body, fixture):
    parsed = urllib.parse.urlsplit(path)
    match = re.fullmatch(r'/backend-api/tbo/([^/]+)/computers/([^/]+)/(connect|connect-and-replace|disconnect)', parsed.path)
    if match is None or body:
        return None
    owner = resource_owner(parsed.path, fixture)
    if owner is None or owner['alias'] == settings.SYSTEM_ALIAS:
        return None
    import secondary_executors
    row = secondary_executors.mapping(owner['alias'])
    profile_id = urllib.parse.unquote(match[1])
    profiles = [p for p in fixture[1] if p['id'] == profile_id]
    query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
    if len(profiles) != 1 or query != {'expected_thread_id': [profiles[0]['active_root_thread_id']]}:
        return None
    if row is not None and row['profile_id'] == profile_id and match[2] in {
            row['environment_id'], row['primary_environment_id']}:
        environment = row['environment_id']
    elif match[3] == 'disconnect' and secondary_executors.valid_environment(match[2]):
        # Revoking an older computer must not depend on the current CQ worker.
        # Confirm the exact environment belongs to this Dot and is revocable.
        try:
            computers = catalog.get(owner, '/backend-api/tbo/' + match[1] + '/computers')['computers']
            if not isinstance(computers, list) or any(not isinstance(c, dict) for c in computers):
                return None
            matches = [c for c in computers if c.get('kind') == 'user'
                       and c.get('environment_id') == match[2] and c.get('can_disconnect') is True]
            if len(matches) != 1:
                return None
        except (OSError, ValueError, KeyError, TypeError):
            return None
        environment = match[2]
    else:
        return None
    return (f'/backend-api/tbo/{match[1]}/computers/{environment}/{match[3]}'
            + '?' + parsed.query)
def native_file_upload(path, content_type):
    match = re.fullmatch(r'/backend-api/messaging/rooms/[A-Za-z0-9_-]+/files(?:/uploads(?:/[A-Za-z0-9_-]+/complete)?)?', path)
    if match is None:
        return False
    if path.endswith('/uploads'):
        return content_type.split(';', 1)[0].strip().lower() == 'application/json'
    return content_type.lower().startswith('multipart/form-data;')

def valid_text_message(body, allow_attachments=False):
    try:
        value = json.loads(body)
        content = value['content']
        if not isinstance(content, dict):
            return False
        attachments = content.get('attachments')
        files = (allow_attachments and isinstance(attachments, list) and bool(attachments)
                 and all(isinstance(item, dict) and item.get('type') == 'file'
                         and isinstance(item.get('file_id'), str)
                         and re.fullmatch(r'[A-Za-z0-9_-]{1,256}', item['file_id']) is not None
                         for item in attachments))
        return (isinstance(value, dict) and isinstance(content, dict)
                and isinstance(content.get('text'), str) and (bool(content['text'].strip()) or files)
                and (not attachments or files)
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

    do_HEAD = do_GET
    do_PUT = do_POST
    do_PATCH = do_POST
    do_DELETE = do_POST
    do_OPTIONS = do_POST

    def primary_body(self, size, chunked):
        while chunked or size:
            if chunked:
                line = self.rfile.readline(8193)
                if len(line) > 8192 or not line.endswith(b'\r\n'):
                    raise ValueError('Invalid native request chunk')
                value = line[:-2].split(b';', 1)[0]
                if re.fullmatch(rb'[0-9a-fA-F]+', value) is None:
                    raise ValueError('Invalid native request chunk')
                size = int(value, 16)
                if size == 0:
                    trailer_size = 0
                    while True:
                        line = self.rfile.readline(8193)
                        trailer_size += len(line)
                        if trailer_size > 65536 or not line.endswith(b'\r\n'):
                            raise ValueError('Invalid native request trailers')
                        if line == b'\r\n':
                            return
            while size:
                data = self.rfile.read(min(size, 65536))
                if not data:
                    raise ValueError('Incomplete native request body')
                size -= len(data)
                yield data
            if chunked and self.rfile.read(2) != b'\r\n':
                raise ValueError('Invalid native request chunk terminator')

    def forward_remote_control(self):
        # Only the native primary remote-control upgrade uses this opaque seam.
        # Native peers retain masking, ping/pong, close and authentication policy.
        upstream = None
        raw = None
        sender = None
        started = False
        self.close_connection = True
        try:
            context = ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)
            raw = socket.create_connection(('chatgpt.com', 443), timeout=25)
            upstream = context.wrap_socket(raw, server_hostname='chatgpt.com')
            items = self.headers.raw_items() if hasattr(self.headers, 'raw_items') else self.headers.items()
            headers = []
            for key, value in items:
                if any(char in key or char in value for char in '\r\n'):
                    raise ValueError('Invalid remote control header')
                headers.append(f'{key}: {"chatgpt.com" if key.lower() == "host" else value}\r\n'.encode('latin-1'))
            upstream.sendall(self.raw_requestline + b''.join(headers) + b'\r\n')
            response = bytearray()
            while b'\r\n\r\n' not in response:
                if len(response) >= 65536:
                    raise ValueError('Remote control handshake exceeds bound')
                data = upstream.recv(65536 - len(response))
                if not data:
                    raise ValueError('Remote control handshake closed')
                response.extend(data)
            status = int(response.split(b'\r\n', 1)[0].split(b' ')[1])
            self.wfile.write(response)
            self.wfile.flush()
            started = True
            event({'operation': 'native_remote_control_websocket', 'status': status})
            # A rejected handshake has no WebSocket stream; do not leave it open.
            if status != 101:
                return
            self.connection.settimeout(None)
            upstream.settimeout(None)

            def send_frames():
                try:
                    while True:
                        data = self.rfile.read1(65536)
                        if not data:
                            break
                        upstream.sendall(data)
                except (OSError, ssl.SSLError):
                    pass
                finally:
                    try: upstream.shutdown(socket.SHUT_RDWR)
                    except OSError: pass

            sender = threading.Thread(target=send_frames, daemon=True)
            sender.start()
            while True:
                data = upstream.recv(65536)
                if not data:
                    break
                self.wfile.write(data)
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as error:
            event({'operation': 'native_remote_control_websocket', 'error_type': type(error).__name__})
            if not started:
                self.send_response(502)
                self.end_headers()
        finally:
            if upstream is not None:
                try: upstream.shutdown(socket.SHUT_RDWR)
                except OSError: pass
                upstream.close()
            elif raw is not None:
                raw.close()
            try: self.connection.shutdown(socket.SHUT_RDWR)
            except OSError: pass
            if sender is not None:
                sender.join(timeout=1)

    def forward_primary(self, size, chunked, owner=None):
        connection = http.client.HTTPSConnection('chatgpt.com', timeout=25)
        response_started = False
        try:
            hop_headers = {'connection', 'proxy-connection', 'keep-alive', 'te', 'trailer', 'upgrade'}
            excluded = hop_headers | {'host'}
            excluded.update(value.strip().lower() for value in self.headers.get('Connection', '').split(','))
            headers = {key: value for key, value in self.headers.items() if key.lower() not in excluded}
            if owner is not None:
                headers = owner_headers(headers, owner)
            observe_primary = self.command == 'GET' and urllib.parse.urlsplit(self.path).path == '/backend-api/tbo/primary'
            if observe_primary:
                headers = {key: value for key, value in headers.items() if key.lower() != 'accept-encoding'}
                headers['Accept-Encoding'] = 'identity'
            body = self.primary_body(size, chunked) if size or chunked else None
            connection.request(self.command, self.path, body=body, headers=headers, encode_chunked=chunked)
            response = connection.getresponse()
            buffered = bytearray()
            complete = False
            if observe_primary and response.status == 200:
                while len(buffered) <= 1024 * 1024:
                    data = response.read1(min(65536, 1024 * 1024 + 1 - len(buffered)))
                    if not data:
                        complete = True
                        break
                    buffered.extend(data)
                if complete and buffered:
                    try:
                        observe_native_primary(dict(self.headers.items()), buffered)
                    except Exception as error:
                        event({'native_primary_observation_failed': True, 'error_type': type(error).__name__})
            self.send_response(response.status)
            response_started = True
            excluded = hop_headers | {'transfer-encoding'}
            excluded.update(value.strip().lower() for value in response.getheader('Connection', '').split(','))
            for key, value in response.getheaders():
                if key.lower() not in excluded:
                    if key.lower() == 'set-cookie':
                        if owner is not None:
                            continue
                        value = re.sub(r';\s*Domain=[^;]+', '', value, flags=re.I)
                    self.send_header(key, value)
            self.send_header('Connection', 'close')
            self.end_headers()
            self.close_connection = True
            if self.command != 'HEAD':
                if buffered:
                    self.wfile.write(buffered)
                    self.wfile.flush()
                while not complete:
                    data = response.read1(65536)
                    if not data:
                        break
                    self.wfile.write(data)
                    self.wfile.flush()
            event({'method': self.command, 'path': urllib.parse.urlsplit(self.path).path,
                   'status': response.status, 'native_primary_passthrough': owner is None,
                   **({'owner_file_download': True, 'routed_owner_alias': owner['alias']} if owner else {})})
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as error:
            event({'method': self.command, 'path': urllib.parse.urlsplit(self.path).path,
                   'error_type': type(error).__name__, 'native_primary_passthrough': True})
            if not response_started:
                self.send_response(502)
                self.end_headers()
        finally:
            connection.close()

    def handle_request(self):
        global CATALOG_FIXTURE, CATALOG_GENERATION, NATIVE_REBOOT_AUTH
        parsed = urllib.parse.urlsplit(self.path)
        path = parsed.path
        if parsed.netloc or not path.startswith('/'):
            self.send_response(400);self.end_headers();return
        native_mode = getattr(settings, 'INTEGRATION_MODE', 'isolated') == 'native'
        owner = None
        file_download = False
        if native_mode:
            try:
                with CATALOG_LOCK:
                    owner = resource_owner(path, CATALOG_FIXTURE)
                    if owner is None and self.command in ['GET', 'HEAD']:
                        owner = download_owner(self.path, CATALOG_FIXTURE)
                        file_download = owner is not None
                    catalog_generation = CATALOG_GENERATION
            except ValueError:
                self.send_response(409);self.end_headers();return
        secondary = owner is not None and owner['alias'] != settings.SYSTEM_ALIAS
        inspected_response = (self.command == 'GET' and path in [
            '/backend-api/tbo', '/backend-api/celsius/ws/user', '/backend-api/ios/attestation_challenge']
            or self.command in ['GET', 'POST'] and path.endswith('/wham/statsig/bootstrap'))
        primary_passthrough = native_mode and not secondary and not inspected_response and path != '/probe/state'
        file_upload = (native_mode and secondary and self.command == 'POST' and not parsed.query
                       and native_file_upload(path, self.headers.get('Content-Type', '')))
        transfer_encoding = self.headers.get('Transfer-Encoding', '')
        chunked = transfer_encoding.lower().strip() == 'chunked'
        lengths = [value for key, value in self.headers.items() if key.lower() == 'content-length']
        if (len(lengths) > 1 or transfer_encoding and (not primary_passthrough or not chunked or lengths)):
            self.send_response(400);self.end_headers();return
        try:
            size=int(self.headers.get('Content-Length','0'))
            limit = NATIVE_UPLOAD_LIMIT if file_upload and not path.endswith('/uploads') else 1024 * 1024
            if size < 0 or not primary_passthrough and size > limit:
                raise ValueError('Request exceeds bound')
        except ValueError:
            self.send_response(413);self.end_headers();return
        self.connection.settimeout(30)
        if primary_passthrough:
            if (self.command == 'GET' and path == '/backend-api/wham/remote/control/server'
                    and not parsed.query and not parsed.fragment
                    and self.headers.get('Upgrade', '').lower() == 'websocket'):
                if size or chunked:
                    self.send_response(400);self.end_headers();return
                self.forward_remote_control()
            else:
                self.forward_primary(size, chunked)
            return
        body=self.rfile.read(size)
        if len(body)!=size:self.send_response(400);self.end_headers();return
        if path == '/probe/state':
            if self.command != 'GET':
                self.send_response(405);self.end_headers();return
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps(STATE).encode())
            return
        if not native_mode or secondary or inspected_response:
            try:NATIVE_GATE.require(dict(self.headers.items()))
            except Exception:
                event({'native_request_rejected':True,'method':self.command})
                self.send_response(401);self.end_headers();self.wfile.write(b'{"error":"Native authentication required"}');return
        if file_download:
            self.forward_primary(size, chunked, owner=owner)
            return
        if not native_mode and self.command == 'POST' and path == '/backend-api/flora/cca/executor' and (ROOT/'native-secondary-executor.enabled').exists():
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
        if not native_mode and self.command == 'POST' and path == '/backend-api/cloud-aeons/primary/reboot':
            self.reboot_probe(parsed,body)
            return
        with CATALOG_LOCK:
            runtime_fixture=CATALOG_FIXTURE
            runtime_owner=runtime_control_owner(self.path,body,runtime_fixture) if self.command=='POST' else None
        if runtime_owner is not None and not native_mode:
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
            if send_owner is not None and send_owner['alias'] != settings.SYSTEM_ALIAS and not STATE['secondary_messages']:
                send_owner = None
        with CATALOG_LOCK:
            read_owner = receipt_owner(path, CATALOG_FIXTURE) if self.command == 'POST' else None
        receipt_write = read_owner is not None and valid_read_receipt(body)
        with CATALOG_LOCK:
            watch_owner = live_owner(path, CATALOG_FIXTURE) if self.command == 'POST' else None
        watch_request = watch_owner is not None and not body
        message_write = send_owner is not None and (
            path.endswith('/aeon/prepare') and not body
            or path.endswith('/messages') and valid_text_message(body, allow_attachments=native_mode))
        if not native_mode and STATE['primary_probe_owner'] is not None:
            message_write = False
        name_write = (self.command == 'PATCH' and not parsed.query
                      and valid_name_update(path, body, runtime_fixture))
        computer_target = (local_computer_target(self.path, body, runtime_fixture)
                           if native_mode and self.command == 'POST' else None)
        allowed = self.command == 'GET' or runtime_owner is not None or message_write or receipt_write or watch_request or (
            self.command == 'POST' and (path.endswith('/wham/statsig/bootstrap') or path == '/backend-api/devicecheck'))
        if native_mode:
            allowed = not secondary or self.command in ['GET', 'HEAD'] or runtime_owner is not None or message_write or receipt_write or watch_request or name_write or computer_target is not None or file_upload
        if not allowed:
            event({'method': self.command, 'path': path, 'blocked': True})
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b'{"error":"Read-only probe blocks this operation"}')
            return
        identity_encoding = not native_mode or secondary or inspected_response
        excluded = ['host', 'connection', 'content-length'] + (['accept-encoding'] if identity_encoding else [])
        headers = {k: v for k, v in self.headers.items() if k.lower() not in excluded}
        if identity_encoding:
            headers['Accept-Encoding'] = 'identity'
        if not native_mode:
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
        if owner is not None and (not native_mode or secondary):
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
            if owner['alias'] != settings.SYSTEM_ALIAS:
                headers = {k:v for k,v in headers.items() if k.lower() != 'x-openai-orbit-selection'}
        if message_write and path.endswith('/messages') and owner['alias'] != settings.SYSTEM_ALIAS:
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
        response_started = False
        try:
            connection.request(self.command, computer_target or self.path, body=body or None, headers=headers)
            response = connection.getresponse()
            activity_stream = (self.command == 'GET' and secondary
                               and re.fullmatch(r'/backend-api/tbo/[^/]+/activity/stream', path))
            if response.status == 200 and (watch_request and path.endswith('/live') or activity_stream):
                if not response.getheader('Content-Type','').startswith('text/event-stream'):
                    raise ValueError('Unexpected native room live response type')
                event({'operation':'owner_activity_stream' if activity_stream else 'owner_room_live','routed_owner_alias':owner['alias'],
                       'status':response.status,'streaming':True})
                self.send_response(response.status)
                response_started = True
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
            if (secondary and self.command == 'GET' and response.status == 200
                    and re.fullmatch(r'/backend-api/messaging/rooms/[^/]+/files/[^/]+', path)):
                file = json.loads(data)
                remember_download(file.get('download_url'), owner)
                asset = file.get('asset_pointer')
                match = re.fullmatch(r'sediment://(file[_-][A-Za-z0-9_-]{1,256})', asset) if isinstance(asset, str) else None
                if match:
                    remember_download('https://chatgpt.com/backend-api/files/download/' + match[1], owner)
            if computer_target is not None:
                summary['owner_local_computer_operation'] = path.rsplit('/', 1)[1]
            if secondary and self.command == 'GET' and response.status == 200:
                computers = re.fullmatch(r'/backend-api/tbo/([^/]+)/computers', path)
                if computers:
                    import secondary_executors
                    data = secondary_executors.project_computers(owner['alias'],
                        urllib.parse.unquote(computers[1]), data)
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
                if path.endswith('/messages') and owner['alias'] != settings.SYSTEM_ALIAS:
                    summary['owner_native_challenge_replaced'] = True
                summary['request_header_names'] = sorted(headers.keys())
                if response.status >= 400:
                    summary.update(error_summary(data, dict(response.getheaders())))
            if self.command == 'GET' and path == '/backend-api/tbo' and response.status == 200:
                envelope = json.loads(data)
                if envelope.get('cursor') is not None or parsed.query.find('cursor=') >= 0:
                    raise ValueError('system catalog pagination exceeds isolated proof scope')
                if native_mode:
                    try:
                        success, refreshed = native_owner_results([{'alias': settings.SYSTEM_ALIAS}],
                            lambda account: catalog.fixture(headers, envelope['items']))[settings.SYSTEM_ALIAS]
                        if not success:
                            raise ValueError('Owner catalogue refresh unavailable')
                        known_owners(refreshed)
                        with CATALOG_LOCK:
                            if catalog_generation == CATALOG_GENERATION:
                                publish_native_roots(refreshed)
                                CATALOG_FIXTURE = refreshed
                                CATALOG_GENERATION += 1
                            fixture = CATALOG_FIXTURE
                    except Exception as error:
                        summary['catalog_refresh_error_type'] = type(error).__name__
                        with CATALOG_LOCK:
                            fixture = CATALOG_FIXTURE
                        try:
                            known_owners(fixture)
                            primary = [profile for profile in fixture[1]
                                       if fixture[2][profile['id']]['alias'] == settings.SYSTEM_ALIAS]
                            current = [profile for profile in envelope['items'] if profile.get('id') == primary[0]['id']]
                            if (len(primary) != 1 or len(current) != 1
                                    or any(current[0].get(key) != primary[0].get(key)
                                           for key in ['active_root_thread_id', 'messaging_room_id'])):
                                raise ValueError('Cached catalogue primary changed')
                            summary['catalog_verified_cache_used'] = True
                        except Exception:
                            fixture = None
                            summary['catalog_primary_passthrough'] = True
                else:
                    with CATALOG_LOCK:
                        if CATALOG_FIXTURE is None:
                            CATALOG_FIXTURE = catalog.fixture(headers, envelope['items'])
                            event({'catalog_fixture': CATALOG_FIXTURE[3]})
                        fixture = CATALOG_FIXTURE
                if fixture is not None:
                    validate_profiles(fixture)
                    system, profiles, _, _ = fixture
                    envelope['items'] = profiles
                    envelope['cursor'] = None
                    data = json.dumps(envelope).encode()
                    summary['aggregate_profile_count'] = len(profiles)
                    summary['native_ui_identity_preserved'] = True
            if not native_mode and self.command == 'GET' and path == '/backend-api/tbo/primary' and response.status == 200:
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
            if (owner is not None and owner['alias'] != settings.SYSTEM_ALIAS
                    and response.status == 200
                    and (self.command == 'GET' or message_write and path.endswith('/messages'))
                    and path.startswith('/backend-api/messaging/rooms/')):
                envelope = json.loads(data)
                count = self_projection.project_response(envelope, owner['viewer_ids'], direct=self.command == 'POST')
                if count:
                    data = json.dumps(envelope).encode()
                    summary['owner_self_hint_count'] = count
                    summary['direct_send_response_projection'] = self.command == 'POST'
            if path.endswith('/wham/statsig/bootstrap') and response.status == 200 and (not native_mode or inspected_response):
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
            if path == '/backend-api/celsius/ws/user' and response.status == 200 and CELSIUS is not None and (not native_mode or self.command == 'GET'):
                try:
                    with CATALOG_LOCK:
                        if CATALOG_FIXTURE is None and not native_mode:
                            system,_ = catalog.inventory()
                            CATALOG_FIXTURE = catalog.fixture(headers,catalog.catalog(system,headers))
                        fixture = CATALOG_FIXTURE
                    accounts = known_owners(fixture)
                    value=json.loads(data)
                    urls={settings.SYSTEM_ALIAS:value['websocket_url']}
                    candidates = [account for alias, account in accounts.items() if alias != settings.SYSTEM_ALIAS]
                    if native_mode:
                        request_path, request_headers = self.path, dict(headers)
                        def resolve_url(alias):
                            account = accounts[alias]
                            return catalog.get(account, request_path,
                                owner_headers(request_headers, account))['websocket_url']
                        value['websocket_url']=CELSIUS.issue(urls,fixture[2],resolve_url=resolve_url)
                    else:
                        for account in candidates:
                            urls[account['alias']]=catalog.get(account,self.path,owner_headers(headers,account))['websocket_url']
                        value['websocket_url']=CELSIUS.issue(urls,fixture[2])
                    data=json.dumps(value).encode()
                    summary['owner_celsius_fanout']=True
                except Exception as error:
                    if not native_mode:
                        raise
                    summary['owner_celsius_fanout'] = False
                    summary['secondary_error_type'] = type(error).__name__
                    # Never report a healthy primary-only stream as a multi-owner
                    # session. Native retry can wait for catalogue readiness.
                    event(summary)
                    self.send_response(503)
                    self.send_header('Retry-After','1')
                    self.send_header('Content-Length','0')
                    self.end_headers()
                    return
            if path == '/backend-api/ios/attestation_challenge':
                summary['request_header_names'] = sorted(headers.keys())
                summary['native_device_proof_present'] = any(
                    k.lower() == 'x-sentinel-dc' and bool(v) for k,v in headers.items())
                if (self.command == 'GET' and response.status == 200
                        and STATE['secondary_challenges'] and summary['native_device_proof_present']):
                    with CATALOG_LOCK:
                        candidates = [] if CATALOG_FIXTURE is None else [
                            account for alias, account in known_owners(CATALOG_FIXTURE).items()
                            if alias != settings.SYSTEM_ALIAS]
                    if native_mode:
                        started = time.monotonic()
                        results = native_owner_results(candidates, lambda account:
                            native_owner_challenge(account, self.path, headers))
                        for alias, (success, result) in results.items():
                            issued = False
                            try:
                                if not success:
                                    event({'operation':'owner_native_challenge', 'routed_owner_alias':alias,
                                           'issued':False, 'error_type':result})
                                    continue
                                status, owner_data = result
                                if status == 200:
                                    remember_challenge(data, owner_data, alias, now=started)
                                    issued = True
                                event({'operation':'owner_native_challenge', 'routed_owner_alias':alias,
                                       'status':status, 'issued':issued, 'native_device_proof_preserved':True})
                            except Exception as error:
                                event({'operation':'owner_native_challenge', 'routed_owner_alias':alias,
                                       'issued':False, 'error_type':type(error).__name__})
                    for secondary in [] if native_mode else candidates:
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
                if k.lower() not in ['content-length', 'transfer-encoding', 'connection'] + (['content-encoding'] if identity_encoding else []):
                    if k.lower() == 'set-cookie':
                        v = re.sub(r';\s*Domain=[^;]+', '', v, flags=re.I)
                    self.send_header(k, v)
            self.send_header('Content-Length', response.getheader('Content-Length', '0') if self.command == 'HEAD' else str(len(data)))
            self.end_headers()
            if self.command != 'HEAD':
                self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as e:
            event({'method': self.command, 'path': path, 'error_type': type(e).__name__})
            if not response_started:
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
    server = create_server()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(ROOT / 'leaf-cert.pem', ROOT / 'leaf-key.pem')
    server.socket = context.wrap_socket(server.socket, server_side=True)
    (ROOT / 'port').write_text(str(server.server_port))
    event({'listening_port': server.server_port, 'verified_room_text_sends_enabled': True})
    server.serve_forever()
