"""One authenticated local exec-server per secondary Dot owner.

Provider credentials stay in the parent broker. Children receive only a local
registry capability and an independent CODEX_HOME. Native task sandboxing stays
enabled; an outer sandbox would prevent sandbox-exec from starting those tasks.
"""
import json
import os
import pathlib
import platform
import re
import signal
import subprocess
import threading
import urllib.parse
import uuid

import awareness
import catalog
import isolated_native
import local_executor
import settings


def directory():
    path = settings.ROOT / 'executors'
    awareness._directory(path)
    return path


def primary_environment():
    expression = """(async()=>{const m=await import('app://-/assets/MODULE');
const principal=await m.Ymr.safeGet('/tbo/primary');
if(principal.profile?.id!==PROFILE)throw Error('Native principal changed');
const state=await m.VAn().getState();
return {enabled:state.enabled,ready:state.ready,environment_id:state.environmentId};})()"""
    profiles = [p for h, (p, _) in settings.DOTS.items() if settings.DOT_OWNERS[h] == settings.SYSTEM_ALIAS]
    if len(profiles) != 1:
        raise ValueError('Native primary Dot unavailable')
    expression = expression.replace('MODULE', settings.MODULE_ASSET).replace('PROFILE', json.dumps(profiles[0]))
    value = isolated_native.evaluate(expression)
    if (not isinstance(value, dict) or value.get('ready') is not True or value.get('enabled') is not True
            or not valid_environment(value.get('environment_id'))):
        raise ValueError('Native local executor is not ready')
    return value['environment_id']


def valid_environment(value):
    return isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value) is not None


def owner_request(bound, host, method, path, body=None):
    system, accounts = catalog.inventory()
    matches = [a for a in accounts if a['alias'] == bound['alias']
               and a['account_id'] == bound['account_id'] and a['account_id'] != system['account_id']]
    if len(matches) != 1 or bound['alias'] not in settings.DOT_OWNERS.values():
        raise ValueError('Secondary executor owner unavailable')
    return local_executor.owner_request(matches[0], host, method, path, body, expected_alias=bound['alias'])


def start_owner(owner, primary):
    alias = owner['alias']
    profiles = {profile for handle, (profile, _) in settings.DOTS.items()
                if settings.DOT_OWNERS[handle] == alias}
    if alias == settings.SYSTEM_ALIAS or len(profiles) != 1 or not valid_environment(primary):
        raise ValueError('Secondary executor identity invalid')
    profile = profiles.pop()
    root = directory() / alias
    awareness._directory(root)
    for name in ['home', 'work', 'tmp']:
        awareness._directory(root / name)
    identity = root / 'installation.json'
    if identity.exists() or identity.is_symlink():
        installation = awareness._read(identity)['id']
        uuid.UUID(installation)
    else:
        installation = str(uuid.uuid4())
        awareness._save(identity, {'id': installation})
    body = json.dumps({'displayName': platform.node(), 'installation_id': installation,
                       'selected_capability_roots': []}).encode()
    status, content_type, data = owner_request(owner, 'chatgpt.com', 'POST',
                                              '/backend-api/flora/cca/executor', body)
    value = json.loads(data) if status == 200 and content_type.startswith('application/json') else {}
    environment = value.get('executor_id')
    if not valid_environment(environment):
        raise ValueError('Secondary executor registration HTTP ' + str(status))
    broker = local_executor.RegistryBroker(owner, environment, owner_request, expected_alias=alias)
    broker.start()
    env = local_executor.cli_environment(broker.capability)
    env.update(CODEX_HOME=str(root / 'home'), TMPDIR=str(root / 'tmp'))
    try:
        child = subprocess.Popen([local_executor.REAL, '-c', 'cli_auth_credentials_store="file"',
            'exec-server', '--remote', f'http://127.0.0.1:{broker.server.server_port}/api',
            '--environment-id', environment, '--exit-on-stdin-close'], cwd=root / 'work', env=env,
            stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except Exception:
        broker.close()
        raise
    record = {'owner_alias': alias, 'profile_id': profile, 'environment_id': environment,
              'primary_environment_id': primary, 'active': True, 'registered': False}
    return {'owner': owner, 'broker': broker, 'child': child, 'record': record, 'connect_attempted': False}


def refresh(worker):
    record, broker, child = worker['record'], worker['broker'], worker['child']
    record['active'] = child.poll() is None and not broker.stopped.is_set()
    with broker.lock:
        record['registered'] = any(e.get('operation') == 'register' and e.get('status') == 200 for e in broker.events)
    if record['active'] and record['registered'] and not worker['connect_attempted']:
        worker['connect_attempted'] = True
        try:
            owner = worker['owner']
            profile = catalog.get(owner, '/backend-api/tbo/primary')['profile']
            if profile['id'] != record['profile_id']:
                raise ValueError('Secondary profile changed')
            path = ('/backend-api/tbo/' + urllib.parse.quote(profile['id'], safe='') + '/computers/'
                    + record['environment_id'] + '/connect?'
                    + urllib.parse.urlencode({'expected_thread_id': profile['active_root_thread_id']}))
            status, _, _ = owner_request(owner, 'chatgpt.com', 'POST', path)
            record['connect_http'] = status
        except Exception as error:
            record['connect_error'] = type(error).__name__
    return record


def close(worker):
    child = worker['child']
    try:
        child.stdin.close()
    except OSError:
        pass
    try:
        child.wait(timeout=10)
    except subprocess.TimeoutExpired:
        child.terminate()
        try:
            child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)
    finally:
        worker['broker'].close()
        worker['record']['active'] = False


