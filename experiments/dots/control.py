"""Lifecycle controls for the existing isolated research app only."""
import argparse
import json
import os
import pathlib
import signal
import subprocess
import time
import release

COMPONENTS = {'probe.py', 'realtime_probe.py', 'room_coordination.py', 'native_null_executor.py'}


def process(pid):
    identity = release.process_identity(pid)
    return identity if identity in COMPONENTS else None


def stop():
    root = release.TARGET
    python = root / 'venv/bin/python'
    # This helper validates the exact app executable, debug listener and user-data path.
    stopped = subprocess.run([str(python), str(root / 'stop_isolated.py')], cwd=root, capture_output=True, text=True)
    if stopped.returncode and any(release.process_identity(pid) == 'app'
                                  for pid in release.isolated_processes()):
        raise ValueError('Verified isolated app could not be stopped')
    for pid in release.isolated_processes():
        if process(pid) in COMPONENTS:
            os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 15
    while release.isolated_processes() and time.monotonic() < deadline:
        time.sleep(0.25)
    remaining = release.isolated_processes()
    if remaining:
        raise ValueError('Isolated processes have not stopped; no forced termination attempted')
    return {'stopped': True, 'system_app_modified': False}


def status():
    root = release.TARGET
    asset = 'app://-/assets/' + release.settings.MODULE_ASSET
    expression = "(async()=>{const m=await import(" + json.dumps(asset) + ");const c=await m.$wt.safeGet('/tbo');const p=await m.$wt.safeGet('/tbo/primary');return {profiles:c.items.map(x=>x.id),primary:p.profile?.id,available:p.selection?.available,workspaceError:document.body.innerText.includes('load workspace settings')};})()"
    code = 'import isolated_native,json;print(json.dumps(isolated_native.evaluate(' + repr(expression) + ')))'
    result = subprocess.run([str(root / 'venv/bin/python'), '-c', code],
        cwd=root, capture_output=True, text=True, timeout=40)
    if result.returncode:
        raise ValueError('Isolated native status unavailable')
    value = json.loads(result.stdout)
    expected = set(release.settings.PROOF_PROFILE_IDS.values())
    if set(value['profiles']) != expected or value['primary'] not in expected or not value['available'] or value['workspaceError']:
        raise ValueError('Native catalog or workspace status not ready')
    # Catalog proves reads. Delivery and computer video require separate live canaries.
    components = [process(pid) for pid in release.isolated_processes()]
    return {'native_catalog_ready': True, 'profiles': len(expected),
            'research_components': [name for name in components if name]}


def start(candidate):
    manifest = release.verify(candidate)
    release.preflight(candidate)
    root = release.TARGET
    for name, expected in manifest['files'].items():
        if release.digest(release.regular(root / name)) != expected:
            raise ValueError('Installed research sources differ from candidate')
    if release.isolated_processes():
        raise ValueError('Isolated research processes already running')
    release.settings.validate_runtime_paths()
    python = root / 'venv/bin/python'

    def spawn(name):
        log = root / ('release-' + name + '.log')
        fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'ab') as stream:
            return subprocess.Popen([str(python), '-u', str(root / name)], cwd=root,
                                    stdout=stream, stderr=stream, start_new_session=True)

    try:
        probe = spawn('probe.py'); realtime = spawn('realtime_probe.py')
        time.sleep(2)
        if probe.poll() is not None or realtime.poll() is not None:
            raise ValueError('Research proxy failed to start')
        subprocess.run([str(python), str(root / 'launch.py'), '--cloud-stdio',
                        '--computer-proxy', '--native-null-executor'], cwd=root, check=True, capture_output=True)
        deadline = time.monotonic() + 30
        while True:
            try:
                ready = status()
                break
            except (ValueError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise ValueError('Isolated native app startup failed')
                time.sleep(1)
        relay = spawn('room_coordination.py')
        time.sleep(0.5)
        if relay.poll() is not None:
            raise ValueError('Coordination relay failed to start')
        return {**ready, 'relay_started': True, 'system_app_modified': False}
    except Exception:
        stop()
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
