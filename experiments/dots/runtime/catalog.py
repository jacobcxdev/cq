"""Read-only, in-memory account catalog fixture for the isolated experiment."""
import base64
import copy
import hashlib
import http.client
import json
import os
import pathlib
import ssl
import stat
import time
import urllib.parse
import settings

SOURCE = settings.SOURCE
CQ_ACCOUNTS = settings.CQ_ACCOUNTS
SYSTEM = settings.SYSTEM
PROOF_PROFILE_IDS = settings.PROOF_PROFILE_IDS
NATIVE_USER_AGENT = ('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) '
                     'AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36')



def read_owned(path, root=None):
    path = pathlib.Path(path)
    if root is not None:
        root = pathlib.Path(root)
        if '..' in path.parts or root.is_symlink():
            raise ValueError('invalid declared credential path')
        path.relative_to(root)
        current = root
        for part in path.relative_to(root).parts:
            current = current / part
            if current.is_symlink():
                raise ValueError('symlink in declared credential path')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(fd)
        if before.st_uid != os.getuid() or not stat.S_ISREG(before.st_mode):
            raise ValueError('invalid credential file authority')
        if before.st_mode & 0o077 or before.st_size > 1024 * 1024:
            raise ValueError('invalid credential file permissions or size')
        data = os.read(fd, 1024 * 1024 + 1)
        if settings.file_revision(before) != settings.file_revision(os.fstat(fd)):
            raise ValueError('credential revision changed during read')
        return data
    finally:
        os.close(fd)


def claims(token):
    if not isinstance(token, str) or not 0 < len(token) <= 16384 or len(token.split('.')) != 3:
        raise ValueError('invalid credential token')
    part = token.split('.')[1]
    value = json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))
    if not isinstance(value, dict):
        raise ValueError('invalid credential claims')
    return value


def credential(data):
    try:
        tokens = json.loads(data)['tokens']
        if not isinstance(tokens, dict):
            raise ValueError('invalid credential tokens')
        owner = tokens['account_id']
        if not isinstance(owner, str) or not 0 < len(owner) <= 256:
            raise ValueError('invalid credential account identity')
        identity, access = claims(tokens['id_token']), claims(tokens['access_token'])
        identity_auth = identity['https://api.openai.com/auth']
        auth = access['https://api.openai.com/auth']
        if not isinstance(identity_auth, dict) or not isinstance(auth, dict):
            raise ValueError('invalid credential principal claims')
        if identity_auth.get('chatgpt_account_id') != owner or auth.get('chatgpt_account_id') != owner:
            raise ValueError('credential principal mismatch')
        expires = access['exp']
        if type(expires) not in (int, float) or expires <= time.time() + 60:
            raise ValueError('credential expired; read-only probe will not refresh')
        viewer_values = [auth.get('chatgpt_user_id'), auth.get('chatgpt_account_user_id')]
        viewer_values = [value for value in viewer_values if value is not None]
        if not viewer_values or not all(isinstance(value, str) and value for value in viewer_values):
            raise ValueError('owner viewer identity unavailable')
        return {'account_id': owner, 'token': tokens['access_token'],
                'viewer_ids': frozenset(viewer_values), 'email': identity.get('email'),
                'alias': hashlib.sha256(owner.encode()).hexdigest()[:8]}
    except (KeyError, TypeError, IndexError, AttributeError):
        raise ValueError('invalid credential structure') from None


class IdentityCollision(ValueError):
    pass


class CatalogHTTPError(ValueError):
    def __init__(self, status):
        self.status = status
        super().__init__('catalog HTTP status ' + str(status))


def stable_handle(account_id, profile_id):
    return settings.stable_handle(account_id, profile_id)


def _managed_ready(document):
    if not isinstance(document, dict):
        raise ValueError('invalid managed credential document')
    metadata = document.get('_cq')
    if '_cq' not in document:
        return True  # Existing codex-auth compatible records are read-only sources too.
    if (not isinstance(metadata, dict) or type(metadata.get('version')) is not int or metadata['version'] != 1
            or type(metadata.get('generation')) is not int or metadata['generation'] < 1
            or any(not isinstance(metadata.get(key), str) or not metadata[key]
                   for key in ('account_key', 'candidate_id', 'lineage_id', 'revision'))):
        raise ValueError('invalid managed credential metadata')
    if metadata.get('provenance') not in {'cq_oauth', 'system_borrowed', 'legacy_unknown'}:
        raise ValueError('unknown managed credential provenance')
    if metadata.get('refresh_ownership') not in {'cq_owned_never_exported', 'exported_to_system', 'unknown'}:
        raise ValueError('unknown managed credential ownership')
    if not isinstance(document.get('tokens'), dict):
        raise ValueError('managed credential tokens missing')
    revision = copy.deepcopy(document)
    revision['_cq']['revision'] = ''
    if not revision['_cq'].get('operation_id'):
        revision['_cq'].pop('operation_id', None)
    revision['tokens'].setdefault('refresh_token', '')
    encoded = json.dumps(revision, sort_keys=True, separators=(',', ':'), ensure_ascii=False)
    for value, escape in (('<', '\\u003c'), ('>', '\\u003e'), ('&', '\\u0026'),
                          ('\u2028', '\\u2028'), ('\u2029', '\\u2029')):
        encoded = encoded.replace(value, escape)
    if hashlib.sha256(encoded.encode()).hexdigest() != metadata['revision']:
        raise ValueError('managed credential revision mismatch')
    state = metadata.get('operation_state')
    if state == 'removing':
        return False
    if state != 'ready':
        raise ValueError('managed credential operation unavailable')
    return True


