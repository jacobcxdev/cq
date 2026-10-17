"""Reconcile isolated Dots when configured account source metadata changes."""
import fcntl
import hashlib
import json
import os
import pathlib
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
        delay = INTERVAL
        while True:
            time.sleep(delay)
            previous, event = watch_once(previous)
            if event is not None:
                print(json.dumps(event), flush=True)
                delay = min(delay * 2, 60) if event['event'] == 'account_sync_failed' else INTERVAL
            else:
                delay = INTERVAL


if __name__ == '__main__':
    main()
