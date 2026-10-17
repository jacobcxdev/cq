"""Qualified native startup patch, staged before an explicit bundle exchange.

The pinned entry and its enabled framework integrity seal change. Existing
mapped app files are never rewritten: installation and rollback exchange
complete, locally re-signed directories atomically.
"""
import copy
import ctypes
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import uuid
from xml.parsers.expat import ExpatError


FRAMEWORK_BUNDLE = 'Contents/Frameworks/Codex Framework.framework'
FRAMEWORK_BINARY = FRAMEWORK_BUNDLE + '/Versions/155.0.8059.80/Codex Framework'
HELPER_DIRECTORY = FRAMEWORK_BUNDLE + '/Versions/155.0.8059.80/Helpers'
HELPER_BINARIES = {
    'Codex (Alerts)': 'cdb6e5470fc98041ae550b05142281c5f3bf83f4d2e7e79bdb23c395f4b37b8b',
    'Codex (Aperitif Alerts)': '57b547bea28364e3e735616f1644bc2a08877422f5be383be79649fca4bf55ae',
    'Codex (Aperitif GPU)': 'a1ce3525531ff95721d8297f8f2ef8da7dfe55c168570eb9bb4f89c48c3bb8bd',
    'Codex (Aperitif Renderer)': '12810199c33ab8822e68953a7e245fa8debfcb4209694eace1b0a2882399bb4f',
    'Codex (Aperitif)': '991eaa8d276869f2939da7f7779374c8ab1f1488f86694c5efe07f1d404a16cd',
    'Codex (GPU)': '69525cc5ba63a1106994ec59bd480c2b1a7c4870418eaa34e471d63b322a97ad',
    'Codex (Renderer)': 'dea17fe18f8d570b6efe9157947d9eb46aa6ed1837ff524e44db24756bff0f30',
    'Codex (Service)': '26292216f0c38a1f725a20657c11fffc8f2d7e33766ccc3f16092664944dbcad'}
INTEGRITY_SENTINEL = b'AGbevlPCksUGKNL8TSn7wGmJEuJsXb2A'
PIN = {'app_version': '26.1013.40934', 'app_build': '20770',
       'asar_sha256': '5ddc4e48259aa6f979e04529b1a5a4ae54b0f5c4ad6db5c6e004924c10916ddb',
       'framework_slot_offset': 267230752,
       'bundle_files': {
           'Contents/Info.plist': '8520b12f2bcf41fe7bb2d7d0fca7043ed495497c5a5405aafea01933f1b53058',
           'Contents/MacOS/ChatGPT': '0e00971d153a24b34bcba44fce1253d1a68573e09441bb7717585094227966b4',
           'Contents/_CodeSignature/CodeResources': 'ffaf14d22258a7b0a8e032aa83103f657603e313c86bb307d4f6e26ea36efe82',
           FRAMEWORK_BINARY: '2306fadc440e2ba60052eae9a5de05835c2c6e16d6ef5aae800b09a2746aaeed',
           FRAMEWORK_BUNDLE + '/Versions/155.0.8059.80/_CodeSignature/CodeResources':
               '00de4e706c98ab7af2f9f05ce0847fc85f076287940e4a0e727fb86637d36147',
           **{HELPER_DIRECTORY + '/' + name + '.app/Contents/MacOS/' + name: value
              for name, value in HELPER_BINARIES.items()},
           **{HELPER_DIRECTORY + '/' + name + '.app/Contents/_CodeSignature/CodeResources':
              ('558717962557755302cdb0df9a8b4a492c8f5739c5b6403e27a385ab7615a16f' if 'Alerts' in name
               else '6686de10a28a2fe11b36cbb86dcbacc827cfc4ea116b4dabf1845e5aee629e9b')
              for name in HELPER_BINARIES}}}
ENTRY = '.vite/build/early-bootstrap.js'
ANCHOR = b'require("./rolldown-runtime-CPUxUITh.js");'
BLOCK_SIZE = 4 * 1024 * 1024
MAX_HEADER = 32 * 1024 * 1024
SIGNING_PROFILE = 'adhoc-local-v1'
STRIPPED_ENTITLEMENTS = frozenset({
    'com.apple.application-identifier', 'com.apple.developer.aps-environment',
    'com.apple.developer.team-identifier', 'com.apple.developer.usernotifications.communication',
    'com.apple.security.application-groups', 'keychain-access-groups'})
