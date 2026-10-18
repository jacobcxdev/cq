"""Lifecycle controls for the configured native or isolated Dots adapter."""
import argparse
import fcntl
import json
import os
import pathlib
import signal
import stat
import subprocess
import time
import release

COMPONENTS = {'probe.py', 'realtime_probe.py', 'room_coordination.py', 'native_null_executor.py', 'account_watch.py', 'native_debug.py', 'secondary_executors.py'}


def process(pid):
    identity = release.process_identity(pid)
    return identity if identity in COMPONENTS else None


def running(preserve_watcher=False):
    pids = release.isolated_processes()
    if preserve_watcher:
        pids = [pid for pid in pids if release.process_identity(pid) != 'account_watch.py']
    return pids


def stop(preserve_watcher=False):
    root = release.TARGET
    python = root / 'venv/bin/python'
    if getattr(release.settings, 'INTEGRATION_MODE', 'isolated') == 'native':
        if (release.normal_app_running()
                or any(release.process_identity(pid) == 'app' for pid in release.isolated_processes())):
            raise ValueError('Quit the normal desktop app before stopping Dots; CQ will not interrupt its tasks')
    # The helper validates the configured app identity before signalling it.
    stopped = subprocess.run([str(python), str(root / 'stop_isolated.py')], cwd=root, capture_output=True, text=True)
    if stopped.returncode and any(release.process_identity(pid) == 'app'
                                  for pid in release.isolated_processes()):
        raise ValueError('Verified isolated app could not be stopped')
    for pid in running(preserve_watcher):
        if process(pid) in COMPONENTS:
            os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 15
    while running(preserve_watcher) and time.monotonic() < deadline:
        time.sleep(0.25)
    remaining = running(preserve_watcher)
    if remaining:
        raise ValueError('Isolated processes have not stopped; no forced termination attempted')
    return {'stopped': True, 'system_app_modified': False}


def status():
    root = release.TARGET
    asset = 'app://-/assets/' + release.settings.MODULE_ASSET
    expression = "(async()=>{if(document.querySelector('[data-testid=\"onboarding-progress-bar\"]'))return {native_catalog_ready:false,onboarding_required:true};const m=await import(" + json.dumps(asset) + ");const c=await m.Ymr.safeGet('/tbo');const p=await m.Ymr.safeGet('/tbo/primary');return {profiles:c.items.map(x=>x.id),primary:p.profile?.id,available:p.selection?.available,workspaceError:document.body.innerText.includes('load workspace settings')};})()"
    code = 'import isolated_native,json;print(json.dumps(isolated_native.evaluate(' + repr(expression) + ')))'
    result = subprocess.run([str(root / 'venv/bin/python'), '-c', code],
        cwd=root, capture_output=True, text=True, timeout=40)
    if result.returncode:
        raise ValueError('Isolated native status unavailable')
    value = json.loads(result.stdout)
    if value.get('onboarding_required') is True and value.get('native_catalog_ready') is False:
        return {'native_catalog_ready': False, 'onboarding_required': True}
    expected = set(release.settings.PROOF_PROFILE_IDS.values())
    native = getattr(release.settings, 'INTEGRATION_MODE', 'isolated') == 'native'
    if (set(value['profiles']) != expected or value['primary'] not in expected
            or (not native and not value['available']) or value['workspaceError']):
        raise ValueError('Native catalog or workspace status not ready')
    # Catalog proves reads. Delivery and computer video require separate live canaries.
    components = [process(pid) for pid in release.isolated_processes()]
    return {'native_catalog_ready': True, 'profiles': len(expected),
            'research_components': [name for name in components if name]}


def reconcile_awareness(root, python):
    code = ('import awareness,json,native_room_cache;native_room_cache.install();'
            'print(json.dumps(awareness.reconcile()))')
    try:
        result = subprocess.run([str(python), '-c', code], cwd=root,
                                capture_output=True, text=True, timeout=120)
        if result.returncode == 0:
            outcomes = json.loads(result.stdout)
            if isinstance(outcomes, list) and all(isinstance(item, dict) for item in outcomes):
                return outcomes
    except (subprocess.SubprocessError, ValueError):
        pass
    # Persisted pending/unknown guards prevent retries after an uncertain subprocess result.
    return [{'target': handle, 'outcome': 'unknown', 'error': 'awareness_reconciliation_failed'}
            for handle in release.settings.DOTS]


def _spawn(root, name):
    log = root / ('release-' + name + '.log')
    fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'ab') as stream:
        return subprocess.Popen([str(root / 'venv/bin/python'), '-u', str(root / name)], cwd=root,
                                stdout=stream, stderr=stream, start_new_session=True)


