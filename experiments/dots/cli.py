"""Packaged Dots adapter for the normal native app or isolated research profile."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import stat
import subprocess
import sys
import time

sys.dont_write_bytecode = True

HERE = pathlib.Path(__file__).resolve().parent
PIN = {
    'app_version': '26.1013.40934',
    'app_build': '20770',
    'asar_sha256': '5ddc4e48259aa6f979e04529b1a5a4ae54b0f5c4ad6db5c6e004924c10916ddb',
    'module_asset': 'app-initial-90eb42395b7d.js',
}


def private_directory(path):
    path = pathlib.Path(path)
    if path.is_symlink():
        raise ValueError('Private installation path must not be a symlink')
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('Private installation directory must be owned and mode 0700')
    return path


def save(path, value):
    path = pathlib.Path(path)
    if path.is_symlink():
        raise ValueError('Private state must not be a symlink')
    temporary = path.with_name(path.name + '.install-tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            stream.write(json.dumps(value, indent=2) + '\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def validate_location(root, app, system_auth, accounts, source=None):
    root = pathlib.Path(root)
    if not root.is_absolute() or '..' in root.parts or root.is_symlink():
        raise ValueError('Installation root must be an absolute non-symlink path')
    root = root.resolve()
    home = pathlib.Path.home().resolve()
    protected = [pathlib.Path(app).resolve(), pathlib.Path(system_auth).resolve().parent,
                 pathlib.Path(accounts).resolve(), home / '.codex']
    if source:
        protected.append(pathlib.Path(source).resolve())
    protected += [home / 'Library/Application Support' / name for name in
                  ('Codex', 'ChatGPT', 'com.openai.codex', 'com.openai.chat', 'OpenAI')]
    if home.is_relative_to(root):
        raise ValueError('Installation must not contain the home directory')
    for boundary in protected:
        if root.is_relative_to(boundary) or boundary.is_relative_to(root):
            raise ValueError('Installation overlaps a protected app or credential path')
    return root


def app_pin(app, root=None):
    app = pathlib.Path(app)
    info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
    digest = hashlib.sha256((app / 'Contents/Resources/app.asar').read_bytes()).hexdigest()
    if (info.get('CFBundleShortVersionString') == PIN['app_version']
            and str(info.get('CFBundleVersion')) == PIN['app_build']
            and digest != PIN['asar_sha256'] and root is not None):
        sys.path.insert(0, str(HERE / 'runtime'))
        import app_patch
        app_patch.verify_installed(app, root, PIN['asar_sha256'])
        return dict(PIN)
    if (info.get('CFBundleShortVersionString') != PIN['app_version']
            or str(info.get('CFBundleVersion')) != PIN['app_build']
            or digest != PIN['asar_sha256']):
        raise ValueError('Native app is not a qualified Dots version; no app files changed')
    return dict(PIN)


def run_checked(command, *, timeout=120):
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith(('PYTHON', 'PIP_'))}
    environment.update(PYTHONDONTWRITEBYTECODE='1', PYTHONNOUSERSITE='1',
                       PIP_NO_CACHE_DIR='1', PIP_CONFIG_FILE=os.devnull)
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout,
                            env=environment)
    if result.returncode:
        raise ValueError('Private dependency or certificate provisioning failed')
    return result


def provision(root, mode='isolated'):
    runtime = private_directory(root / 'runtime')
    for name in (() if mode == 'native' else
                 ('user-data', 'codex-home', 'null-executor', 'null-executor/home',
                  'null-executor/tmp', 'null-executor/work', 'null-executor/room-relay')):
        private_directory(runtime / name)
    venv = runtime / 'venv'
    if venv.is_symlink():
        raise ValueError('Private Python environment must not be a symlink')
    if not venv.exists():
        if sys.version_info[:2] != (3, 14):
            raise ValueError('Dots installation requires Python 3.14')
        run_checked([sys.executable, '-m', 'venv', str(venv)])
    python = venv / 'bin/python'
    run_checked([str(python), '-m', 'pip', 'install', '--disable-pip-version-check',
                 '--quiet', 'websockets==17.2'], timeout=120)
    names = ('leaf-key.pem', 'leaf-cert.pem', 'probe-cert.pem', 'realtime-cert.pem')
    present = [(runtime / name).exists() for name in names]
    if any(present) and not all(present):
        raise ValueError('Private certificate set is incomplete')
    if not any(present):
        run_checked(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                     '-keyout', str(runtime / 'leaf-key.pem'),
                     '-out', str(runtime / 'leaf-cert.pem'), '-days', '365',
                     '-subj', '/CN=localhost', '-addext', 'basicConstraints=critical,CA:FALSE', '-addext',
                     'subjectAltName=DNS:localhost,IP:127.0.0.1,DNS:realtime.chatgpt.com,DNS:ws.chatgpt.com'])
        for name in ('probe-cert.pem', 'realtime-cert.pem'):
            shutil.copyfile(runtime / 'leaf-cert.pem', runtime / name)
    for name in names:
        path = runtime / name
        if path.is_symlink() or not path.is_file() or path.stat().st_uid != os.getuid():
            raise ValueError('Private certificate authority changed')
        path.chmod(0o600)
    if (runtime / 'port').is_symlink():
        raise ValueError('Proxy port must not be a symlink')
    if not (runtime / 'port').exists():
        (runtime / 'port').write_text('8000')
        (runtime / 'port').chmod(0o600)
    return runtime


def configure(root):
    # Ignore inherited research configuration, including when invoked from its shell.
    os.environ['CQ_DOTS_CONFIG'] = str(root / 'settings.json')
    sys.path.insert(0, str(HERE / 'runtime'))
    import settings
    settings.reload()
    return settings


def discover_configuration(config, discovery):
    """Keep known profiles during an outage, but remove explicitly absent owners."""
    statuses = discovery['accounts']
    uncertain_source = any('owner_alias' not in row for row in statuses)
    unavailable = {row['owner_alias'] for row in statuses
                   if row.get('status') == 'unavailable' and 'owner_alias' in row}
    authoritative = {row['owner_alias'] for row in statuses
                     if row.get('status') in ('ready', 'no_dot') and 'owner_alias' in row}
    dots = {}
    for handle, dot in config.get('dots', {}).items():
        if (dot['owner_alias'] not in authoritative
                and (uncertain_source or dot['owner_alias'] in unavailable)):
            dots[handle] = dot
    for row in discovery['dots']:
        dots[row['handle']] = {key: row[key] for key in
            ('owner_alias', 'owner_account_id', 'profile_id', 'root_thread_id', 'display_name')}
    return {**config, 'dots': dots}, statuses


def refresh(root):
    settings = configure(root)
    import catalog
    # catalog holds source boundaries from import time; each CLI operation is a new process.
    current = json.loads((root / 'settings.json').read_text())
    discovery = catalog.discover()
    updated, statuses = discover_configuration(current, discovery)
    changed = updated != current
    if changed:
        save(root / 'settings.json', updated)
    save(root / 'accounts.json', statuses)
    settings.reload()
    return changed, statuses


def candidate(root):
    configure(root)
    import release
    output = root / 'candidate'
    if output.exists():
        # Candidate is immutable for this installed payload. Reinstall replaces it only stopped.
        release.verify(output)
    else:
        release.prepare(output)
    return output


def install(args):
    if sys.platform != 'darwin':
        raise ValueError('Native Dots installation currently requires macOS')
    root = validate_location(args.root, args.app, args.system_auth, args.cq_accounts, args.account_source)
    pin = app_pin(args.app, root if args.mode == 'native' else None)
    private_directory(root)
    existing = root / 'settings.json'
    if existing.exists():
        configure(root)
        import release
        if release.isolated_processes():
            raise ValueError('Quit the desktop app and stop this Dots installation before reinstalling')
        prior = json.loads(existing.read_text())
    else:
        prior = {}
    # Credential claims are read in memory only. No credential material enters config.
    sys.path.insert(0, str(HERE / 'runtime'))
    # catalog requires settings at import time; write a bootstrap snapshot before reading it.
    config = {**pin, 'runtime_root': str(root / 'runtime'), 'app_path': str(pathlib.Path(args.app).resolve()),
              'system_auth': str(pathlib.Path(args.system_auth).resolve()),
              'cq_accounts': str(pathlib.Path(args.cq_accounts).resolve()),
              'system_alias': prior.get('system_alias', '00000000'), 'dots': prior.get('dots', {}),
              'debug_port': 0, 'integration_mode': args.mode}
    if args.account_source:
        config['account_source'] = str(pathlib.Path(args.account_source).resolve())
    # Derive system identity directly without loading the not-yet-complete runtime settings.
    import base64
    import stat as stat_module
    path = pathlib.Path(args.system_auth)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat_module.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o077 or info.st_size > 1024 * 1024):
            raise ValueError('System credential source must be an owned private regular file')
        material = json.loads(os.read(fd, 1024 * 1024 + 1))['tokens']
        owner = material['account_id']
        claim = material['id_token'].split('.')[1]
        identity = json.loads(base64.urlsafe_b64decode(claim + '=' * (-len(claim) % 4)))
        if identity['https://api.openai.com/auth']['chatgpt_account_id'] != owner:
            raise ValueError('System credential identity mismatch')
    finally:
        os.close(fd)
    if prior.get('system_account_id', owner) != owner:
        raise ValueError('Installation system account changed; choose a new isolated root')
    config.update(system_alias=hashlib.sha256(owner.encode()).hexdigest()[:8], system_account_id=owner)
    # Reject unsafe paths before creating runtime state or running dependencies.
    private_directory(root / 'runtime')
    save(existing, config)
    settings = configure(root)
    settings.validate_runtime_paths()
    provision(root, mode=args.mode)
    refresh(root)
    settings.reload()
    import release
    old = root / 'candidate'
    if old.exists():
        # The old candidate never owns setup or relay state.
        if old.is_symlink() or old.parent != root:
            raise ValueError('Candidate path is unsafe')
        shutil.rmtree(old)
    prepared = candidate(root)
    release.activate(prepared)
    if args.mode == 'native':
        import app_patch
        app_patch.prepare(settings.APP, root, update_hook=True)
    save(root / 'installation.json', {'schema': 1, 'installed': True, 'app': pin,
                                    'installed_at': time.time(), 'system_app_modified': False})
    return start(root)


def apply_native_patch(root):
    """Apply a prepared native hook only after the user's existing app exits."""
    settings = configure(root)
    if getattr(settings, 'INTEGRATION_MODE', 'isolated') != 'native':
        return None
    if not (root / 'app-patch/receipt.json').exists():
        return None  # Older manually launched installations retain their transport.
    import app_patch
    import release
    receipt = app_patch.prepare(settings.APP, root)
    if receipt['state'] == 'prepared':
        if release.normal_app_running():
            return {'app_patch': 'prepared', 'system_app_modified': False,
                    'restart_required': True}
        receipt = app_patch.install(receipt)
    return {'app_patch': receipt['state'], 'system_app_modified': receipt['state'] == 'installed'}


