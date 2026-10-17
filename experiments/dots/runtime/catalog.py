"""Read-only, in-memory account catalog fixture for the isolated experiment."""
import base64
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
SYSTEM = settings.SYSTEM
PROOF_PROFILE_IDS = settings.PROOF_PROFILE_IDS



def read_owned(path, root=None):
    if root is not None:
        path.relative_to(root)
        current = root
        for part in path.relative_to(root).parts:
            current = current / part
            if current.is_symlink():
                raise ValueError('symlink in declared credential path')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
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
    part = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))


def credential(data):
    tokens = json.loads(data)['tokens']
    owner = tokens['account_id']
    identity = claims(tokens['id_token'])
    if identity['https://api.openai.com/auth']['chatgpt_account_id'] != owner:
        raise ValueError('credential principal mismatch')
    if claims(tokens['access_token'])['exp'] <= time.time() + 60:
        raise ValueError('credential expired; read-only probe will not refresh')
    auth = claims(tokens['access_token'])['https://api.openai.com/auth']
    if auth.get('chatgpt_account_id') != owner:
        raise ValueError('access token principal mismatch')
    viewer_ids = {auth.get('chatgpt_user_id'), auth.get('chatgpt_account_user_id')}
    viewer_ids.discard(None)
    if not viewer_ids or not all(isinstance(value, str) and value for value in viewer_ids):
        raise ValueError('owner viewer identity unavailable')
    return {'account_id': owner, 'token': tokens['access_token'],
            'viewer_ids': frozenset(viewer_ids),
            'email': identity.get('email'),
            'alias': hashlib.sha256(owner.encode()).hexdigest()[:8]}


def inventory():
    system = credential(read_owned(SYSTEM))
    if system['alias'] != settings.SYSTEM_ALIAS:
        raise ValueError('system credential principal changed')
    manifest = json.loads(read_owned(SOURCE / 'managed-codex-accounts.json', SOURCE))
    if manifest['version'] != 3:
        raise ValueError('unsupported declared manifest')
    accounts = {system['account_id']: system}
    for row in manifest['accounts']:
        if hashlib.sha256(row['providerAccountID'].encode()).hexdigest()[:8] not in PROOF_PROFILE_IDS:
            continue
        path = pathlib.Path(row['managedHomePath']) / 'auth.json'
        data = read_owned(path, SOURCE)
        if hashlib.sha256(data).hexdigest() != row['authFingerprint']:
            raise ValueError('declared credential fingerprint changed')
        if json.loads(data)['tokens']['account_id'] in accounts:
            continue
        account = credential(data)
        if row['providerAccountID'] != account['account_id'] or row['workspaceAccountID'] != account['account_id']:
            raise ValueError('declared credential identity mismatch')
        accounts.setdefault(account['account_id'], account)
    by_alias = {account['alias']: account for account in accounts.values()}
    if set(by_alias) != set(PROOF_PROFILE_IDS):
        raise ValueError('configured account principals unavailable')
    if by_alias[settings.SECONDARY_ALIAS]['email'] != settings.SECONDARY_SELECTOR:
        raise ValueError('secondary credential selector changed')
    return system, list(accounts.values())


def get(account, path, request_headers=None):
    context = ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)
    conn = http.client.HTTPSConnection('chatgpt.com', timeout=20, context=context)
    try:
        headers = {k: v for k, v in (request_headers or {}).items()
                   if k.lower() not in ['authorization', 'chatgpt-account-id', 'x-openai-account-routing-override', 'host', 'content-length', 'connection']}
        headers.update({'Authorization': 'Bearer ' + account['token'],
                        'ChatGPT-Account-Id': account['account_id'],
                        'Accept': 'application/json', 'Accept-Encoding': 'identity'})
        conn.request('GET', path, headers=headers)
        response = conn.getresponse()
        body = response.read(1024 * 1024 + 1)
        if len(body) > 1024 * 1024:
            raise ValueError('catalog response too large')
        if response.status != 200:
            raise ValueError('catalog HTTP status ' + str(response.status))
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
    if set(accounts)!=set(PROOF_PROFILE_IDS):
        raise ValueError('Proof primary principals changed')
    roots={}
    for alias,account in accounts.items():
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
        if account['account_id'] == system['account_id']:
            items = primary_items
        elif account['alias'] == settings.SECONDARY_ALIAS and account['email'] == settings.SECONDARY_SELECTOR:
            items = catalog(account, request_headers)
        else:
            continue
        summary.append({'owner_alias': account['alias'],
                        'system': account['account_id'] == system['account_id'],
                        'requested_secondary': account['alias'] == settings.SECONDARY_ALIAS,
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
