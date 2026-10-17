"""Prepare and install the configured isolated Dots experiment."""
import argparse
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import shlex
import stat
import subprocess
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE / 'runtime'))
import settings

FILES = ('catalog.py', 'self_projection.py', 'celsius.py', 'probe.py',
         'realtime_probe.py', 'cloud_stdio.py', 'isolated_native.py',
         'native_dot_transport.py', 'native_executor_reuse.py',
         'native_null_executor.py', 'local_executor.py', 'launch.py',
         'stop_isolated.py', 'room_coordination.py', 'native_gate.py',
         'settings.py', 'awareness.py')
EXECUTABLES = {'cloud_stdio.py', 'native_null_executor.py'}
PYTHON_VERSION = '3.14'
WEBSOCKETS_VERSION = '17.2'


def __getattr__(name):
    aliases = {'TARGET': 'ROOT', 'APP': 'APP', 'APP_HASH': 'ASAR_SHA256'}
    if name in aliases:
        return getattr(settings, aliases[name])
    raise AttributeError(name)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def regular(path):
    value = path.lstat()
    if not stat.S_ISREG(value.st_mode) or value.st_uid != os.getuid():
        raise ValueError('Expected an owned regular file')
    return path


def app_check():
    info = plistlib.loads((settings.APP / 'Contents/Info.plist').read_bytes())
    if (info['CFBundleShortVersionString'], str(info['CFBundleVersion'])) != (settings.APP_VERSION, settings.APP_BUILD):
        raise ValueError('Native app version differs from configured pin')
    if digest(settings.APP / 'Contents/Resources/app.asar') != settings.ASAR_SHA256:
        raise ValueError('Native app renderer differs from configured pin')


def identity():
    return {'schema': 1, 'kind': 'isolated-dots-experiment',
            'target': str(settings.ROOT), 'app_path': str(settings.APP),
            'app_version': settings.APP_VERSION, 'app_build': settings.APP_BUILD,
            'app_sha256': settings.ASAR_SHA256, 'module_asset': settings.MODULE_ASSET,
            'python': PYTHON_VERSION, 'websockets': WEBSOCKETS_VERSION}


def prepare(output):
    output = pathlib.Path(output).absolute()
    if output.exists() or output.is_symlink():
        raise ValueError('Candidate destination already exists')
    # Resolve parents before checking write boundaries, including symlinked parents.
    output = output.resolve()
    home = pathlib.Path.home()
    protected = [settings.ROOT, settings.APP, settings.SOURCE, settings.SYSTEM.parent,
                 home / '.codex']
    protected += [home / 'Library/Application Support' / name for name in
                  ('Codex', 'ChatGPT', 'com.openai.codex', 'com.openai.chat', 'OpenAI')]
    if any(output == root.resolve() or root.resolve() in output.parents for root in protected):
        raise ValueError('Candidate must be outside runtime, app and credential directories')
    output.mkdir(mode=0o700, parents=True)
    hashes = {}
    for name in FILES:
        source = regular(HERE / 'runtime' / name)
        destination = output / name
        shutil.copyfile(source, destination)
        destination.chmod(0o700 if name in EXECUTABLES else 0o600)
        hashes[name] = digest(destination)
    manifest = {**identity(), 'files': hashes,
                'limitations': ['No native cross-account @ picker',
                                'Persistent secondary Reboot unsupported',
                                'No additional onboarding slots',
                                'Fresh local-task sandbox remains unqualified']}
    path = output / 'manifest.json'
    path.write_text(json.dumps(manifest, indent=2) + '\n')
    path.chmod(0o600)
    verify(output)
    return {'candidate': str(output), 'files': len(hashes), 'manifest_sha256': digest(path)}


def verify(candidate):
    candidate = pathlib.Path(candidate)
    if candidate.is_symlink() or not candidate.is_dir():
        raise ValueError('Candidate directory invalid')
    manifest = json.loads(regular(candidate / 'manifest.json').read_text())
    if (any(manifest.get(key) != value for key, value in identity().items())
            or set(manifest.get('files', {})) != set(FILES)
            or {p.name for p in candidate.iterdir()} != set(FILES) | {'manifest.json'}):
        raise ValueError('Candidate manifest or file scope invalid')
    for name, expected in manifest['files'].items():
        if digest(regular(candidate / name)) != expected:
            raise ValueError('Candidate source hash mismatch')
    return manifest


def process_identity(pid):
    """Match an owned process to the configured app or an exact runtime script."""
    result = subprocess.run(['/bin/ps', '-p', str(pid), '-o', 'uid=,command='],
                            capture_output=True, text=True, check=False)
    if result.returncode:
        return None
    parts = result.stdout.strip().split(maxsplit=1)
    if len(parts) != 2 or not parts[0].isdigit() or int(parts[0]) != os.getuid():
        return None
    try:
        args = shlex.split(parts[1])
    except ValueError:
        return None
    if not args:
        return None
    root = settings.ROOT
    if args[0] == str(settings.APP / 'Contents/MacOS/ChatGPT'):
        directories = [a.split('=', 1)[1] for a in args if a.startswith('--user-data-dir=')]
        if len(directories) == 1 and pathlib.Path(directories[0]).resolve() == root / 'user-data':
            return 'app'
        return None
    if args[0] != str(root / 'venv/bin/python'):
        return None
    remaining = args[1:]
    if remaining and remaining[0] == '-u':
        remaining = remaining[1:]
    if not remaining:
        return None
    script = pathlib.Path(remaining[0])
    if script.name not in FILES or script not in (pathlib.Path(script.name), root / script.name):
        return None
    if (root / script.name).is_symlink():
        return None
    cwd = subprocess.run(['/usr/sbin/lsof', '-a', '-p', str(pid), '-d', 'cwd', '-Fn'],
                         capture_output=True, text=True, check=False)
    directories = [line[1:] for line in cwd.stdout.splitlines() if line.startswith('n')]
    if cwd.returncode or len(directories) != 1 or pathlib.Path(directories[0]).resolve() != root:
        return None
    return script.name


