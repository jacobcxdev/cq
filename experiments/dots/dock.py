"""Install and run the Dock entry point without changing the signed native app."""
import json
import os
import pathlib
import plistlib
import shlex
import stat
import subprocess
import sys
import tempfile
import time

IDENTIFIER = 'dev.jacobcx.cq.chatgpt-launcher'
RECOVERY_DELAY = 40


def owned(path, directory=False):
    info = path.lstat()
    expected = stat.S_ISDIR if directory else stat.S_ISREG
    if not expected(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise ValueError('Dock launcher paths must be owned, without symlinks or shared writes')


def write(path, data, mode=0o600):
    fd, temporary = tempfile.mkstemp(prefix='.cq-dock-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        pathlib.Path(temporary).unlink(missing_ok=True)


def install(root, app):
    cq = pathlib.Path(os.environ.get('CQ_DOTS_CQ_PATH', ''))
    if not cq.is_absolute() or not cq.is_file() or not os.access(cq, os.X_OK):
        raise ValueError('Dock launcher requires the installed CQ executable')
    applications = pathlib.Path.home() / 'Applications'
    bundle = applications / 'ChatGPT via CQ.app'
    contents = bundle / 'Contents'
    directories = [applications, bundle, contents, contents / 'MacOS', contents / 'Resources']
    files = [contents / 'Info.plist', contents / 'MacOS/launch', contents / 'Resources/ChatGPT.icns']
    # Validate every existing destination before any replacement, including parent aliases.
    for path in directories + files:
        if path.exists() or path.is_symlink():
            owned(path, directory=path in directories)
    if bundle.exists():
        info = plistlib.loads((contents / 'Info.plist').read_bytes())
        if info.get('CFBundleIdentifier') != IDENTIFIER or info.get('CQDotsRoot') != str(root):
            raise ValueError('Dock launcher destination belongs to another app or installation')
    original = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
    icon = original.get('CFBundleIconFile', '')
    if not isinstance(icon, str) or not icon or pathlib.Path(icon).name != icon:
        raise ValueError('Native app icon path is invalid')
    if not icon.endswith('.icns'):
        icon += '.icns'
    icon_path = app / 'Contents/Resources' / icon
    if not icon_path.is_file() or icon_path.stat().st_size > 16 * 1024 * 1024:
        raise ValueError('Native app icon is unavailable')
    icon_data = icon_path.read_bytes()
    info = {'CFBundleIdentifier': IDENTIFIER, 'CFBundleName': 'ChatGPT via CQ',
            'CFBundleDisplayName': 'ChatGPT via CQ', 'CFBundlePackageType': 'APPL',
            'CFBundleExecutable': 'launch', 'CFBundleIconFile': 'ChatGPT.icns',
            'CFBundleVersion': '1', 'CFBundleShortVersionString': '1.0',
            'LSUIElement': True, 'CQDotsRoot': str(root)}
    command = [str(root / 'runtime/venv/bin/python'), '-I', '-B',
               str(root / 'payload/dock.py'), str(root), str(cq)]
    script = '#!/bin/sh\nexec ' + shlex.join(command) + '\n'
    for path in directories:
        path.mkdir(mode=0o700, exist_ok=True)
    write(files[2], icon_data)
    write(files[1], script.encode(), 0o700)
    write(files[0], plistlib.dumps(info))
    return bundle


def configuration(root):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
    import cli
    return cli.configure(pathlib.Path(root))


def recover_app(settings):
    import release
    if not release.normal_app_running():
        subprocess.run(['/usr/bin/open', '-a', str(settings.APP)], check=True, timeout=10)


def controller(root, cq):
    settings = configuration(root)
    if getattr(settings, 'INTEGRATION_MODE', 'isolated') != 'native':
        return 1, 'Dock launcher requires a native Dots installation.'
    # Independent of this process and the CLI: setup may continue after the app
    # has opened, but must never leave the user's other chats inaccessible.
    watchdog = None
    try:
        watchdog = subprocess.Popen([sys.executable, '-I', '-B', str(pathlib.Path(__file__).resolve()),
                                     '--recover', root], start_new_session=True,
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL)
        started = subprocess.run([cq, 'codex', 'dots', '--root', root, 'start', '--json'],
                                 capture_output=True, text=True)
        if started.returncode:
            recover_app(settings)
            try:
                message = json.loads(started.stderr)['error']
            except (ValueError, KeyError):
                message = 'CQ could not start Dots. Run cq codex dots status for details.'
            return started.returncode, message
        result = json.loads(started.stdout)
        if result.get('restart_required'):
            return 1, 'Quit ChatGPT, then reopen ChatGPT via CQ. Active work has not been interrupted.'
        if result.get('started') is False:
            return 1, result['reason']
        subprocess.run(['/usr/bin/open', '-a', str(settings.APP)], check=True, timeout=15)
        return 0, ''
    except (ValueError, OSError, KeyError, subprocess.SubprocessError):
        recover_app(settings)
        raise
    finally:
        if watchdog is not None and watchdog.poll() is None:
            watchdog.terminate()
            watchdog.wait(timeout=5)


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    if len(args) == 2 and args[0] == '--recover':
        time.sleep(RECOVERY_DELAY)
        try:
            settings = configuration(args[1])
            if settings.INTEGRATION_MODE != 'native':
                return 1
            recover_app(settings)
            return 0
        except (ValueError, OSError, subprocess.SubprocessError):
            return 1
    try:
        code, message = controller(args[0], args[1])
    except (ValueError, OSError, KeyError, subprocess.SubprocessError):
        code, message = 1, 'CQ could not start Dots. Run cq codex dots status for details.'
    if code:
        script = ('on run argv\n'
                  'display alert "ChatGPT via CQ" message (item 1 of argv) as critical buttons {"OK"}\n'
                  'end run')
        subprocess.run(['/usr/bin/osascript', '-e', script, message], check=False, timeout=300)
    return code


if __name__ == '__main__':
    raise SystemExit(main())