UNRESTRICTED_ENTITLEMENTS = frozenset({
    'com.apple.security.app-sandbox', 'com.apple.security.automation.apple-events',
    'com.apple.security.cs.allow-jit', 'com.apple.security.cs.allow-unsigned-executable-memory',
    'com.apple.security.cs.disable-library-validation', 'com.apple.security.device.audio-input',
    'com.apple.security.device.camera', 'com.apple.security.files.user-selected.read-write',
    'com.apple.security.network.client', 'com.apple.security.personal-information.calendars'})
LOST_CAPABILITIES = ['openai-team-identity', 'apple-push-notifications',
                     'communication-notifications', 'openai-application-groups',
                     'openai-keychain-groups', 'original-signature-keychain-acl']
RECEIPT_FIELDS = {'schema', 'state', 'app_path', 'installation_root', 'app_version',
                  'app_build', 'original_asar_sha256', 'patched_asar_sha256',
                  'header_sha256', 'entry', 'hook_path', 'hook_sha256',
                  'staged_app', 'backup_app', 'signing_profile', 'lost_capabilities',
                  'original_bundle_files', 'patched_bundle_files'}


def digest(path):
    value = hashlib.sha256()
    with pathlib.Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()


def _private_directory(path, create=False):
    path = pathlib.Path(path)
    if not path.is_absolute() or '..' in path.parts or path.is_symlink():
        raise ValueError('App patch directory must be an absolute non-symlink path')
    if create:
        path.mkdir(mode=0o700, parents=False, exist_ok=True)
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('App patch directory must be owned and mode 0700')
    return path.resolve()


def _regular(path, private=False):
    info = pathlib.Path(path).lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or (private and stat.S_IMODE(info.st_mode) != 0o600)):
        raise ValueError('App patch state must be an owned regular private file')


def _app(path):
    path = pathlib.Path(path)
    if not path.is_absolute() or '..' in path.parts or path.is_symlink() or not path.is_dir():
        raise ValueError('Native app path must be an absolute non-symlink directory')
    path = path.resolve()
    for name in ('Contents/Info.plist', 'Contents/Resources/app.asar'):
        if (path / name).is_symlink() or not (path / name).is_file():
            raise ValueError('Native app resources must be regular files')
    return path


def _paths(app, root, create=False):
    app = _app(app)
    root = _private_directory(root)
    if root.is_relative_to(app) or app.is_relative_to(root):
        raise ValueError('App patch installation must be outside the app')
    directory = _private_directory(root / 'app-patch', create=create)
    if directory.stat().st_dev != app.parent.stat().st_dev:
        raise ValueError('Native app staging must be on the destination filesystem')
    return app, root, directory


def read_asar(path):
    """Read Chromium Pickle header without loading the archive body."""
    path = pathlib.Path(path)
    with path.open('rb') as stream:
        prefix = stream.read(16)
        if len(prefix) != 16:
            raise ValueError('ASAR header is truncated')
        outer, header_size, payload_size, json_size = struct.unpack('<4I', prefix)
        if (outer != 4 or header_size != payload_size + 4
                or payload_size != (json_size + 7) // 4 * 4
                or not 0 < json_size <= MAX_HEADER
                or 8 + header_size > path.stat().st_size):
            raise ValueError('ASAR header is invalid')
        raw = stream.read(json_size)
    try:
        header = json.loads(raw)
    except (ValueError, UnicodeError):
        raise ValueError('ASAR header JSON is invalid') from None
    if not isinstance(header, dict) or not isinstance(header.get('files'), dict):
        raise ValueError('ASAR file catalogue is invalid')
    return header, 8 + header_size, hashlib.sha256(raw).hexdigest()


def _entry(header):
    node = header
    try:
        for component in ENTRY.split('/'):
            node = node['files'][component]
        if (not isinstance(node, dict) or type(node['size']) is not int
                or node['size'] < 0 or not isinstance(node['offset'], str)
                or not node['offset'].isdigit() or node.get('unpacked') or node.get('link')):
            raise ValueError()
        return node
    except (KeyError, TypeError, ValueError):
        raise ValueError('Qualified native startup entry is unavailable') from None


def _entry_bytes(path, header, base):
    entry = _entry(header)
    if entry['size'] > 1024 * 1024:
        raise ValueError('Qualified native startup entry is too large')
    offset = base + int(entry['offset'])
    if offset + entry['size'] > pathlib.Path(path).stat().st_size:
        raise ValueError('ASAR entry is outside the archive')
    with pathlib.Path(path).open('rb') as stream:
        stream.seek(offset)
        return stream.read(entry['size'])