def native_restart_required(root, settings):
    try:
        launch = json.loads((settings.ROOT / 'launch.json').read_text())
        return launch.get('catalogue_revision') != settings.catalogue_revision()
    except (OSError, ValueError):
        return True


def restart_pending(control):
    return {'installed': True, 'started': False, 'restart_required': True,
            'reason': 'Quit the normal desktop app, then run cq codex dots start; CQ will not interrupt running work',
            **control.ensure_watcher(), 'system_app_modified': False}


def start(root, preserve_watcher=False):
    settings = configure(root)
    patch = apply_native_patch(root)
    if patch and patch.get('restart_required'):
        import control
        return {**restart_pending(control), **patch}
    if not settings.DOTS:
        import control
        watcher = control.ensure_watcher()
        return {'installed': True, 'started': False, 'reason': 'No onboarded Dots found; account discovery remains active',
                **watcher, 'system_app_modified': False}
    if settings.SYSTEM_ALIAS not in settings.DOT_OWNERS.values():
        raise ValueError('Onboard the system account Dot through the native or web UI before starting secondary Dots')
    import control
    if getattr(settings, 'INTEGRATION_MODE', 'isolated') == 'native':
        import release
        identities = [release.process_identity(pid) for pid in release.isolated_processes()]
        if release.normal_app_running():
            if 'app' in identities and not native_restart_required(root, settings):
                return control.finish_start()
            return restart_pending(control)
        # A user quit leaves sidecars behind; restart only those, never the app.
        if identities:
            control.stop(preserve_watcher=True)
        preserve_watcher = True
    return {**control.start(candidate(root), preserve_watcher=preserve_watcher), **(patch or {})}


