"""Secondary-only research executor. No system login, refresh, or credential persistence."""
import hashlib, hmac, http.client, http.server, json, os, pathlib, secrets
import ssl, subprocess, threading, time, urllib.parse, uuid
import catalog,settings

ROOT = settings.ROOT / 'null-executor'
REAL = str(settings.APP / 'Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex')
ALIAS = settings.SECONDARY_ALIAS
REGISTRY = 'codex-cloud-environments.chatgpt.com'
MAX_BODY = 1024 * 1024
USER_HOME = pathlib.Path.home()
PROTECTED = [str(USER_HOME / '.codex'), str(settings.APP), str(settings.SOURCE),
             str(settings.SYSTEM.parent)]
PROTECTED += [str(USER_HOME / 'Library' / directory / name)
              for directory, names in [
                  ('Application Support', ['ChatGPT','Codex','com.openai.codex','com.openai.chat','CodexBar','OpenAI']),
                  ('Preferences', ['com.openai.codex.plist']),
                  ('Logs', ['com.openai.codex']), ('Caches', ['com.openai.codex'])]
              for name in names]


def null_credential():
    manifest = json.loads(catalog.read_owned(catalog.SOURCE / 'managed-codex-accounts.json', catalog.SOURCE))
    if manifest.get('version') != 3:
        raise ValueError('Unsupported managed account manifest')
    rows = [row for row in manifest['accounts']
            if hashlib.sha256(row['providerAccountID'].encode()).hexdigest()[:8] == ALIAS]
    if len(rows) != 1:
        raise ValueError('Secondary account selection ambiguous')
    row = rows[0]
    data = catalog.read_owned(pathlib.Path(row['managedHomePath']) / 'auth.json', catalog.SOURCE)
    if hashlib.sha256(data).hexdigest() != row['authFingerprint']:
        raise ValueError('Secondary credential revision changed')
    owner = catalog.credential(data)
    if (owner['alias'] != ALIAS or row['providerAccountID'] != owner['account_id']
            or row['workspaceAccountID'] != owner['account_id']):
        raise ValueError('Secondary credential owner mismatch')
    return owner

def owner_headers(owner, expected_alias=None):
    alias = ALIAS if expected_alias is None else expected_alias
    if (owner['alias'] != alias or alias == settings.SYSTEM_ALIAS
            or alias not in settings.DOT_OWNERS.values()
            or catalog.claims(owner['token'])['exp'] <= time.time() + 60):
        raise ValueError('Secondary credential unavailable; refresh forbidden')
    return {'Authorization': 'Bearer ' + owner['token'],
            'ChatGPT-Account-Id': owner['account_id'], 'Content-Type': 'application/json',
            'Accept': 'application/json', 'originator': 'codex_work_desktop',
            'User-Agent': 'Codex Desktop/'+settings.APP_VERSION+' (Mac OS; arm64)'}

def public_tls():
    return ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)

def owner_request(owner, host, method, path, body=None, *, expected_alias=None):
    if host not in {'chatgpt.com', REGISTRY}:
        raise ValueError('Host outside research transport')
    connection = http.client.HTTPSConnection(host, context=public_tls(), timeout=25)
    try:
        connection.request(method, path, body=body, headers=owner_headers(owner, expected_alias))
        response = connection.getresponse()
        data = response.read(MAX_BODY + 1)
        if len(data) > MAX_BODY:
            raise ValueError('Research response exceeds bound')
        return response.status, response.getheader('Content-Type', ''), data
    finally:
        connection.close()

def current_root(owner):
    value = catalog.get(owner, '/backend-api/tbo/primary', owner_headers(owner))
    profile, selection = value.get('profile', {}), value.get('selection', {})
    root = profile.get('active_root_thread_id')
    if (profile.get('id') != catalog.PROOF_PROFILE_IDS[ALIAS]
            or selection.get('available') is not True or selection.get('thread_id') != root
            or not isinstance(root, str) or not root or len(root) > 128):
        raise ValueError('Secondary primary identity changed')
    return root

def prepare():
    ROOT.mkdir(mode=0o700, exist_ok=True)
    if ROOT.is_symlink() or ROOT.resolve().parent != settings.ROOT.resolve():
        raise ValueError('Executor root outside research directory')
    for name in ['home', 'work', 'tmp']:
        path = ROOT / name
        path.mkdir(mode=0o700, exist_ok=True)
        if path.is_symlink() or path.resolve().parent != ROOT.resolve():
            raise ValueError('Executor directory outside research root')
    policy = '(version 1)\n(allow default)\n(deny signal (target others))\n'
    policy += '\n'.join('(deny file-write* (subpath ' + json.dumps(path) + '))' for path in PROTECTED)
    policy += '\n(deny file-read* (subpath '+json.dumps(str(USER_HOME / '.codex'))+'))\n'
    (ROOT / 'isolation.sb').write_text(policy)
    os.chmod(ROOT / 'isolation.sb', 0o600)