def ensure_watcher():
    """Keep discovery active even when there are no onboarded native Dots."""
    release.settings.validate_runtime_paths()
    root = release.TARGET
    release.regular(root / 'account_watch.py')
    fd = os.open(root / 'account-watch-start.lock',
                 os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    with os.fdopen(fd, 'w') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('Account watcher start lock must be owned and mode 0600')
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        watchers = [pid for pid in release.isolated_processes()
                    if release.process_identity(pid) == 'account_watch.py']
        if len(watchers) > 1:
            raise ValueError('Multiple verified account watchers are running')
        if not watchers:
            watcher = _spawn(root, 'account_watch.py')
            time.sleep(0.5)
            if watcher.poll() is not None:
                raise ValueError('Account watcher failed to start')
        return {'account_watcher_started': True, 'account_watcher_existing': bool(watchers)}


def finish_start(ready=None):
    """Resume startup after native onboarding; awareness guards own send deduplication."""
    ready = status() if ready is None else ready
    if ready.get('onboarding_required'):
        return {**ready, **ensure_watcher(), 'system_app_modified': False}
    root = release.TARGET
    awareness = reconcile_awareness(root, root / 'venv/bin/python')
    relays = [pid for pid in release.isolated_processes()
              if release.process_identity(pid) == 'room_coordination.py']
    if len(relays) > 1:
        raise ValueError('Multiple verified coordination relays are running')
    if not relays:
        relay = _spawn(root, 'room_coordination.py')
        time.sleep(0.5)
        if relay.poll() is not None:
            raise ValueError('Coordination relay failed to start')
    executors = []
    if getattr(release.settings, 'INTEGRATION_MODE', 'isolated') == 'native':
        executors = [pid for pid in release.isolated_processes()
                     if release.process_identity(pid) == 'secondary_executors.py']
        if len(executors) > 1:
            raise ValueError('Multiple verified secondary executor managers are running')
        if not executors and any(owner != release.settings.SYSTEM_ALIAS
                                 for owner in release.settings.DOT_OWNERS.values()):
            manager = _spawn(root, 'secondary_executors.py')
            time.sleep(0.5)
            if manager.poll() is not None:
                raise ValueError('Secondary executor manager failed to start')
    return {**ready, 'relay_started': True, 'relay_existing': bool(relays),
            'secondary_executors_existing': bool(executors),
            **ensure_watcher(), 'awareness': awareness, 'system_app_modified': False}


def start(candidate, preserve_watcher=False):
    manifest = release.verify(candidate)
    release.preflight(candidate)
    root = release.TARGET
    for name, expected in manifest['files'].items():
        if release.digest(release.regular(root / name)) != expected:
            raise ValueError('Installed research sources differ from candidate')
    if running(preserve_watcher):
        raise ValueError('Isolated research processes already running')
    release.settings.validate_runtime_paths()
    python = root / 'venv/bin/python'


    try:
        probe = _spawn(root, 'probe.py'); realtime = _spawn(root, 'realtime_probe.py')
        time.sleep(2)
        if probe.poll() is not None or realtime.poll() is not None:
            raise ValueError('Research proxy failed to start')
        subprocess.run([str(python), str(root / 'launch.py'), '--cloud-stdio',
                        '--computer-proxy'], cwd=root, check=True, capture_output=True)
        deadline = time.monotonic() + 90
        while True:
            try:
                ready = status()
                break
            except (ValueError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise ValueError('Isolated native app startup failed')
                time.sleep(1)
        return finish_start(ready)
    except Exception:
        # Native startup may already have opened the user's normal profile.
        # Preserve both that app and its transports instead of hiding the cause
        # behind stop's intentional refusal to interrupt normal-profile work.
        native = getattr(release.settings, 'INTEGRATION_MODE', 'isolated') == 'native'
        app_alive = native and (release.normal_app_running()
                                or any(release.process_identity(pid) == 'app'
                                       for pid in release.isolated_processes()))
        if not app_alive:
            stop(preserve_watcher=preserve_watcher)
        raise


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=['start', 'stop', 'status'])
    parser.add_argument('candidate', nargs='?')
    arguments = parser.parse_args()
    try:
        if arguments.operation == 'start':
            if not arguments.candidate:
                raise ValueError('Start requires a verified candidate')
            result = start(arguments.candidate)
        else:
            result = {'stop': stop, 'status': status}[arguments.operation]()
        print(json.dumps(result))
    except subprocess.SubprocessError:
        parser.exit(1, 'Isolated runtime command failed\n')
    except Exception as error:
        parser.exit(1, type(error).__name__ + ': ' + str(error) + '\n')