def _private_source(root, declared=False):
    info = root.lstat()
    mode = stat.S_IMODE(info.st_mode)
    unsafe_mode = bool(mode & 0o022) if declared else mode != 0o700
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or unsafe_mode:
        raise ValueError('invalid credential source directory')


def _inventory():
    system = credential(read_owned(SYSTEM))
    if (system['alias'] != settings.SYSTEM_ALIAS
            or (settings.SYSTEM_ACCOUNT_ID is not None and system['account_id'] != settings.SYSTEM_ACCOUNT_ID)):
        raise ValueError('system credential principal changed')
    accounts = {system['account_id']: system}
    aliases = {system['alias']: system['account_id']}
    failures = []

    def add(data):
        account = credential(data)
        previous = aliases.get(account['alias'])
        if previous is not None and previous != account['account_id']:
            raise IdentityCollision('account alias collision')
        aliases[account['alias']] = account['account_id']
        accounts.setdefault(account['account_id'], account)

    def failure(kind, error, document=None):
        outcome = {'status': 'unavailable', 'source': kind, 'failure_type': type(error).__name__}
        tokens = (document or {}).get('tokens', {})
        owner = tokens.get('account_id') if isinstance(tokens, dict) else None
        if isinstance(owner, str) and owner:
            outcome.update(owner_alias=hashlib.sha256(owner.encode()).hexdigest()[:8], owner_account_id=owner)
        failures.append(outcome)

    if CQ_ACCOUNTS.exists() or CQ_ACCOUNTS.is_symlink():
        try:
            _private_source(CQ_ACCOUNTS)
            paths = sorted(CQ_ACCOUNTS.glob('*.auth.json'))
        except (OSError, ValueError) as error:
            failure('cq', error)
            paths = []
        for path in paths:
            document = None
            try:
                data = read_owned(path, CQ_ACCOUNTS)
                document = json.loads(data)
                if _managed_ready(document):
                    add(data)
            except IdentityCollision:
                raise
            except (OSError, ValueError, KeyError, TypeError, IndexError) as error:
                failure('cq', error, document if isinstance(document, dict) else None)
    if SOURCE is not None and ((SOURCE / 'managed-codex-accounts.json').exists()
                               or (SOURCE / 'managed-codex-accounts.json').is_symlink()):
        try:
            _private_source(SOURCE, declared=True)
            manifest = json.loads(read_owned(SOURCE / 'managed-codex-accounts.json', SOURCE))
            if manifest['version'] != 3 or not isinstance(manifest['accounts'], list):
                raise ValueError('unsupported declared manifest')
            rows = manifest['accounts']
        except (OSError, ValueError, KeyError, TypeError) as error:
            failure('codexbar', error)
            rows = []
        for row in rows:
            document = None
            try:
                data = read_owned(pathlib.Path(row['managedHomePath']) / 'auth.json', SOURCE)
                if hashlib.sha256(data).hexdigest() != row['authFingerprint']:
                    raise ValueError('declared credential fingerprint changed')
                document = json.loads(data)
                account = credential(data)
                if row['providerAccountID'] != account['account_id'] or row['workspaceAccountID'] != account['account_id']:
                    raise ValueError('declared credential identity mismatch')
                add(data)
            except IdentityCollision:
                raise
            except (OSError, ValueError, KeyError, TypeError, IndexError) as error:
                failure('codexbar', error, document if isinstance(document, dict) else None)
    return system, list(accounts.values()), failures


def inventory():
    """Reread current owner credentials; never copy, write, or refresh a source."""
    system, accounts, _ = _inventory()
    return system, accounts


