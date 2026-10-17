"""Reconcile accounts and restore a previously approved, qualified native hook."""
import fcntl
import hashlib
import json
import os
import pathlib
import plistlib
import shlex
import subprocess
import stat
import time

import settings

INTERVAL = 5


def _metadata(path):
    try:
        info = path.lstat()
        return [info.st_dev, info.st_ino, info.st_mode, info.st_size,
                info.st_mtime_ns, info.st_ctime_ns]
    except FileNotFoundError:
        return None


def fingerprint():
    """Read filesystem metadata only, never credential contents."""
    accounts = pathlib.Path(settings.CQ_ACCOUNTS)
    entries = []
    if accounts.is_dir() and not accounts.is_symlink():
        entries = [[path.name, _metadata(path)] for path in sorted(accounts.iterdir())]
    source = settings.SOURCE
    manifest = None if source is None else _metadata(pathlib.Path(source) / 'managed-codex-accounts.json')
    value = [_metadata(accounts), entries, manifest, _metadata(pathlib.Path(settings.SYSTEM))]
    return hashlib.sha256(json.dumps(value, separators=(',', ':')).encode()).hexdigest()


def sync():
    root = pathlib.Path(settings.ROOT)
    installation = root.parent
    command = [str(root / 'venv/bin/python'), str(installation / 'payload/cli.py'),
               'sync', '--root', str(installation), '--watcher']
    try:
        result = subprocess.run(command, cwd=root, capture_output=True, timeout=120)
        if result.returncode != 0:
            return False
        outcome = json.loads(result.stdout)
        return (isinstance(outcome, dict) and isinstance(outcome.get('discovery_complete'), bool)
                and isinstance(outcome.get('discovery_retryable'), bool)
                and not outcome['discovery_retryable'])
    except (OSError, subprocess.SubprocessError, ValueError, TypeError):
        return False


def watch_once(previous):
    """Keep an unsuccessful revision pending; discovery may safely retry next poll."""
    try:
        current = fingerprint()
    except OSError:
        return previous, {'event': 'account_sync_failed'}
    if current == previous:
        return previous, None
    if sync():
        return current, {'event': 'account_sync_complete'}
    return previous, {'event': 'account_sync_failed'}


def patch_fingerprint():
    app = pathlib.Path(settings.APP)
    value = [_metadata(app / name) for name in
             ('Contents/Info.plist', 'Contents/Resources/app.asar')]
    value.append(_metadata(pathlib.Path(settings.ROOT).parent / 'app-patch/receipt.json'))
    return hashlib.sha256(json.dumps(value, separators=(',', ':')).encode()).hexdigest()


def _patch_status(state, reason):
    actions = {'action_required': 'Update CQ for this app build, then run cq codex dots start.',
               'waiting': 'Quit the desktop app when convenient; CQ will not stop or restart it.'}
    if reason in ('explicit_start_required', 'patch_reapplication_failed'):
        actions['action_required'] = 'Run cq codex dots start to review and enable the native hook.'
    elif reason == 'patch_receipt_invalid':
        actions['action_required'] = 'Repair this CQ Dots installation before enabling its native hook.'
    if reason == 'operation_busy':
        actions['waiting'] = 'Wait for the current CQ operation to finish.'
    return {'state': state, 'reason': reason, **({'action': actions[state]} if state in actions else {})}