def isolated_processes():
    lines = subprocess.check_output(['/bin/ps', '-axo', 'pid=,command='], text=True).splitlines()
    executables = {str(settings.APP / 'Contents/MacOS/ChatGPT'),
                   str(settings.ROOT / 'venv/bin/python')}
    matches = []
    for line in lines:
        parts = line.strip().split(maxsplit=1)
        if len(parts) != 2 or not parts[0].isdigit():
            continue
        try:
            args = shlex.split(parts[1])
        except ValueError:
            continue
        if args and args[0] in executables and process_identity(int(parts[0])):
            matches.append(int(parts[0]))
    return matches


def preflight(candidate):
    settings.validate_runtime_paths()
    verify(candidate)
    app_check()
    root = settings.ROOT
    if root.is_symlink() or not root.is_dir() or root.stat().st_uid != os.getuid():
        raise ValueError('Existing isolated research directory unavailable')
    for name in ('leaf-key.pem', 'leaf-cert.pem', 'probe-cert.pem', 'realtime-cert.pem', 'port'):
        regular(root / name)
    regular(root / 'venv/pyvenv.cfg')
    version = subprocess.check_output([str(root / 'venv/bin/python'), '-c',
        'import pathlib,sys\n'
        'if sys.prefix == sys.base_prefix or pathlib.Path(sys.prefix).resolve() != pathlib.Path(sys.argv[1]).resolve():\n'
        ' raise RuntimeError("Interpreter escaped isolated venv")\n'
        'import websockets\n'
        'print(str(sys.version_info.major)+"."+str(sys.version_info.minor)+" "+websockets.__version__)',
        str(root / 'venv')], text=True).strip()
    if version != PYTHON_VERSION + ' ' + WEBSOCKETS_VERSION:
        raise ValueError('Isolated dependency version mismatch')


def atomic_copy(source, destination, mode):
    temporary = destination.with_name(destination.name + '.release-tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(source.read_bytes())
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


def activate(candidate):
    preflight(candidate)
    if isolated_processes():
        raise ValueError('Stop isolated app and research Python processes before activation')
    settings.validate_runtime_paths()
    root = settings.ROOT
    rollback = root / ('rollback-' + str(time.time_ns()))
    rollback.mkdir(mode=0o700)
    previous = {}
    for name in FILES:
        path = root / name
        previous[name] = None
        if path.exists() or path.is_symlink():
            regular(path)
            shutil.copyfile(path, rollback / name)
            (rollback / name).chmod(stat.S_IMODE(path.stat().st_mode))
            previous[name] = digest(rollback / name)
    state = rollback / 'rollback.json'
    state.write_text(json.dumps({'target': str(root), 'files': previous}, indent=2) + '\n')
    state.chmod(0o600)
    try:
        for name in FILES:
            atomic_copy(pathlib.Path(candidate) / name, root / name,
                        0o700 if name in EXECUTABLES else 0o600)
    except Exception:
        restore(rollback)
        raise
    return {'installed': True, 'rollback': str(rollback), 'system_app_modified': False}


def restore(rollback):
    root = settings.ROOT
    rollback = pathlib.Path(rollback)
    if rollback.is_symlink() or rollback.parent.resolve() != root or not rollback.name.startswith('rollback-'):
        raise ValueError('Rollback outside isolated target')
    if isolated_processes():
        raise ValueError('Stop isolated app and research Python processes before rollback')
    state = json.loads(regular(rollback / 'rollback.json').read_text())
    if state.get('target') != str(root) or set(state.get('files', {})) != set(FILES):
        raise ValueError('Rollback manifest scope invalid')
    for name, expected in state['files'].items():
        if expected is not None and digest(regular(rollback / name)) != expected:
            raise ValueError('Rollback source hash mismatch')
    settings.validate_runtime_paths()
    for name, expected in state['files'].items():
        if expected is None:
            (root / name).unlink(missing_ok=True)
        else:
            atomic_copy(rollback / name, root / name, stat.S_IMODE((rollback / name).stat().st_mode))
    return {'restored': True, 'target': str(root)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=['prepare', 'verify', 'preflight', 'activate', 'rollback'])
    parser.add_argument('path')
    arguments = parser.parse_args()
    operations = {'prepare': prepare, 'verify': verify, 'preflight': preflight,
                  'activate': activate, 'rollback': restore}
    try:
        result = operations[arguments.operation](arguments.path)
        print(json.dumps(result or {'ready': True}))
    except subprocess.SubprocessError:
        parser.exit(1, 'Isolated runtime command failed\n')
    except Exception as error:
        parser.exit(1, type(error).__name__ + ': ' + str(error) + '\n')
