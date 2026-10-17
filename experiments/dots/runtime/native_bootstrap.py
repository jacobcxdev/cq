"""Bounded preparation inside the official app, followed by detached completion."""
import argparse
import fcntl
import importlib
import json
import os
import pathlib
import shlex
import signal
import socket
import stat
import subprocess
import sys
import time

import settings

sys.dont_write_bytecode = True
MAX_OUTPUT = 64 * 1024
PREPARE_SECONDS = 15
SIDECARS = ('probe.py', 'realtime_probe.py')


def private_file(path, limit=MAX_OUTPUT):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > limit):
            raise ValueError('Private startup state invalid')
        value = os.read(fd, limit + 1)
        if len(value) > limit or settings.file_revision(info) != settings.file_revision(os.fstat(fd)):
            raise ValueError('Private startup state changed')
        return value
    finally:
        os.close(fd)


def save(path, value):
    temporary = path.with_name(path.name + '.bootstrap-tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, separators=(',', ':'))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def lock(name):
    fd = os.open(settings.ROOT / name, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    stream = os.fdopen(fd, 'r+')
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise ValueError('Private startup lock invalid')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return stream
    except BaseException:
        stream.close()
        raise


def parent_identity(parent_pid, direct=False):
    if type(parent_pid) is not int or parent_pid <= 0 or (direct and os.getppid() != parent_pid):
        raise ValueError('Native startup parent invalid')
    expected = str(settings.APP / 'Contents/MacOS/ChatGPT')
    rows = subprocess.check_output(['/bin/ps', '-axo', 'pid=,uid=,command='], text=True, timeout=3)
    found = False
    for row in rows.splitlines():
        values = row.strip().split(maxsplit=2)
        if len(values) != 3 or not values[0].isdigit() or values[1] != str(os.getuid()):
            continue
        command = values[2]
        if command != expected and not command.startswith(expected + ' '):
            continue
        arguments = shlex.split(command[len(expected):])
        alternate = any(value.startswith(('--user-data-dir', '--profile-directory')) for value in arguments)
        if int(values[0]) == parent_pid:
            if alternate:
                raise ValueError('Native startup requires the normal profile')
            found = True
        elif not alternate:
            raise ValueError('Another normal desktop app is running')
    if not found:
        raise ValueError('Native startup parent unavailable')


def payload():
    root = settings.ROOT
    if (getattr(settings, 'INTEGRATION_MODE', 'isolated') != 'native'
            or pathlib.Path(os.environ['CQ_DOTS_CONFIG']).resolve() != root.parent / 'settings.json'
            or pathlib.Path(__file__).resolve() != root / 'native_bootstrap.py'):
        raise ValueError('Native startup installation invalid')
    settings.validate_runtime_paths()
    directory = root.parent / 'payload'
    info = directory.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('Native startup payload invalid')
    private_file(directory / 'release.py', 1024 * 1024)
    sys.path.insert(0, str(directory))
    release = importlib.import_module('release')
    if pathlib.Path(release.__file__).resolve() != directory / 'release.py':
        raise ValueError('Native startup payload import invalid')
    candidate = root.parent / 'candidate'
    release.preflight(candidate)
    manifest = release.verify(candidate)
    for name, expected in manifest['files'].items():
        if release.digest(release.regular(root / name)) != expected:
            raise ValueError('Native startup runtime differs from candidate')
    return release, release.digest(candidate / 'manifest.json')


def listeners(port):
    result = subprocess.run(['/usr/sbin/lsof', '-nP', '-iTCP:' + str(port),
                             '-sTCP:LISTEN', '-Fpn'], capture_output=True, text=True, timeout=3)
    if result.returncode not in (0, 1) or (result.returncode and result.stdout):
        raise ValueError('Native startup listener lookup unavailable')
    pids = set()
    pid = None
    for line in result.stdout.splitlines():
        if line.startswith('p') and line[1:].isdigit():
            pid = int(line[1:])
        elif line.startswith('n'):
            if pid is None or line[1:] != '127.0.0.1:' + str(port):
                raise ValueError('Native startup listener is not private loopback')
            pids.add(pid)
    return pids


def port(name, default=None):
    path = settings.ROOT / name
    if not path.exists() and not path.is_symlink() and default is not None:
        return default
    value = private_file(path, 16).decode('ascii').strip()
    if not value.isdigit() or not 1024 < int(value) < 65536:
        raise ValueError('Native startup port invalid')
    return int(value)


def spawn(script, arguments=()):
    root = settings.ROOT
    log = root / ('release-' + script + '.log')
    fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'ab') as stream:
        return subprocess.Popen([str(root / 'venv/bin/python'), '-u', str(root / script), *arguments],
                                cwd=root, stdin=subprocess.DEVNULL, stdout=stream, stderr=stream,
                                start_new_session=True)


def prepare(parent_pid):
    parent_identity(parent_pid, direct=True)
    release, manifest_hash = payload()
    root = settings.ROOT
    with lock('native-startup.lock'):
        processes = {name: [] for name in SIDECARS}
        for pid in release.isolated_processes():
            name = release.process_identity(pid)
            if name in processes:
                processes[name].append(pid)
        if any(len(pids) > 1 for pids in processes.values()):
            raise ValueError('Duplicate native sidecars')
        revision = settings.catalogue_revision()
        if any(processes.values()):
            previous = json.loads(private_file(root / 'native-startup.json'))
            if (previous.get('catalogue_revision') != revision or previous.get('manifest_sha256') != manifest_hash
                    or any(previous.get('sidecars', {}).get(name) != pids[0]
                           for name, pids in processes.items() if pids)):
                raise ValueError('Native sidecar readiness is stale')
        http_port = port('port')
        if http_port != 8000:
            raise ValueError('Native startup requires the qualified proxy port')
        realtime_port = port('realtime-port', 0)
        if not realtime_port:
            with socket.socket() as connection:
                connection.bind(('127.0.0.1', 0))
                realtime_port = connection.getsockname()[1]
            # The realtime server expects a plain decimal port file.
            path = root / 'realtime-port'
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, 'w') as stream:
                stream.write(str(realtime_port))
        ports = dict(zip(SIDECARS, (http_port, realtime_port)))
        started = {}
        try:
            for name in SIDECARS:
                existing = set(processes[name])
                active = listeners(ports[name])
                if active and active != existing:
                    raise ValueError('Native sidecar port is already in use')
                if not existing:
                    child = spawn(name)
                    started[name] = child
                    processes[name] = [child.pid]
            deadline = time.monotonic() + 8
            while True:
                ready = True
                for name in SIDECARS:
                    pid = processes[name][0]
                    if name in started and started[name].poll() is not None:
                        raise ValueError('Native sidecar exited during startup')
                    if release.process_identity(pid) != name or listeners(ports[name]) != {pid}:
                        ready = False
                if ready:
                    break
                if time.monotonic() >= deadline:
                    raise ValueError('Native sidecar readiness timed out')
                time.sleep(0.1)
            command = [str(root / 'venv/bin/python'), str(root / 'launch.py'),
                       '--prepare-native', '--parent-pid=' + str(parent_pid)]
            result = subprocess.run(command, capture_output=True, timeout=5)
            if result.returncode or len(result.stdout) > MAX_OUTPUT:
                raise ValueError('Native launch preparation failed')
            configuration = json.loads(result.stdout)
            if (not isinstance(configuration, dict) or set(configuration) != {'env', 'switches'}
                    or not isinstance(configuration['env'], dict) or not isinstance(configuration['switches'], dict)):
                raise ValueError('Native launch preparation response invalid')
            save(root / 'native-startup.json', {'app_pid': parent_pid, 'proxy_port': http_port,
                 'realtime_port': realtime_port, 'catalogue_revision': revision,
                 'manifest_sha256': manifest_hash, 'sidecars': {name: pids[0] for name, pids in processes.items()}})
            spawn('native_bootstrap.py', ('--finish', '--parent-pid', str(parent_pid)))
            return configuration
        except BaseException:
            # Only children created by this failed attempt are stopped. Existing
            # sidecars and the official desktop process are never signalled.
            for child in started.values():
                if child.poll() is None:
                    child.terminate()
            raise