def discover():
    """Return existing owner-primary Dots and safe per-owner availability outcomes."""
    system, accounts, failures = _inventory()
    dots, outcomes = [], []
    for account in accounts:
        outcome = {'owner_alias': account['alias'], 'owner_account_id': account['account_id'],
                   'system': account['account_id'] == system['account_id']}
        try:
            value = get(account, '/backend-api/tbo/primary')
            selection, profile = value.get('selection'), value.get('profile')
            if selection is None or (isinstance(selection, dict) and selection.get('available') is False):
                outcome['status'] = 'no_dot'
            else:
                if (not isinstance(selection, dict) or selection.get('available') is not True
                        or not isinstance(profile, dict)):
                    raise ValueError('invalid owner primary response')
                profile_id, root = profile.get('id'), profile.get('active_root_thread_id')
                name = profile.get('display_name')
                if (not isinstance(profile_id, str) or not 0 < len(profile_id) <= 128
                        or not isinstance(root, str) or not 0 < len(root) <= 128
                        or selection.get('thread_id') != root or not isinstance(name, str)
                        or not 0 < len(name) <= 80 or any(ord(c) < 32 for c in name)):
                    raise ValueError('invalid owner primary identity')
                if profile.get('status') == 'deleting':
                    outcome['status'] = 'no_dot'
                else:
                    handle = stable_handle(account['account_id'], profile_id)
                    if any(dot['profile_id'] == profile_id or dot['root_thread_id'] == root for dot in dots):
                        raise IdentityCollision('native profile ownership collision')
                    dots.append({'handle': handle, 'owner_alias': account['alias'],
                                 'owner_account_id': account['account_id'], 'profile_id': profile_id,
                                 'root_thread_id': root, 'display_name': name})
                    outcome.update(status='ready', handle=handle)
        except IdentityCollision:
            raise
        except (OSError, ValueError, KeyError, TypeError, http.client.HTTPException) as error:
            outcome.update(status='unavailable', failure_type=type(error).__name__)
            if isinstance(error, CatalogHTTPError):outcome['http_status'] = error.status
        outcomes.append(outcome)
    healthy = {account['account_id'] for account in accounts}
    outcomes += [error for error in failures if error.get('owner_account_id') not in healthy]
    return {'dots': dots, 'accounts': outcomes}


def get(account, path, request_headers=None):
    context = ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)
    conn = http.client.HTTPSConnection('chatgpt.com', timeout=20, context=context)
    try:
        headers = {k: v for k, v in (request_headers or {}).items()
                   if k.lower() not in ['authorization', 'chatgpt-account-id', 'x-openai-account-routing-override', 'host', 'content-length', 'connection']}
        # Headless discovery needs the same client agent as the qualified native renderer.
        if not any(key.lower() == 'user-agent' for key in headers):
            headers['User-Agent'] = NATIVE_USER_AGENT
        headers.update({'Authorization': 'Bearer ' + account['token'],
                        'ChatGPT-Account-Id': account['account_id'],
                        'Accept': 'application/json', 'Accept-Encoding': 'identity'})
        conn.request('GET', path, headers=headers)
        response = conn.getresponse()
        body = response.read(1024 * 1024 + 1)
        if len(body) > 1024 * 1024:
            raise ValueError('catalog response too large')
        if response.status != 200:
            raise CatalogHTTPError(response.status)
        return json.loads(body)
    finally:
        conn.close()


def catalog(account, request_headers=None):
    items = []
    cursor = None
    seen = set()
    for _ in range(20):
        query = {'limit': 25, 'include_room_preview': 'false'}
        if cursor is not None:
            query['cursor'] = cursor
        page = get(account, '/backend-api/tbo?' + urllib.parse.urlencode(query), request_headers)
        items.extend(page['items'])
        cursor = page.get('cursor')
        if cursor is None:
            return items
        if cursor in seen:
            raise ValueError('catalog cursor repeated')
        seen.add(cursor)
    raise ValueError('catalog pagination exceeds proof scope')

def primary_roots(accounts, request_headers):
    if not set(PROOF_PROFILE_IDS) <= set(accounts):
        raise ValueError('Proof primary principals changed')
    roots={}
    for alias in PROOF_PROFILE_IDS:
        account=accounts[alias]
        if account['alias']!=alias:raise ValueError('Primary principal mismatch')
        value=get(account,'/backend-api/tbo/primary',request_headers)
        selection=value.get('selection');profile=value.get('profile')
        if (not isinstance(selection,dict) or selection.get('available') is not True
                or not isinstance(profile,dict) or profile.get('id')!=PROOF_PROFILE_IDS[alias]):
            raise ValueError('Primary profile outside proof scope')
        root=profile.get('active_root_thread_id')
        if not isinstance(root,str) or not root or len(root)>128 or selection.get('thread_id')!=root:
            raise ValueError('Primary root mismatch')
        if root in roots:raise ValueError('Primary root ownership collision')
        roots[root]=(alias,profile['id'])
    return roots


def fixture(request_headers, primary_items):
    system, accounts = inventory()
    profiles = []
    owners = {}
    summary = []
    for account in accounts:
        if account['alias'] not in PROOF_PROFILE_IDS:
            continue
        items = primary_items if account['account_id'] == system['account_id'] else catalog(account, request_headers)
        items = [profile for profile in items if profile.get('id') == PROOF_PROFILE_IDS[account['alias']]]
        summary.append({'owner_alias': account['alias'],
                        'system': account['account_id'] == system['account_id'],
                        'requested_secondary': account['account_id'] != system['account_id'],
                        'profiles': [{'name': p['display_name'],
                                      'root_present': p.get('active_root_thread_id') is not None,
                                      'room_present': p.get('messaging_room_id') is not None,
                                      'status': p.get('status')} for p in items]})
        for profile in items:
            if profile.get('status') == 'deleting':
                continue
            for key in [profile['id'], profile.get('active_root_thread_id'), profile.get('messaging_room_id')]:
                if key is None:
                    continue
                if key in owners and owners[key]['account_id'] != account['account_id']:
                    raise ValueError('resource ownership collision')
                owners[key] = account
            profiles.append(profile)
    return system, profiles, owners, summary