def _check_patch():
    import app_patch
    root = pathlib.Path(settings.ROOT).parent
    app = pathlib.Path(settings.APP)
    try:
        receipt, _ = app_patch._receipt(app, root)
    except (OSError, ValueError, TypeError):
        return _patch_status('action_required', 'patch_receipt_invalid')
    # An initial prepared receipt is not prior consent to activate the patch.
    # A failed reapplication also needs an explicit command, rather than a loop.
    if receipt['state'] != 'installed':
        return _patch_status('action_required', 'explicit_start_required')
    try:
        info_path = app / 'Contents/Info.plist'
        if info_path.is_symlink() or info_path.stat().st_size > 1024 * 1024:
            raise ValueError('Invalid native app metadata')
        info = plistlib.loads(info_path.read_bytes())
        if ((info.get('CFBundleShortVersionString'), str(info.get('CFBundleVersion')))
                != (app_patch.PIN['app_version'], app_patch.PIN['app_build'])):
            return _patch_status('action_required', 'unsupported_app_version')
        current = app_patch.digest(app / 'Contents/Resources/app.asar')
        if current == receipt['patched_asar_sha256']:
            app_patch.verify_installed(app, root, app_patch.PIN['asar_sha256'])
            return _patch_status('installed', 'verified')
        if current != app_patch.PIN['asar_sha256']:
            return _patch_status('action_required', 'unsupported_app_build')
        try:
            app_patch._ensure_stopped(app)
        except ValueError:
            return _patch_status('waiting', 'verified_app_exit_required')
        fd = os.open(root / 'operation.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
        with os.fdopen(fd, 'r+') as lock:
            metadata = os.fstat(fd)
            if (not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.getuid()
                    or stat.S_IMODE(metadata.st_mode) != 0o600):
                raise ValueError('Invalid native operation lock')
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return _patch_status('waiting', 'operation_busy')
            # The patch module owns recovery and rollback policy. An installed
            # receipt alone does not grant permission to exchange a new bundle.
            qualification = app_patch.status(app, root)
            if qualification.get('automatic_reapply_allowed') is not True:
                return _patch_status('action_required', 'explicit_start_required')
            prepared = app_patch.prepare(app, root)
            if prepared['state'] != 'prepared':
                raise ValueError('Native reapplication was not prepared')
            app_patch.install(prepared)
        return _patch_status('installed', 'reapplied')
    except (OSError, ValueError, TypeError, KeyError, plistlib.InvalidFileException):
        return _patch_status('action_required', 'patch_reapplication_failed')


def _save_patch_status(value):
    path = pathlib.Path(settings.ROOT) / 'app-patch-status.json'
    temporary = path.with_name(path.name + '.watch-tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, separators=(',', ':'))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def watch_patch_once(previous):
    """Unknown builds settle until changed; only deferred exit/lock checks retry."""
    receipt = pathlib.Path(settings.ROOT).parent / 'app-patch/receipt.json'
    if getattr(settings, 'INTEGRATION_MODE', 'isolated') != 'native' or not receipt.exists():
        return None, None
    try:
        current = patch_fingerprint()
        if previous and current == previous['revision'] and previous['status']['state'] != 'waiting':
            return previous, None
        status = _check_patch()
        state = {'revision': patch_fingerprint(), 'status': status}
        if previous and status == previous['status']:
            return state, None
        _save_patch_status(status)
        return state, {'event': 'native_patch_' + status['state'], **status}
    except (OSError, ValueError, TypeError):
        return previous, {'event': 'native_patch_check_failed'}


def _normal_app_open():
    """A lookup failure never grants permission to launch the app."""
    executable = str(pathlib.Path(settings.APP) / 'Contents/MacOS/ChatGPT')
    try:
        result = subprocess.run(['/bin/ps', '-axo', 'pid=,uid=,command='],
                                capture_output=True, text=True, timeout=3)
        if result.returncode:
            return False
        found = False
        for row in result.stdout.splitlines():
            values = row.strip().split(maxsplit=2)
            if len(values) != 3 or not values[0].isdigit() or not values[1].removeprefix('-').isdigit():
                return False
            command = values[2]
            if (values[1] == str(os.getuid())
                    and (command == executable or command.startswith(executable + ' '))):
                arguments = shlex.split(command[len(executable):])
                if not any(value.startswith(('--user-data-dir', '--profile-directory')) for value in arguments):
                    found = True
        return found
    except (OSError, ValueError, subprocess.SubprocessError):
        return False


def main():
    settings.validate_runtime_paths()
    root = pathlib.Path(settings.ROOT)
    fd = os.open(root / 'account-watch.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    with os.fdopen(fd, 'w') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('Account watcher lock must be owned and mode 0600')
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # Reconcile once after every watcher start, including changes made while stopped.
        previous = None
        patch_previous = None
        delay = INTERVAL
        while True:
            time.sleep(delay)
            patch_previous, patch_event = watch_patch_once(patch_previous)
            if patch_event is not None:
                print(json.dumps(patch_event), flush=True)
            if patch_previous and patch_previous['status']['state'] != 'installed':
                # Account reconciliation can launch the app; update handling must
                # leave it closed and must not repeatedly start an unknown build.
                delay = INTERVAL
                continue
            if (patch_previous and patch_previous['status'].get('reason') == 'reapplied'
                    and not _normal_app_open()):
                # Reapplying a hook does not authorise opening the app again.
                # Keep account changes pending until the user next opens it.
                delay = INTERVAL
                continue
            previous, event = watch_once(previous)
            if event is not None:
                print(json.dumps(event), flush=True)
                delay = min(delay * 2, 60) if event['event'] == 'account_sync_failed' else INTERVAL
            else:
                delay = INTERVAL


if __name__ == '__main__':
    main()