def _integrity(data):
    return {'algorithm': 'SHA256', 'hash': hashlib.sha256(data).hexdigest(),
            'blockSize': BLOCK_SIZE,
            'blocks': [hashlib.sha256(data[i:i + BLOCK_SIZE]).hexdigest()
                       for i in range(0, len(data), BLOCK_SIZE)]}


def patch_asar(path, hook):
    """Append a replacement entry and rebuild offsets relative to the new header."""
    path = pathlib.Path(path)
    header, base, _ = read_asar(path)
    original = _entry_bytes(path, header, base)
    if not original.startswith(ANCHOR) or b'CQ_NATIVE_STARTUP' in original:
        raise ValueError('Qualified native startup anchor changed')
    prefix = ('/* CQ_NATIVE_STARTUP */ try{require(' + json.dumps(str(hook))
              + ').configure(' + json.dumps(str(pathlib.Path(hook).parent.parent))
              + ')}catch{}\n').encode()
    replacement = prefix + original
    updated = copy.deepcopy(header)
    entry = _entry(updated)
    entry.update(size=len(replacement), offset=str(path.stat().st_size - base),
                 integrity=_integrity(replacement))
    raw = json.dumps(updated, separators=(',', ':'), ensure_ascii=False).encode()
    payload = struct.pack('<I', len(raw)) + raw
    payload += b'\0' * (-len(payload) % 4)
    packed = struct.pack('<III', 4, len(payload) + 4, len(payload)) + payload
    temporary = path.with_name(path.name + '.cq-patch-tmp')
    mode = stat.S_IMODE(path.stat().st_mode)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(fd, 'wb') as output, path.open('rb') as source:
            output.write(packed)
            source.seek(base)
            shutil.copyfileobj(source, output, 1024 * 1024)
            output.write(replacement)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)
    return hashlib.sha256(raw).hexdigest()


def _integrity_dictionary_digest(info):
    integrity = info.get('ElectronAsarIntegrity')
    if (not isinstance(integrity, dict) or set(integrity) != {'Resources/app.asar'}
            or not isinstance(integrity['Resources/app.asar'], dict)):
        raise ValueError('Native ASAR integrity dictionary changed')
    value = integrity['Resources/app.asar']
    if (set(value) != {'algorithm', 'hash'} or value['algorithm'] != 'SHA256'
            or not isinstance(value['hash'], str) or len(value['hash']) != 64
            or any(c not in '0123456789abcdef' for c in value['hash'])):
        raise ValueError('Native ASAR integrity dictionary is invalid')
    # Electron v44 integrity_digest.mm hashes sorted UTF-8 path, algorithm, hash.
    return hashlib.sha256(('Resources/app.asar' + value['algorithm'] + value['hash']).encode()).digest()


def _framework_slot(path):
    """Require the qualified enabled version-one Mach-O integrity section."""
    with pathlib.Path(path).open('rb') as stream:
        header = stream.read(32)
        if len(header) != 32:
            raise ValueError('Native framework Mach-O header is truncated')
        magic, cpu, _, kind, count, size, _, _ = struct.unpack('<8I', header)
        if magic != 0xfeedfacf or cpu != 0x100000c or kind != 6 or count > 4096 or size > 1024 * 1024:
            raise ValueError('Native framework Mach-O format changed')
        commands = stream.read(size)
        position, matches = 0, []
        for _ in range(count):
            if position + 8 > len(commands):
                raise ValueError('Native framework load commands are truncated')
            command, length = struct.unpack_from('<II', commands, position)
            if length < 8 or position + length > len(commands):
                raise ValueError('Native framework load command is invalid')
            if command == 0x19:
                if length < 72:
                    raise ValueError('Native framework segment is truncated')
                sections = struct.unpack_from('<I', commands, position + 64)[0]
                if length != 72 + sections * 80:
                    raise ValueError('Native framework section table is invalid')
                for index in range(sections):
                    section, segment, _, section_size, offset = struct.unpack_from(
                        '<16s16sQQI', commands, position + 72 + index * 80)
                    if section.rstrip(b'\0') == b'__asar_integrity':
                        if segment.rstrip(b'\0') != b'__DATA_CONST' or section_size != 66:
                            raise ValueError('Native framework integrity section changed')
                        matches.append(offset)
            position += length
        if position != size or matches != [PIN['framework_slot_offset']]:
            raise ValueError('Native framework integrity slot moved')
        stream.seek(matches[0])
        slot = stream.read(66)
    if len(slot) != 66 or slot[:34] != INTEGRITY_SENTINEL + b'\x01\x01':
        raise ValueError('Native framework integrity validation is unavailable')
    return matches[0], slot[34:]