def sync(root, preserve_watcher=False):
    settings = configure(root)
    import release
    import control
    identities = [release.process_identity(pid) for pid in release.isolated_processes()]
    running = any(identity != 'account_watch.py' for identity in identities)
    preserve_watcher = preserve_watcher or (not running and 'account_watch.py' in identities)
    changed, accounts = refresh(root)
    native = getattr(settings, 'INTEGRATION_MODE', 'isolated') == 'native'
    if native and release.normal_app_running() and (changed or native_restart_required(root, settings)):
        result = restart_pending(control)
    elif changed:
        if running:
            control.stop(preserve_watcher=preserve_watcher)
        # Source manifest identity excludes catalogue rows, so no runtime source rewrite is needed.
        result = start(root, preserve_watcher=preserve_watcher)
    elif running:
        result = control.finish_start()
    else:
        result = start(root, preserve_watcher=preserve_watcher)
    complete = all(row.get('status') in ('ready', 'no_dot') for row in accounts)
    retryable = any(row.get('status') == 'unavailable' and 'source' not in row
                    and (row.get('http_status') is None or row['http_status'] in (408, 429)
                         or row['http_status'] >= 500) for row in accounts)
    return {**result, 'catalogue_changed': changed, 'accounts': accounts,
            'discovery_complete': complete,
            'discovery_retryable': retryable or result.get('onboarding_required', False)}