def cli_environment(capability):
    # Deliberately exclude inherited auth, proxy, plugin pipe and CLI overrides.
    env = {key: os.environ[key] for key in ['PATH', 'HOME', 'USER', 'LOGNAME', 'LANG'] if key in os.environ}
    env.update({'CODEX_HOME': str(ROOT / 'home'), 'TMPDIR': str(ROOT / 'tmp'),
                'CODEX_API_KEY': capability, 'CODEX_EXEC_SERVER_EXIT_ON_STDIN_CLOSE': 'true',
                'CODEX_INTERNAL_ORIGINATOR_OVERRIDE': 'codex_work_desktop',
                'LOG_FORMAT': 'json', 'RUST_LOG': 'codex_exec_server::remote=info'})
    return env

def launch(port, environment_id, capability):
    if not environment_id or len(environment_id) > 128:
        raise ValueError('Invalid research environment')
    return subprocess.Popen([
        '/usr/bin/sandbox-exec', '-f', str(ROOT / 'isolation.sb'), REAL,
        '-c', 'cli_auth_credentials_store="file"',
        'exec-server', '--remote', f'http://127.0.0.1:{port}/api',
        '--environment-id', environment_id, '--exit-on-stdin-close',
    ], cwd=ROOT / 'work', env=cli_environment(capability), stdin=subprocess.PIPE,
       stdout=subprocess.PIPE, stderr=subprocess.PIPE)

def validate_registry_response(data, environment_id):
    value = json.loads(data)
    if value.get('environment_id') != environment_id:
        raise ValueError('Registry environment identity mismatch')
    target = urllib.parse.urlsplit(value.get('url', ''))
    if (target.scheme != 'wss' or target.username is not None or target.password is not None
            or target.port not in {None, 443} or not target.hostname
            or not any(target.hostname == domain or target.hostname.endswith('.' + domain)
                       for domain in ['chatgpt.com', 'openai.com', 'openai.org'])):
        raise ValueError('Registry rendezvous authority invalid')

class RegistryBroker:
    def __init__(self, owner, environment_id, request=owner_request, expected_alias=None):
        alias = ALIAS if expected_alias is None else expected_alias
        if (owner['alias'] != alias or alias == settings.SYSTEM_ALIAS
                or alias not in settings.DOT_OWNERS.values()):
            raise ValueError('Executor principal outside Secondary proof')
        self.owner, self.environment_id, self.request = owner, environment_id, request
        self.capability = secrets.token_urlsafe(32)
        self.events = []
        self.stopped = threading.Event()
        self.lock = threading.Lock()
        broker = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_POST(self):
                prefix = '/api/cloud/environment/' + urllib.parse.quote(broker.environment_id, safe='')
                if (self.path not in {prefix + '/register', prefix + '/validate'}
                        or not hmac.compare_digest(self.headers.get('Authorization', ''),
                                                   'Bearer ' + broker.capability)
                        or broker.stopped.is_set()):
                    self.send_error(403); return
                try:
                    length = int(self.headers.get('Content-Length', '0'))
                    if not 0 < length <= MAX_BODY or self.headers.get('Transfer-Encoding'):
                        raise ValueError('Invalid executor request framing')
                    body = self.rfile.read(length)
                    value = json.loads(body)
                    if self.path.endswith('/register') and (
                            set(value) != {'security_profile', 'executor_public_key'}
                            or value['security_profile'] != 'noise_hybrid_ik_v1'
                            or not isinstance(value['executor_public_key'], dict)):
                        raise ValueError('Invalid native Noise registration')
                    status, content_type, data = broker.request(broker.owner, REGISTRY, 'POST', self.path, body)
                    if status == 200 and self.path.endswith('/register'):
                        validate_registry_response(data, broker.environment_id)
                    if status in {401, 403}:
                        broker.stopped.set()  # No repeated credential rejection.
                    with broker.lock:
                        broker.events.append({'operation': self.path.rsplit('/', 1)[1], 'status': status})
                    self.send_response(status); self.send_header('Content-Type', content_type)
                    self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
                except Exception as error:
                    broker.stopped.set()
                    with broker.lock: broker.events.append({'failure_type': type(error).__name__})
                    self.send_error(502, 'Research registry unavailable')

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self): self.thread.start()
    def close(self):
        self.stopped.set(); self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=3)

def register(owner, installation_id):
    uuid.UUID(installation_id)
    body = json.dumps({'displayName': 'CQ isolated secondary research',
                       'installation_id': installation_id, 'selected_capability_roots': []}).encode()
    status, content_type, data = owner_request(owner, 'chatgpt.com', 'POST',
                                             '/backend-api/flora/cca/executor', body)
    if status != 200 or not content_type.startswith('application/json'):
        raise ValueError('Secondary executor registration HTTP ' + str(status))
    value = json.loads(data)
    environment_id = value.get('executor_id')
    if not isinstance(environment_id, str) or not environment_id or len(environment_id) > 128:
        raise ValueError('Secondary executor registration identity invalid')
    return environment_id
