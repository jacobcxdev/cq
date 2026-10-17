"""Validated private configuration for the isolated Dots runtime."""
import functools
import json
import os
import pathlib
import re
import stat
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    ROOT: pathlib.Path
    APP: pathlib.Path
    APP_VERSION: str
    APP_BUILD: str
    ASAR_SHA256: str
    MODULE_ASSET: str
    SOURCE: pathlib.Path
    SYSTEM: pathlib.Path
    SYSTEM_ALIAS: str
    SECONDARY_ALIAS: str
    SECONDARY_SELECTOR: str
    DOTS: dict
    DOT_OWNERS: dict
    PROOF_PROFILE_IDS: dict
    DEBUG_PORT: int


def _text(data, name, limit=256):
    value = data.get(name)
    if not isinstance(value, str) or not value or len(value) > limit or any(ord(c) < 32 for c in value):
        raise ValueError('invalid Dots setting: ' + name)
    return value


def _path(data, name):
    value = pathlib.Path(_text(data, name, 4096))
    if not value.is_absolute() or '..' in value.parts:
        raise ValueError('Dots path must be absolute: ' + name)
    return value


def _private_directory(path):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError('Dots configuration directory must be owned and mode 0700')


def file_revision(info):
    """File identity and write revision; a read may legitimately update atime."""
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def validate_runtime_paths(root=None):
    """Check mutable state boundaries without creating or changing anything."""
    root = pathlib.Path(root) if root is not None else get_settings().ROOT
    if not root.exists() and not root.is_symlink():
        return
    _private_directory(root)
    resolved = root.resolve()
    for name in ('user-data', 'codex-home', 'null-executor',
                 'null-executor/home', 'null-executor/tmp', 'null-executor/work',
                 'null-executor/room-relay'):
        path = root / name
        if path.exists() or path.is_symlink():
            _private_directory(path)
            if not path.resolve().is_relative_to(resolved):
                raise ValueError('Dots mutable state escapes the private runtime')
    launch = root / 'launch.json'
    if launch.exists() or launch.is_symlink():
        info = launch.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise ValueError('Dots launch state must be an owned regular file with mode 0600')


def load(path=None):
    """Read configuration only; never discover, copy, or refresh credentials."""
    if path is None:
        path = os.environ.get('CQ_DOTS_CONFIG')
    if not path:
        raise ValueError('CQ_DOTS_CONFIG must name a private configuration file')
    path = pathlib.Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('CQ_DOTS_CONFIG must be an absolute path')
    _private_directory(path.parent)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_uid != os.getuid()
                or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size > 1024 * 1024):
            raise ValueError('Dots configuration must be an owned regular file with mode 0600')
        raw = os.read(fd, 1024 * 1024 + 1)
        if file_revision(before) != file_revision(os.fstat(fd)) or len(raw) > 1024 * 1024:
            raise ValueError('Dots configuration changed during read')
    finally:
        os.close(fd)
    try:
        data = json.loads(raw)
    except (ValueError, UnicodeError):
        raise ValueError('Dots configuration must be valid JSON') from None
    fields = {'runtime_root', 'app_path', 'app_version', 'app_build', 'asar_sha256',
              'module_asset', 'account_source', 'system_auth', 'system_alias',
              'secondary_alias', 'secondary_selector', 'dots', 'debug_port'}
    if not isinstance(data, dict) or set(data) - fields:
        raise ValueError('unsupported Dots configuration fields')
    root = _path(data, 'runtime_root')
    source_tree = next((parent for parent in pathlib.Path(__file__).resolve().parents
                        if (parent / 'go.mod').is_file()), None)
    if source_tree is not None and root.resolve().is_relative_to(source_tree):
        raise ValueError('Dots runtime must be outside the source tree')
    if root.exists() or root.is_symlink():
        _private_directory(root)
    app = _path(data, 'app_path')
    source = _path(data, 'account_source')
    system = _path(data, 'system_auth')
    resolved_root = root.resolve()
    home = pathlib.Path.home().resolve()
    protected = [app, source, system.parent, home / '.codex']
    protected += [home / 'Library' / 'Application Support' / name for name in
                  ('Codex', 'ChatGPT', 'com.openai.codex', 'com.openai.chat', 'OpenAI')]
    if home.is_relative_to(resolved_root):
        raise ValueError('Dots runtime must not contain the home directory')
    for boundary in protected:
        boundary = boundary.resolve()
        if resolved_root.is_relative_to(boundary) or boundary.is_relative_to(resolved_root):
            raise ValueError('Dots runtime overlaps a protected application or credential path')
    system_alias = _text(data, 'system_alias')
    secondary_alias = _text(data, 'secondary_alias')
    if (not re.fullmatch(r'[0-9a-f]{8}', system_alias)
            or not re.fullmatch(r'[0-9a-f]{8}', secondary_alias)
            or system_alias == secondary_alias):
        raise ValueError('Dots requires two distinct account aliases')
    digest = _text(data, 'asar_sha256')
    if not re.fullmatch(r'[0-9a-f]{64}', digest):
        raise ValueError('invalid Dots setting: asar_sha256')
    asset = _text(data, 'module_asset')
    if not re.fullmatch(r'app-shared-[A-Za-z0-9_-]+\.js', asset):
        raise ValueError('invalid Dots setting: module_asset')
    port = data.get('debug_port', 0)
    if type(port) is not int or not 0 <= port <= 65535:
        raise ValueError('invalid Dots setting: debug_port')
    dots = data.get('dots')
    if not isinstance(dots, dict) or len(dots) != 2:
        raise ValueError('Dots requires two configured native profiles')
    targets, owners, profiles = {}, {}, {}
    roots = set()
    for name, dot in dots.items():
        if (not isinstance(name, str) or not name or len(name) > 80
                or any(ord(c) < 32 for c in name) or not isinstance(dot, dict)
                or set(dot) != {'owner_alias', 'profile_id', 'root_thread_id'}):
            raise ValueError('invalid Dots profile configuration')
        alias = _text(dot, 'owner_alias')
        profile = _text(dot, 'profile_id', 128)
        thread = _text(dot, 'root_thread_id', 128)
        if (alias not in {system_alias, secondary_alias} or alias in profiles
                or profile in profiles.values() or thread in roots):
            raise ValueError('Dots profile ownership must be unique')
        profiles[alias] = profile
        roots.add(thread)
        owners[name] = alias
        targets[name] = (profile, thread)
    validate_runtime_paths(root)
    return Settings(resolved_root, app, _text(data, 'app_version'), _text(data, 'app_build'),
                    digest, asset, source, system, system_alias, secondary_alias,
                    _text(data, 'secondary_selector'), targets, owners, profiles, port)


@functools.lru_cache(maxsize=1)
def get_settings():
    return load()


def __getattr__(name):
    if name in Settings.__dataclass_fields__:
        return getattr(get_settings(), name)
    raise AttributeError(name)