def _patch_framework(app, original_info, updated_info):
    path = pathlib.Path(app) / FRAMEWORK_BINARY
    if digest(path) != PIN['bundle_files'][FRAMEWORK_BINARY]:
        raise ValueError('Native framework is not the qualified patch version')
    offset, original_digest = _framework_slot(path)
    if original_digest != _integrity_dictionary_digest(original_info):
        raise ValueError('Native framework original integrity seal changed')
    with path.open('r+b') as stream:
        stream.seek(offset + 34)
        stream.write(_integrity_dictionary_digest(updated_info))
        stream.flush()
        os.fsync(stream.fileno())


def _bundle_files(app):
    app = pathlib.Path(app)
    for name in PIN['bundle_files']:
        path = app
        for component in pathlib.Path(name).parts:
            path = path / component
            if path.is_symlink():
                raise ValueError('Qualified native bundle files must not use symlinks')
        if not path.is_file():
            raise ValueError('Qualified native bundle file is unavailable')
    return {name: digest(app / name) for name in PIN['bundle_files']}


def _bundle_check(app, expected_hash, expected_files=None):
    app = _app(app)
    try:
        info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
    except (plistlib.InvalidFileException, ExpatError, ValueError, TypeError):
        raise ValueError('Native app property list is invalid') from None
    if not isinstance(info, dict):
        raise ValueError('Native app property list is not a dictionary')
    asar = app / 'Contents/Resources/app.asar'
    if (info.get('CFBundleShortVersionString') != PIN['app_version']
            or str(info.get('CFBundleVersion')) != PIN['app_build']
            or digest(asar) != expected_hash):
        raise ValueError('Native app is not the qualified patch version')
    header, base, header_hash = read_asar(asar)
    expected_integrity = {'algorithm': 'SHA256', 'hash': header_hash}
    if info.get('ElectronAsarIntegrity', {}).get('Resources/app.asar') != expected_integrity:
        raise ValueError('Native app ASAR header integrity changed')
    if _entry(header).get('integrity') != _integrity(_entry_bytes(asar, header, base)):
        raise ValueError('Native app startup entry integrity changed')
    if _bundle_files(app) != (PIN['bundle_files'] if expected_files is None else expected_files):
        raise ValueError('Native app bundle is not the qualified patch version')
    if _framework_slot(app / FRAMEWORK_BINARY)[1] != _integrity_dictionary_digest(info):
        raise ValueError('Native framework ASAR integrity seal changed')
    return header_hash


def _signing_entitlements(app):
    try:
        result = subprocess.run(['/usr/bin/codesign', '--display', '--entitlements', ':-', str(app)],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=30, check=False)
        if result.returncode or not 0 < len(result.stdout) <= 65536:
            raise ValueError('Native signing entitlements are unavailable')
        entitlements = plistlib.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, plistlib.InvalidFileException, ExpatError,
            ValueError, TypeError):
        raise ValueError('Native signing entitlements are unavailable or invalid') from None
    if not isinstance(entitlements, dict) or not all(isinstance(key, str) for key in entitlements):
        raise ValueError('Native signing entitlements must be a property-list dictionary')
    if set(entitlements) - STRIPPED_ENTITLEMENTS - UNRESTRICTED_ENTITLEMENTS:
        raise ValueError('Unknown native signing entitlement; app update is not qualified')
    result = {key: value for key, value in entitlements.items() if key in UNRESTRICTED_ENTITLEMENTS}
    result['com.apple.security.cs.disable-library-validation'] = True
    return result