def finish(parent_pid):
    parent_identity(parent_pid)
    payload()
    import native_debug
    import control
    with lock('native-finish.lock'):
        deadline = time.monotonic() + 90
        while True:
            parent_identity(parent_pid)
            try:
                identity = native_debug.identity(settings.ROOT)
                if identity.get('app_pid') != parent_pid or identity.get('debug_transport') != 'embedded':
                    raise ValueError('Native bridge identity differs')
                ready = control.status()
                break
            except (OSError, ValueError, subprocess.SubprocessError):
                if time.monotonic() >= deadline:
                    raise ValueError('Native app readiness timed out') from None
                time.sleep(0.5)
        return control.finish_start(ready)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--parent-pid', type=int, required=True)
    parser.add_argument('--finish', action='store_true')
    arguments = parser.parse_args()
    def expired(*_):
        raise TimeoutError('Native startup deadline exceeded')
    if not arguments.finish:
        signal.signal(signal.SIGALRM, expired)
        signal.setitimer(signal.ITIMER_REAL, PREPARE_SECONDS)
    try:
        result = finish(arguments.parent_pid) if arguments.finish else prepare(arguments.parent_pid)
        data = json.dumps(result, separators=(',', ':')).encode()
        if len(data) > MAX_OUTPUT:
            raise ValueError('Native startup response exceeds bound')
        if not arguments.finish:
            sys.stdout.buffer.write(data + b'\n')
        return 0
    except Exception:
        print('Native Dots startup unavailable; the desktop app can continue.', file=sys.stderr)
        return 1
    finally:
        if not arguments.finish:
            signal.setitimer(signal.ITIMER_REAL, 0)


if __name__ == '__main__':
    raise SystemExit(main())