def mapping(alias):
    """Expose only an active, owner-bound environment alias; never invent status."""
    try:
        rows = awareness._read(settings.ROOT / 'executors' / 'state.json')['owners']
        if not isinstance(rows, dict):
            return None
        row = rows.get(alias)
        profiles = {p for h, (p, _) in settings.DOTS.items() if settings.DOT_OWNERS[h] == alias}
        if (not isinstance(row, dict) or alias == settings.SYSTEM_ALIAS
                or row.get('owner_alias') != alias or row.get('profile_id') not in profiles
                or row.get('active') is not True or row.get('registered') is not True
                or not valid_environment(row.get('environment_id'))
                or not valid_environment(row.get('primary_environment_id'))):
            return None
        return row
    except (OSError, ValueError, KeyError, TypeError):
        return None


def project_computers(alias, profile, data):
    row = mapping(alias)
    if row is None or row['profile_id'] != profile:
        return data
    value = json.loads(data)
    if (not isinstance(value, dict) or not isinstance(value.get('computers'), list)
            or any(not isinstance(c, dict) for c in value['computers'])):
        raise ValueError('Owner computer list invalid')
    for computer in value['computers']:
        if computer.get('kind') == 'user' and computer.get('environment_id') == row['environment_id']:
            # The native UI compares against its single global installation ID.
            # Only that ID is aliased; online/attached/permission fields stay real.
            computer['environment_id'] = row['primary_environment_id']
    return json.dumps(value).encode()


def main():
    if settings.INTEGRATION_MODE != 'native':
        raise ValueError('Secondary executors require native integration')
    root = directory()
    lock_fd = os.open(root / 'process.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    import fcntl
    with os.fdopen(lock_fd, 'w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        stop = threading.Event()
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        workers, failures = [], {}
        state = {'owners': {}}
        def save():
            state['owners'] = {w['record']['owner_alias']: w['record'] for w in workers}
            state['failures'] = failures
            awareness._save(root / 'state.json', state)
        try:
            primary = None
            for _ in range(30):
                if stop.is_set(): return
                try:
                    isolated_native.isolated_pid()
                    primary = primary_environment()
                    break
                except (OSError, ValueError):
                    stop.wait(2)
            if primary is None:
                raise ValueError('Native executor startup timed out')
            system, accounts = catalog.inventory()
            if system['alias'] != settings.SYSTEM_ALIAS:
                raise ValueError('Native account changed')
            for owner in accounts:
                alias = owner['alias']
                if alias == system['alias'] or alias not in settings.DOT_OWNERS.values(): continue
                try:
                    workers.append(start_owner(owner, primary))
                except Exception as error:
                    failures[alias] = type(error).__name__
            save()
            while not stop.wait(2):
                isolated_native.isolated_pid()
                if catalog.inventory()[0]['alias'] != settings.SYSTEM_ALIAS:
                    raise ValueError('Native account changed')
                for worker in workers: refresh(worker)
                save()
        finally:
            for worker in workers: close(worker)
            save()


if __name__ == '__main__':
    main()