def _codesign(app, sign=False, framework=False):
    temporary = None
    try:
        command = ['/usr/bin/codesign']
        if sign:
            command += ['--force', '--sign', '-', '--timestamp=none',
                        '--preserve-metadata=identifier,flags,runtime']
            if not framework:
                entitlements = _signing_entitlements(app)
                fd, temporary = tempfile.mkstemp(prefix='.cq-entitlements-', suffix='.plist',
                                                 dir=pathlib.Path(app).parent)
                os.fchmod(fd, 0o600)
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(plistlib.dumps(entitlements))
                    stream.flush()
                    os.fsync(stream.fileno())
                command += ['--entitlements', temporary]
        else:
            command += ['--verify', '--deep', '--strict']
        result = subprocess.run([*command, str(app)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                                timeout=120, check=False)
        if result.returncode:
            raise ValueError('Native app signature operation failed')
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError('Native app signature operation failed') from None
    finally:
        if temporary is not None:
            pathlib.Path(temporary).unlink(missing_ok=True)


def _ensure_stopped(app):
    executable = str(pathlib.Path(app) / 'Contents/MacOS/ChatGPT')
    try:
        result = subprocess.run(['/bin/ps', '-axo', 'pid=,uid=,comm='],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, timeout=10, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError('Cannot verify that the native app is stopped') from None
    if result.returncode:
        raise ValueError('Cannot verify that the native app is stopped')
    for row in result.stdout.splitlines():
        fields = row.strip().split(maxsplit=2)
        if (len(fields) != 3 or not fields[0].isdigit()
                or not fields[1].lstrip('-').isdigit() or fields[1].startswith('--')):
            raise ValueError('Native app process inventory is invalid')
        if int(fields[1]) == os.getuid() and fields[2] == executable:
            raise ValueError('Quit the native app before installing or rolling back its startup patch')


def _save(directory, receipt):
    path = directory / 'receipt.json'
    temporary = directory / ('receipt-' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(receipt, stream, indent=2)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _receipt(app, root, require_hook=True):
    app, root, directory = _paths(app, root)
    path = directory / 'receipt.json'
    _regular(path, private=True)
    if path.stat().st_size > 16384:
        raise ValueError('Native patch receipt is too large')
    receipt = json.loads(path.read_text())
    if (not isinstance(receipt, dict) or set(receipt) != RECEIPT_FIELDS
            or receipt.get('schema') != 1
            or receipt.get('state') not in ('prepared', 'installing', 'installed',
                                          'rolling_back', 'rolled_back')
            or receipt.get('app_path') != str(app)
            or receipt.get('installation_root') != str(root)
            or receipt.get('app_version') != PIN['app_version']
            or receipt.get('app_build') != PIN['app_build']
            or receipt.get('original_asar_sha256') != PIN['asar_sha256']
            or receipt.get('original_bundle_files') != PIN['bundle_files']
            or receipt.get('entry') != ENTRY
            or receipt.get('hook_path') != str(root / 'runtime/native_startup.cjs')
            or receipt.get('staged_app') != str(directory / 'staged.app')
            or receipt.get('backup_app') != str(directory / 'pristine.app')):
        raise ValueError('Native patch receipt scope changed')
    if (receipt.get('signing_profile') != SIGNING_PROFILE
            or receipt.get('lost_capabilities') != LOST_CAPABILITIES):
        raise ValueError('Native patch signing limitations changed')
    for field in ('patched_asar_sha256', 'header_sha256', 'hook_sha256'):
        value = receipt.get(field)
        if not isinstance(value, str) or len(value) != 64 or any(c not in '0123456789abcdef' for c in value):
            raise ValueError('Native patch receipt hash is invalid')
    if receipt['patched_asar_sha256'] == receipt['original_asar_sha256']:
        raise ValueError('Native patch receipt hashes must differ')
    bundle = receipt.get('patched_bundle_files')
    if (not isinstance(bundle, dict) or set(bundle) != set(PIN['bundle_files'])
            or any(not isinstance(value, str) or len(value) != 64
                   or any(c not in '0123456789abcdef' for c in value) for value in bundle.values())):
        raise ValueError('Native patch receipt bundle hashes are invalid')
    if require_hook:
        _private_directory(root / 'runtime')
        hook = pathlib.Path(receipt['hook_path'])
        _regular(hook)
        if digest(hook) != receipt.get('hook_sha256'):
            raise ValueError('Native startup hook changed')
    return receipt, directory


def _pair_state(receipt):
    """Resolve a journal intent using both complete, qualified bundle copies."""
    current = digest(pathlib.Path(receipt['app_path']) / 'Contents/Resources/app.asar')
    staged = digest(pathlib.Path(receipt['staged_app']) / 'Contents/Resources/app.asar')
    original, patched = receipt['original_asar_sha256'], receipt['patched_asar_sha256']
    if (current, staged) == (original, patched):
        state = 'rolled_back' if receipt['state'] == 'rolling_back' else 'prepared'
    elif (current, staged) == (patched, original):
        state = 'installed'
    else:
        raise ValueError('Native patch bundle pair changed')
    for field, expected in (('app_path', current), ('staged_app', staged), ('backup_app', original)):
        files = receipt['patched_bundle_files'] if expected == patched else receipt['original_bundle_files']
        header = _bundle_check(receipt[field], expected, files)
        if expected == patched and header != receipt['header_sha256']:
            raise ValueError('Native patch header changed')
    return state


def _recover(receipt, directory):
    if receipt['state'] not in ('installing', 'rolling_back'):
        return receipt
    state = _pair_state(receipt)
    _codesign(receipt['app_path'])
    _codesign(receipt['staged_app'])
    recovered = {**receipt, 'state': state}
    _save(directory, recovered)
    return recovered


def status(app, installation_root):
    """Read actual bundle state without creating files or requiring a live hook."""
    result = {'state': 'update_required', 'system_app_modified': None,
              'automatic_reapply_allowed': False}
    try:
        app = _app(app)
        current = digest(app / 'Contents/Resources/app.asar')
        if current == PIN['asar_sha256']:
            _bundle_check(app, PIN['asar_sha256'])
            result['system_app_modified'] = False
        root = _private_directory(installation_root)
        path = root / 'app-patch/receipt.json'
        if not path.exists() and not path.is_symlink():
            _bundle_check(app, PIN['asar_sha256'])
            result['state'] = 'not_installed'
            return result
        receipt, _ = _receipt(app, root, require_hook=False)
        if current == receipt['patched_asar_sha256']:
            result['system_app_modified'] = True
        raw_state = receipt['state']
        if raw_state == 'installed' and current == PIN['asar_sha256']:
            for field in ('app_path', 'staged_app', 'backup_app'):
                _bundle_check(receipt[field], PIN['asar_sha256'])
            result['automatic_reapply_allowed'] = True
            return result
        effective = _pair_state(receipt)
        if raw_state == 'rolled_back' and effective == 'prepared':
            result['state'] = 'rolled_back'
        elif raw_state in ('installing', 'rolling_back'):
            result['state'] = effective
        elif raw_state == effective:
            result['state'] = effective
        return result
    except (OSError, ValueError, TypeError, KeyError, plistlib.InvalidFileException, ExpatError):
        return result


def _remove_unpublished(path, inode):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return
    if stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and info.st_ino == inode:
        shutil.rmtree(path)


def _copy_bundle(source, destination):
    # Reserving the inode before copying distinguishes our partial copy from an
    # unrelated pre-existing path, even when copytree fails part-way through.
    destination.mkdir(mode=0o700)
    inode = destination.lstat().st_ino
    try:
        shutil.copytree(source, destination, symlinks=True, copy_function=shutil.copy2,
                        dirs_exist_ok=True)
    except BaseException:
        _remove_unpublished(destination, inode)
        raise
    return inode


def _stage_bundle(backup, stage, hook):
    inode = _copy_bundle(backup, stage)
    try:
        header_hash = patch_asar(stage / 'Contents/Resources/app.asar', hook)
        info_path = stage / 'Contents/Info.plist'
        original_info = plistlib.loads(info_path.read_bytes())
        info = copy.deepcopy(original_info)
        info['ElectronAsarIntegrity']['Resources/app.asar'] = {
            'algorithm': 'SHA256', 'hash': header_hash}
        info_path.write_bytes(plistlib.dumps(info))
        _patch_framework(stage, original_info, info)
        for name in HELPER_BINARIES:
            if name != 'Codex (Service)':  # Already permits the locally signed framework.
                _codesign(stage / HELPER_DIRECTORY / (name + '.app'), sign=True)
        _codesign(stage / FRAMEWORK_BUNDLE, sign=True, framework=True)
        _codesign(stage, sign=True)
        _codesign(stage)
        patched_hash = digest(stage / 'Contents/Resources/app.asar')
        bundle_files = _bundle_files(stage)
        _bundle_check(stage, patched_hash, bundle_files)
        return header_hash, patched_hash, bundle_files
    except BaseException:
        _remove_unpublished(stage, inode)
        raise


def _reapply(app, root, directory, receipt):
    _bundle_check(app, PIN['asar_sha256'])
    _codesign(app)
    _bundle_check(receipt['backup_app'], PIN['asar_sha256'])
    _codesign(receipt['backup_app'])
    candidate = directory / ('staged-next-' + uuid.uuid4().hex + '.app')
    header_hash, patched_hash, bundle_files = _stage_bundle(pathlib.Path(receipt['backup_app']), candidate,
                                                           pathlib.Path(receipt['hook_path']))
    if (header_hash != receipt['header_sha256'] or patched_hash != receipt['patched_asar_sha256']):
        raise ValueError('Reapplied native patch changed unexpectedly')
    stage = pathlib.Path(receipt['staged_app'])
    _bundle_check(stage, PIN['asar_sha256'])
    # Retain old inodes: a previous app process may still have mapped this copy.
    stage.rename(directory / ('retired-stage-' + uuid.uuid4().hex + '.app'))
    candidate.rename(stage)
    prepared = {**receipt, 'state': 'prepared', 'patched_bundle_files': bundle_files}
    _save(directory, prepared)
    return prepared


def _refresh_hook(receipt, directory):
    _private_directory(pathlib.Path(receipt['installation_root']) / 'runtime')
    hook = pathlib.Path(receipt['hook_path'])
    _regular(hook)
    current_hash = digest(hook)
    if current_hash == receipt['hook_sha256']:
        return receipt
    _ensure_stopped(receipt['app_path'])
    current = digest(pathlib.Path(receipt['app_path']) / 'Contents/Resources/app.asar')
    if receipt['state'] == 'installed' and current == receipt['original_asar_sha256']:
        for field in ('app_path', 'staged_app', 'backup_app'):
            _bundle_check(receipt[field], receipt['original_asar_sha256'])
    else:
        _pair_state(receipt)
    for field in ('app_path', 'staged_app', 'backup_app'):
        _codesign(receipt[field])
    refreshed = {**receipt, 'hook_sha256': current_hash}
    _save(directory, refreshed)
    return refreshed


def prepare(app, installation_root, update_hook=False):
    """Copy, patch and sign a qualified app without changing its installed copy."""
    app, root, directory = _paths(app, installation_root, create=True)
    if (directory / 'receipt.json').exists():
        receipt, _ = _receipt(app, root, require_hook=not update_hook)
        receipt = _recover(receipt, directory)
        if update_hook:
            receipt = _refresh_hook(receipt, directory)
        if receipt['state'] == 'installed':
            current = digest(app / 'Contents/Resources/app.asar')
            if current == PIN['asar_sha256']:
                return _reapply(app, root, directory, receipt)
            verify_installed(app, root, PIN['asar_sha256'])
        elif receipt['state'] in ('prepared', 'rolled_back'):
            _bundle_check(app, PIN['asar_sha256'])
            _bundle_check(receipt['staged_app'], receipt['patched_asar_sha256'], receipt['patched_bundle_files'])
            _bundle_check(receipt['backup_app'], PIN['asar_sha256'])
            _codesign(receipt['staged_app'])
            if receipt['state'] == 'rolled_back':
                receipt = {**receipt, 'state': 'prepared'}
                _save(directory, receipt)
        return receipt
    _bundle_check(app, PIN['asar_sha256'])
    _codesign(app)
    _private_directory(root / 'runtime')
    hook = root / 'runtime/native_startup.cjs'
    _regular(hook)
    stage, backup = directory / 'staged.app', directory / 'pristine.app'
    if stage.exists() or backup.exists() or stage.is_symlink() or backup.is_symlink():
        raise ValueError('Native patch staging directory is not empty')
    backup_inode, stage_inode = _copy_bundle(app, backup), None
    try:
        _bundle_check(backup, PIN['asar_sha256'])
        _codesign(backup)
        header_hash, patched_hash, bundle_files = _stage_bundle(backup, stage, hook)
        stage_inode = stage.lstat().st_ino
        receipt = {'schema': 1, 'state': 'prepared', 'app_path': str(app),
                   'installation_root': str(root), **{k: PIN[k] for k in ('app_version', 'app_build')},
                   'original_asar_sha256': PIN['asar_sha256'],
                   'original_bundle_files': dict(PIN['bundle_files']), 'patched_bundle_files': bundle_files,
                   'patched_asar_sha256': patched_hash, 'header_sha256': header_hash,
                   'entry': ENTRY, 'hook_path': str(hook), 'hook_sha256': digest(hook),
                   'staged_app': str(stage), 'backup_app': str(backup),
                   'signing_profile': SIGNING_PROFILE, 'lost_capabilities': list(LOST_CAPABILITIES)}
        _save(directory, receipt)
        return receipt
    except BaseException:
        if not (directory / 'receipt.json').exists():
            if stage_inode is not None:
                _remove_unpublished(stage, stage_inode)
            _remove_unpublished(backup, backup_inode)
        raise


def verify_installed(app, installation_root, original_hash):
    receipt, _ = _receipt(app, installation_root)
    if receipt['state'] != 'installed' or receipt['original_asar_sha256'] != original_hash:
        raise ValueError('Native patch is not installed for this qualified app')
    _verify_installed_contents(receipt)
    return receipt['patched_asar_sha256']


def _verify_installed_contents(receipt):
    if _bundle_check(receipt['app_path'], receipt['patched_asar_sha256'], receipt['patched_bundle_files']) != receipt['header_sha256']:
        raise ValueError('Native patch header changed')
    _bundle_check(receipt['backup_app'], receipt['original_asar_sha256'])
    _codesign(receipt['app_path'])


def _exchange(first, second):
    if sys.platform != 'darwin':
        raise ValueError('Atomic native app exchange requires macOS')
    libc = ctypes.CDLL(None, use_errno=True)
    rename = libc.renameatx_np
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    # AT_FDCWD=-2 and RENAME_SWAP=0x2 on macOS; neither directory is overwritten.
    if rename(-2, os.fsencode(first), -2, os.fsencode(second), 0x2) != 0:
        raise OSError(ctypes.get_errno(), 'Atomic native app exchange failed')


def install(receipt):
    stored, directory = _receipt(receipt['app_path'], receipt['installation_root'])
    if {**stored, 'state': receipt['state']} != receipt:
        raise ValueError('Native patch preparation changed')
    stored = _recover(stored, directory)
    if stored['state'] == 'installed' and receipt['state'] in ('prepared', 'installing', 'installed'):
        _verify_installed_contents(stored)
        return stored
    if stored['state'] != 'prepared' or receipt['state'] not in ('prepared', 'installing'):
        raise ValueError('Native patch preparation changed')
    _bundle_check(stored['app_path'], stored['original_asar_sha256'])
    _bundle_check(stored['staged_app'], stored['patched_asar_sha256'], stored['patched_bundle_files'])
    _bundle_check(stored['backup_app'], stored['original_asar_sha256'])
    _codesign(stored['staged_app'])
    _ensure_stopped(stored['app_path'])
    installed = {**stored, 'state': 'installed'}
    _save(directory, {**stored, 'state': 'installing'})
    swapped = False
    try:
        _exchange(stored['app_path'], stored['staged_app'])
        swapped = True
        _verify_installed_contents(installed)
        _save(directory, installed)
    except BaseException:
        if swapped:
            _exchange(stored['app_path'], stored['staged_app'])
        _save(directory, stored)
        raise
    return installed


def rollback(receipt):
    stored, directory = _receipt(receipt['app_path'], receipt['installation_root'], require_hook=False)
    if {**stored, 'state': receipt['state']} != receipt:
        raise ValueError('Native patch installation changed')
    stored = _recover(stored, directory)
    if stored['state'] == 'rolled_back' and receipt['state'] in ('installed', 'rolling_back', 'rolled_back'):
        _pair_state(stored)
        return stored
    if stored['state'] != 'installed' or receipt['state'] not in ('installed', 'rolling_back'):
        raise ValueError('Native patch installation changed')
    _verify_installed_contents(stored)
    _bundle_check(stored['staged_app'], stored['original_asar_sha256'])
    _codesign(stored['staged_app'])
    _ensure_stopped(stored['app_path'])
    rolled_back = {**stored, 'state': 'rolled_back'}
    _save(directory, {**stored, 'state': 'rolling_back'})
    swapped = False
    try:
        _exchange(stored['app_path'], stored['staged_app'])
        swapped = True
        _bundle_check(stored['app_path'], stored['original_asar_sha256'])
        _codesign(stored['app_path'])
        _save(directory, rolled_back)
    except BaseException:
        if swapped:
            _exchange(stored['app_path'], stored['staged_app'])
        _save(directory, stored)
        raise
    return rolled_back