def list_dots(root):
    settings = configure(root)
    import awareness
    states = {row['target']: row for row in awareness.summary()}
    return {'dots': [{'id': handle, 'name': settings.DOT_NAMES.get(handle, handle),
                      'account': settings.DOT_OWNERS[handle],
                      'setup': states.get(handle, {}).get('outcome', 'not_attempted')}
                     for handle in settings.DOTS]}


def native_patch_summary(root):
    """Report bundle state even when a transport operation has failed."""
    try:
        settings = configure(root)
        if getattr(settings, 'INTEGRATION_MODE', 'isolated') != 'native':
            return {'system_app_modified': False}
        import app_patch
        state = app_patch.status(settings.APP, root)
        result = {'app_patch': state['state'],
                  'system_app_modified': state['system_app_modified']}
        path = settings.ROOT / 'app-patch-status.json'
        if path.exists() or path.is_symlink():
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            try:
                info = os.fstat(fd)
                if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                        or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4096):
                    raise ValueError('Native update status must be a private regular file')
                update = json.loads(os.read(fd, 4097))
            finally:
                os.close(fd)
            if (not isinstance(update, dict) or set(update) - {'state', 'reason', 'action'}
                    or update.get('state') not in ('installed', 'waiting', 'action_required')
                    or not all(isinstance(value, str) for value in update.values())):
                raise ValueError('Native update status is invalid')
            result['app_update'] = update
        return result
    except (OSError, ValueError, KeyError, TypeError, AttributeError):
        return {'system_app_modified': None}


@contextlib.contextmanager
def operation_lock(root, wait_seconds=0):
    fd = os.open(root / 'operation.lock',
                 os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    with os.fdopen(fd, 'w') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('Dots operation lock must be owned and mode 0600')
        deadline = time.monotonic() + wait_seconds
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise ValueError('Another Dots operation is in progress; try again shortly') from None
                time.sleep(0.1)
        yield


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('install', 'preinstall', 'start', 'stop', 'status', 'sync', 'list', 'prime', 'restore-app'))
    parser.add_argument('target', nargs='?')
    parser.add_argument('--root', type=pathlib.Path, required=True)
    parser.add_argument('--app', default='/Applications/ChatGPT.app')
    parser.add_argument('--mode', choices=('native', 'isolated'), default='native')
    parser.add_argument('--system-auth', default=str(pathlib.Path.home() / '.codex/auth.json'))
    parser.add_argument('--cq-accounts', default=str(pathlib.Path.home() / '.codex/accounts'))
    parser.add_argument('--account-source')
    parser.add_argument('--all', action='store_true')
    parser.add_argument('--json', action='store_true')
    parser.add_argument('--watcher', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    root = args.root
    try:
        if args.root.is_symlink():
            raise ValueError('Installation root must not be a symlink')
        root = args.root.resolve()
        if args.operation == 'install':
            validate_location(args.root, args.app, args.system_auth, args.cq_accounts, args.account_source)
        if args.operation != 'install':
            private_directory(root)
            settings = configure(root)
            validate_location(root, settings.APP, settings.SYSTEM, settings.CQ_ACCOUNTS, settings.SOURCE)
        # Atomic snapshots let reads proceed while the watcher reconciles mutations.
        private_directory(root)
        lock = (contextlib.nullcontext() if args.operation in ('list', 'status') else
                operation_lock(root, wait_seconds=45 if args.operation == 'prime' else 0))
        with lock:
            if args.operation == 'preinstall':
                import release
                if release.isolated_processes():
                    raise ValueError('Quit the desktop app and stop this Dots installation before reinstalling')
                result = {'safe_to_extract': True}
            elif args.operation == 'install':
                result = install(args)
            elif args.operation == 'start':
                result = start(root)
            elif args.operation == 'sync':
                result = sync(root, preserve_watcher=args.watcher)
            elif args.operation == 'list':
                result = list_dots(root)
            elif args.operation == 'prime':
                import awareness
                result = {'priming': awareness.prime(args.target, all=args.all, manual=True)}
            elif args.operation == 'restore-app':
                import app_patch
                receipt, _ = app_patch._receipt(settings.APP, root, require_hook=False)
                restored = app_patch.rollback(receipt)
                result = {'app_patch': restored['state'], 'system_app_modified': False}
            else:
                import control
                result = {'stop': control.stop, 'status': control.status}[args.operation]()
        result.update(native_patch_summary(root))
        print(json.dumps(result))
        if args.operation == 'prime' and any(row['outcome'] != 'accepted' for row in result['priming']):
            return 1
        return 0
    except (ValueError, OSError, KeyError, subprocess.SubprocessError) as error:
        # External output, credential JSON and server bodies never become diagnostics.
        message = str(error) if isinstance(error, ValueError) else type(error).__name__
        print(json.dumps({'error': message, **native_patch_summary(root)}), file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
