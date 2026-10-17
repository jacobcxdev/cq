"""Synthetic archives and copied test apps only; no installed app mutation."""
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import struct
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import app_patch


def archive(path, startup):
    other = b'unchanged other asset'
    header = {'files': {'.vite': {'files': {'build': {'files': {
        'early-bootstrap.js': {'size': len(startup), 'offset': '0',
                               'integrity': app_patch._integrity(startup)}}}}},
        'other.txt': {'size': len(other), 'offset': str(len(startup)),
                      'integrity': app_patch._integrity(other)}}}
    raw = json.dumps(header, separators=(',', ':')).encode()
    payload = struct.pack('<I', len(raw)) + raw
    payload += b'\0' * (-len(payload) % 4)
    path.write_bytes(struct.pack('<III', 4, len(payload) + 4, len(payload))
                     + payload + startup + other)
    return hashlib.sha256(raw).hexdigest()


def framework(path, info):
    offset = 256
    segment = struct.pack('<II16sQQQQIIII', 0x19, 152, b'__DATA_CONST',
                          0, 66, offset, 66, 1, 1, 1, 0)
    section = struct.pack('<16s16sQQIIIIIIII', b'__asar_integrity', b'__DATA_CONST',
                          0, 66, offset, 0, 0, 0, 0, 0, 0, 0)
    data = struct.pack('<8I', 0xfeedfacf, 0x100000c, 0, 6, 1, 152, 0, 0) + segment + section
    path.parent.mkdir(parents=True)
    path.write_bytes(data + b'\0' * (offset - len(data)) + app_patch.INTEGRITY_SENTINEL
                     + b'\x01\x01' + app_patch._integrity_dictionary_digest(info) + b'unchanged trailer')
    return offset


class AppPatchTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = pathlib.Path(temporary.name).resolve()
        self.app = self.base / 'Native.app'
        (self.app / 'Contents/Resources').mkdir(parents=True)
        self.asar = self.app / 'Contents/Resources/app.asar'
        self.original = app_patch.ANCHOR + b'const unchanged=true;'
        header_hash = archive(self.asar, self.original)
        self.info = self.app / 'Contents/Info.plist'
        self.info.write_bytes(plistlib.dumps({
            'CFBundleShortVersionString': 'test-version', 'CFBundleVersion': '123',
            'ElectronAsarIntegrity': {'Resources/app.asar': {
                'algorithm': 'SHA256', 'hash': header_hash}}}))
        self.original_hash = app_patch.digest(self.asar)
        framework_offset = framework(self.app / app_patch.FRAMEWORK_BINARY,
                                     plistlib.loads(self.info.read_bytes()))
        for name in app_patch.PIN['bundle_files']:
            path = self.app / name
            if not path.exists():
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b'original signed fixture')
        bundle_files = {name: app_patch.digest(self.app / name) for name in app_patch.PIN['bundle_files']}
        self.root = self.base / 'cq-private'
        self.root.mkdir(mode=0o700)
        (self.root / 'runtime').mkdir(mode=0o700)
        self.hook = self.root / 'runtime/native_startup.cjs'
        self.hook.write_text('exports.configure = function(root) {};')
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(app_patch, 'PIN', {
            'app_version': 'test-version', 'app_build': '123',
            'asar_sha256': self.original_hash, 'framework_slot_offset': framework_offset,
            'bundle_files': bundle_files}).start()
        self.real_codesign = app_patch._codesign
        self.real_stopped = app_patch._ensure_stopped
        self.codesign = mock.patch.object(app_patch, '_codesign').start()
        self.stopped = mock.patch.object(app_patch, '_ensure_stopped').start()
        self.exchange = mock.patch.object(app_patch, '_exchange', side_effect=self.swap).start()

    def swap(self, first, second):
        first, second = pathlib.Path(first), pathlib.Path(second)
        temporary = first.with_name(first.name + '.test-swap')
        first.rename(temporary)
        second.rename(first)
        temporary.rename(second)

    def prepare(self):
        return app_patch.prepare(self.app, self.root)

    def test_preparation_preserved_installed_bytes_and_other_assets(self):
        original_info = self.info.read_bytes()
        original_inode = self.asar.stat().st_ino
        original_framework = (self.app / app_patch.FRAMEWORK_BINARY).read_bytes()
        receipt = self.prepare()
        self.assertEqual(receipt['state'], 'prepared')
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        self.assertEqual(self.asar.stat().st_ino, original_inode)
        self.assertEqual(self.info.read_bytes(), original_info)
        staged = pathlib.Path(receipt['staged_app']) / 'Contents/Resources/app.asar'
        header, base, header_hash = app_patch.read_asar(staged)
        startup = app_patch._entry_bytes(staged, header, base)
        self.assertTrue(startup.endswith(self.original))
        self.assertIn(b'try{require(', startup)
        self.assertIn(json.dumps(str(self.hook)).encode(), startup)
        self.assertIn(('.configure(' + json.dumps(str(self.root)) + ')}catch{}').encode(), startup)
        self.assertEqual(app_patch._entry(header)['integrity'], app_patch._integrity(startup))
        node = header['files']['other.txt']
        with staged.open('rb') as stream:
            stream.seek(base + int(node['offset']))
            self.assertEqual(stream.read(node['size']), b'unchanged other asset')
        staged_info = plistlib.loads((staged.parent.parent / 'Info.plist').read_bytes())
        self.assertEqual(staged_info['ElectronAsarIntegrity']['Resources/app.asar']['hash'], header_hash)
        self.assertEqual(header_hash, receipt['header_sha256'])
        self.assertEqual((self.root / 'app-patch/receipt.json').stat().st_mode & 0o777, 0o600)
        self.assertNotEqual(staged.stat().st_ino, original_inode)
        self.codesign.assert_any_call(pathlib.Path(receipt['staged_app']), sign=True)
        self.codesign.assert_any_call(pathlib.Path(receipt['staged_app']) / app_patch.FRAMEWORK_BUNDLE,
                                     sign=True, framework=True)
        staged_framework = pathlib.Path(receipt['staged_app']) / app_patch.FRAMEWORK_BINARY
        framework_bytes = staged_framework.read_bytes()
        seal = app_patch.PIN['framework_slot_offset'] + 34
        self.assertEqual(framework_bytes[:seal], original_framework[:seal])
        self.assertEqual(framework_bytes[seal + 32:], original_framework[seal + 32:])
        self.assertNotEqual(framework_bytes[seal:seal + 32], original_framework[seal:seal + 32])
        self.assertEqual((self.app / app_patch.FRAMEWORK_BINARY).read_bytes(), original_framework)
        self.assertEqual(app_patch._framework_slot(staged_framework)[1],
                         app_patch._integrity_dictionary_digest(staged_info))
        self.exchange.assert_not_called()

    def test_prepare_idempotent_before_and_after_install(self):
        receipt = self.prepare()
        self.assertEqual(self.prepare(), receipt)
        installed = app_patch.install(receipt)
        self.assertEqual(installed['state'], 'installed')
        self.assertEqual(self.prepare(), installed)
        self.assertEqual(app_patch.verify_installed(self.app, self.root, self.original_hash),
                         installed['patched_asar_sha256'])
        self.assertEqual(self.exchange.call_count, 1)

    def test_unknown_build_or_hash_rejected_before_copy(self):
        for field, value in [('CFBundleVersion', 'unknown'),
                             ('CFBundleShortVersionString', 'unknown')]:
            info = plistlib.loads(self.info.read_bytes())
            original = dict(info)
            info[field] = value
            self.info.write_bytes(plistlib.dumps(info))
            with self.assertRaisesRegex(ValueError, 'qualified patch version'):
                self.prepare()
            self.info.write_bytes(plistlib.dumps(original))
        self.asar.write_bytes(self.asar.read_bytes() + b'changed')
        with self.assertRaisesRegex(ValueError, 'qualified patch version'):
            self.prepare()
        self.assertFalse((self.root / 'app-patch/pristine.app').exists())
        self.exchange.assert_not_called()

    def test_unknown_anchor_and_bad_archive_rejected(self):
        path = self.base / 'unknown.asar'
        archive(path, b'unknown startup')
        with self.assertRaisesRegex(ValueError, 'anchor changed'):
            app_patch.patch_asar(path, self.hook)
        for raw in [b'bad', struct.pack('<4I', 4, 99, 42, 2),
                    struct.pack('<4I', 4, 12, 8, app_patch.MAX_HEADER + 1)]:
            path.write_bytes(raw)
            with self.assertRaises(ValueError):
                app_patch.read_asar(path)

    def test_receipt_hook_and_private_permissions_revalidated(self):
        receipt = app_patch.install(self.prepare())
        path = self.root / 'app-patch/receipt.json'
        original_receipt = path.read_text()
        changed = dict(receipt, app_path=str(self.base / 'other.app'))
        path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError, 'scope changed'):
            app_patch.verify_installed(self.app, self.root, self.original_hash)
        path.write_text(original_receipt)
        path.chmod(0o644)
        with self.assertRaisesRegex(ValueError, 'private file'):
            app_patch.verify_installed(self.app, self.root, self.original_hash)
        path.chmod(0o600)
        self.hook.write_text('changed')
        with self.assertRaisesRegex(ValueError, 'hook changed'):
            app_patch.verify_installed(self.app, self.root, self.original_hash)

    def test_install_refuses_stale_app_or_stage(self):
        receipt = self.prepare()
        stage = pathlib.Path(receipt['staged_app']) / 'Contents/Resources/app.asar'
        stage.write_bytes(stage.read_bytes() + b'tampered')
        with self.assertRaisesRegex(ValueError, 'qualified patch version'):
            app_patch.install(receipt)
        self.exchange.assert_not_called()
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)

    def test_rollback_exchanges_bundles_without_rewriting_open_file(self):
        receipt = self.prepare()
        with self.asar.open('rb') as opened:
            installed = app_patch.install(receipt)
            opened.seek(0)
            self.assertEqual(hashlib.sha256(opened.read()).hexdigest(), self.original_hash)
        with self.asar.open('rb') as opened:
            result = app_patch.rollback(installed)
            opened.seek(0)
            self.assertEqual(hashlib.sha256(opened.read()).hexdigest(), installed['patched_asar_sha256'])
        self.assertEqual(result['state'], 'rolled_back')
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        self.assertEqual(app_patch.digest(pathlib.Path(result['backup_app']) / 'Contents/Resources/app.asar'),
                         self.original_hash)

    def test_rollback_rejects_changed_current_app(self):
        installed = app_patch.install(self.prepare())
        self.asar.write_bytes(self.asar.read_bytes() + b'update')
        with self.assertRaisesRegex(ValueError, 'qualified patch version'):
            app_patch.rollback(installed)
        self.assertEqual(self.exchange.call_count, 1)

    def test_failed_post_install_verification_restored_original_bundle(self):
        receipt = self.prepare()
        def verify(app, sign=False):
            if pathlib.Path(app) == self.app and app_patch.digest(self.asar) != self.original_hash:
                raise ValueError('test signature failed')
        self.codesign.side_effect = verify
        with self.assertRaisesRegex(ValueError, 'test signature failed'):
            app_patch.install(receipt)
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        self.assertEqual(json.loads((self.root / 'app-patch/receipt.json').read_text())['state'], 'prepared')
        self.assertEqual(self.exchange.call_count, 2)

    def test_plist_header_and_entry_integrity_validated(self):
        info = plistlib.loads(self.info.read_bytes())
        info['ElectronAsarIntegrity']['Resources/app.asar']['hash'] = '0' * 64
        self.info.write_bytes(plistlib.dumps(info))
        with self.assertRaisesRegex(ValueError, 'header integrity'):
            self.prepare()

    def test_unsafe_root_and_hook_symlink_rejected(self):
        self.root.chmod(0o755)
        with self.assertRaisesRegex(ValueError, '0700'):
            self.prepare()
        self.root.chmod(0o700)
        self.hook.unlink()
        self.hook.symlink_to(self.info)
        with self.assertRaisesRegex(ValueError, 'regular private file'):
            self.prepare()
        self.exchange.assert_not_called()

    def test_signature_command_preserved_outer_metadata_and_verified_deeply(self):
        original = {key: True for key in app_patch.STRIPPED_ENTITLEMENTS}
        original.update({'com.apple.security.cs.allow-jit': True,
                         'com.apple.security.app-sandbox': False})
        temporary_paths = []
        def run_command(command, **kwargs):
            if '--display' in command:
                return mock.Mock(returncode=0, stdout=plistlib.dumps(original))
            if '--sign' in command:
                self.assertNotIn('--deep', command)
                self.assertIn('--preserve-metadata=identifier,flags,runtime', command)
                path = pathlib.Path(command[command.index('--entitlements') + 1])
                temporary_paths.append(path)
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(plistlib.loads(path.read_bytes()), {
                    'com.apple.security.cs.allow-jit': True,
                    'com.apple.security.app-sandbox': False,
                    'com.apple.security.cs.disable-library-validation': True})
            return mock.Mock(returncode=0)
        with mock.patch.object(app_patch.subprocess, 'run', side_effect=run_command) as run:
            self.real_codesign(self.app, sign=True)
            self.assertTrue(temporary_paths)
            self.assertFalse(temporary_paths[0].exists())
            self.real_codesign(self.app)
            self.assertEqual(run.call_args.args[0],
                ['/usr/bin/codesign', '--verify', '--deep', '--strict', str(self.app)])
            run.side_effect = None
            run.return_value.returncode = 1
            with self.assertRaisesRegex(ValueError, 'signature operation failed'):
                self.real_codesign(self.app)

    def test_invalid_or_unknown_entitlements_failed_before_signing(self):
        payloads = [b'not a property list', b'<?xml version="1.0"?><plist><dict>',
                    plistlib.dumps(['unexpected']),
                    plistlib.dumps({'com.apple.developer.unknown': True}),
                    plistlib.dumps({'unknown.entitlement': True})]
        for payload in payloads:
            with self.subTest(payload=payload), mock.patch.object(app_patch.subprocess, 'run',
                    return_value=mock.Mock(returncode=0, stdout=payload)) as run:
                with self.assertRaises(ValueError):
                    self.real_codesign(self.app, sign=True)
                self.assertEqual(run.call_count, 1)
        self.assertEqual(list(self.base.glob('.cq-entitlements-*')), [])

    def test_temporary_entitlements_removed_after_signing_error(self):
        temporary_paths = []
        def run_command(command, **kwargs):
            if '--display' in command:
                return mock.Mock(returncode=0, stdout=plistlib.dumps({}))
            temporary_paths.append(pathlib.Path(command[command.index('--entitlements') + 1]))
            return mock.Mock(returncode=1)
        with mock.patch.object(app_patch.subprocess, 'run', side_effect=run_command):
            with self.assertRaisesRegex(ValueError, 'signature operation failed'):
                self.real_codesign(self.app, sign=True)
        self.assertFalse(temporary_paths[0].exists())

    def test_install_and_rollback_refused_running_native_app(self):
        receipt = self.prepare()
        self.stopped.side_effect = ValueError('Quit the native app')
        with self.assertRaisesRegex(ValueError, 'Quit the native app'):
            app_patch.install(receipt)
        self.exchange.assert_not_called()
        self.stopped.side_effect = None
        installed = app_patch.install(receipt)
        self.stopped.side_effect = ValueError('Quit the native app')
        with self.assertRaisesRegex(ValueError, 'Quit the native app'):
            app_patch.rollback(installed)
        self.assertEqual(self.exchange.call_count, 1)

    def test_process_guard_matched_exact_executable_and_current_uid(self):
        executable = str(self.app / 'Contents/MacOS/ChatGPT')
        for value, blocked in [(executable, True), (executable + ' Helper', False)]:
            with mock.patch.object(app_patch.subprocess, 'run', return_value=mock.Mock(
                    returncode=0, stdout=f'123 {os.getuid()} {value}\n')):
                if blocked:
                    with self.assertRaisesRegex(ValueError, 'Quit the native app'):
                        self.real_stopped(self.app)
                else:
                    self.real_stopped(self.app)
        with mock.patch.object(app_patch.subprocess, 'run', return_value=mock.Mock(
                returncode=0, stdout=f'123 {os.getuid() + 1} {executable}\n')):
            self.real_stopped(self.app)
        with mock.patch.object(app_patch.subprocess, 'run', return_value=mock.Mock(
                returncode=0, stdout='64614 -2 /usr/libexec/dhcp6d\n')):
            self.real_stopped(self.app)
        for row in ('123 --2 /usr/libexec/test', '123 - /usr/libexec/test',
                    '123 unknown /usr/libexec/test', '123 501'):
            with mock.patch.object(app_patch.subprocess, 'run', return_value=mock.Mock(
                    returncode=0, stdout=row)):
                with self.assertRaisesRegex(ValueError, 'inventory is invalid'):
                    self.real_stopped(self.app)
        with mock.patch.object(app_patch.subprocess, 'run', return_value=mock.Mock(returncode=1)):
            with self.assertRaisesRegex(ValueError, 'Cannot verify'):
                self.real_stopped(self.app)

    def test_same_pinned_app_replacement_prepared_new_patch_without_installing(self):
        installed = app_patch.install(self.prepare())
        restored = self.base / 'restored.app'
        shutil.copytree(installed['backup_app'], restored)
        self.swap(self.app, restored)
        prepared = self.prepare()
        self.assertEqual(prepared['state'], 'prepared')
        self.assertEqual(prepared['patched_asar_sha256'], installed['patched_asar_sha256'])
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        self.assertEqual(self.exchange.call_count, 1)
        self.assertEqual(len(list((self.root / 'app-patch').glob('retired-stage-*.app'))), 1)
        self.assertEqual(app_patch.install(prepared)['state'], 'installed')

    def test_signing_limitations_were_recorded_and_revalidated(self):
        receipt = self.prepare()
        self.assertEqual(receipt['signing_profile'], 'adhoc-local-v1')
        self.assertIn('original-signature-keychain-acl', receipt['lost_capabilities'])
        path = self.root / 'app-patch/receipt.json'
        path.write_text(json.dumps({**receipt, 'lost_capabilities': []}))
        with self.assertRaisesRegex(ValueError, 'signing limitations changed'):
            self.prepare()

    def crash_after_swap(self, operation, receipt):
        pid = os.fork()
        if pid == 0:
            def exchange_and_exit(first, second):
                self.swap(first, second)
                os._exit(73)
            self.exchange.side_effect = exchange_and_exit
            try:
                operation(receipt)
            except BaseException:
                os._exit(99)
            os._exit(98)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 73)

    def test_install_recovers_real_process_death_after_exchange(self):
        prepared = self.prepare()
        self.crash_after_swap(app_patch.install, prepared)
        path = self.root / 'app-patch/receipt.json'
        self.assertEqual(json.loads(path.read_text())['state'], 'installing')
        self.assertEqual(app_patch.digest(self.asar), prepared['patched_asar_sha256'])
        before = path.read_bytes()
        observed = app_patch.status(self.app, self.root)
        self.assertEqual(observed['state'], 'installed')
        self.assertTrue(observed['system_app_modified'])
        self.assertFalse(observed['automatic_reapply_allowed'])
        self.assertEqual(path.read_bytes(), before)
        installed = app_patch.install(prepared)
        self.assertEqual(installed['state'], 'installed')
        self.assertEqual(json.loads(path.read_text())['state'], 'installed')
        self.exchange.assert_not_called()

    def test_rollback_recovers_real_process_death_without_auto_reapply(self):
        installed = app_patch.install(self.prepare())
        self.exchange.reset_mock()
        self.crash_after_swap(app_patch.rollback, installed)
        path = self.root / 'app-patch/receipt.json'
        self.assertEqual(json.loads(path.read_text())['state'], 'rolling_back')
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        observed = app_patch.status(self.app, self.root)
        self.assertEqual(observed['state'], 'rolled_back')
        self.assertFalse(observed['system_app_modified'])
        self.assertFalse(observed['automatic_reapply_allowed'])
        rolled_back = app_patch.rollback(installed)
        self.assertEqual(rolled_back['state'], 'rolled_back')
        self.assertEqual(app_patch.status(self.app, self.root)['state'], 'rolled_back')
        self.exchange.assert_not_called()

    def test_journal_recovery_before_exchange_preserved_original_intent(self):
        prepared = self.prepare()
        path = self.root / 'app-patch/receipt.json'
        path.write_text(json.dumps({**prepared, 'state': 'installing'}))
        self.assertEqual(self.prepare()['state'], 'prepared')
        self.exchange.assert_not_called()
        installed = app_patch.install(prepared)
        path.write_text(json.dumps({**installed, 'state': 'rolling_back'}))
        observed = app_patch.status(self.app, self.root)
        self.assertEqual(observed['state'], 'installed')
        self.assertFalse(observed['automatic_reapply_allowed'])
        self.assertEqual(app_patch.rollback(installed)['state'], 'rolled_back')

    def test_explicit_reinstall_after_rollback_reused_same_private_root(self):
        installed = app_patch.install(self.prepare())
        app_patch.rollback(installed)
        prepared = self.prepare()
        self.assertEqual(prepared['state'], 'prepared')
        self.assertEqual(prepared['patched_asar_sha256'], installed['patched_asar_sha256'])
        self.assertEqual(app_patch.install(prepared)['state'], 'installed')

    def test_rollback_remained_available_after_hook_changed_or_disappeared(self):
        installed = app_patch.install(self.prepare())
        self.hook.write_text('new incompatible hook')
        self.assertEqual(app_patch.status(self.app, self.root)['state'], 'installed')
        self.hook.unlink()
        self.assertEqual(app_patch.rollback(installed)['state'], 'rolled_back')
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)

    def test_status_read_only_without_preparation_and_unknown_updates(self):
        before = sorted(str(path.relative_to(self.root)) for path in self.root.rglob('*'))
        observed = app_patch.status(self.app, self.root)
        self.assertEqual(observed, {'state': 'not_installed', 'system_app_modified': False,
                                    'automatic_reapply_allowed': False})
        self.assertEqual(sorted(str(path.relative_to(self.root)) for path in self.root.rglob('*')), before)
        installed = app_patch.install(self.prepare())
        self.assertEqual(app_patch.status(self.app, self.root)['state'], 'installed')
        self.asar.write_bytes(self.asar.read_bytes() + b'unknown update')
        self.assertEqual(app_patch.status(self.app, self.root), {
            'state': 'update_required', 'system_app_modified': None,
            'automatic_reapply_allowed': False})

    def test_auto_reapply_only_after_supported_stock_replacement(self):
        installed = app_patch.install(self.prepare())
        restored = self.base / 'restored.app'
        shutil.copytree(installed['backup_app'], restored)
        self.swap(self.app, restored)
        self.assertEqual(app_patch.status(self.app, self.root), {
            'state': 'update_required', 'system_app_modified': False,
            'automatic_reapply_allowed': True})
        path = self.root / 'app-patch/receipt.json'
        path.write_text(json.dumps({**installed, 'state': 'rolling_back'}))
        self.assertFalse(app_patch.status(self.app, self.root)['automatic_reapply_allowed'])

    def test_framework_disabled_unknown_or_moved_seal_rejected(self):
        path = self.app / app_patch.FRAMEWORK_BINARY
        original = path.read_bytes()
        offset = app_patch.PIN['framework_slot_offset']
        for location, value in ((offset, ord('X')), (offset + 32, 0), (offset + 33, 2)):
            with self.subTest(location=location):
                changed = bytearray(original)
                changed[location] = value
                path.write_bytes(changed)
                with self.assertRaisesRegex(ValueError, 'integrity validation'):
                    app_patch._framework_slot(path)
        path.write_bytes(original)
        with mock.patch.dict(app_patch.PIN, framework_slot_offset=offset + 1):
            with self.assertRaisesRegex(ValueError, 'slot moved'):
                app_patch._framework_slot(path)
        for payload in (b'bad', struct.pack('<8I', 0xfeedfacf, 7, 0, 6, 1, 0, 0, 0),
                        struct.pack('<8I', 0xfeedfacf, 0x100000c, 0, 6, 1, 8, 0, 0) + b'\0' * 8):
            path.write_bytes(payload)
            with self.assertRaises(ValueError):
                app_patch._framework_slot(path)

    def test_framework_and_outer_bundle_pin_rejected_unqualified_source(self):
        for name in app_patch.PIN['bundle_files']:
            with self.subTest(name=name):
                path = self.app / name
                original = path.read_bytes()
                path.write_bytes(original + b'changed')
                with self.assertRaises(ValueError):
                    self.prepare()
                path.write_bytes(original)
        self.assertFalse((self.root / 'app-patch/pristine.app').exists())
        self.exchange.assert_not_called()

    def test_framework_seal_and_bundle_receipt_rechecked_after_install(self):
        installed = app_patch.install(self.prepare())
        path = self.app / app_patch.FRAMEWORK_BINARY
        original = path.read_bytes()
        path.write_bytes(original + b'unknown signed binary')
        with self.assertRaisesRegex(ValueError, 'bundle is not'):
            app_patch.verify_installed(self.app, self.root, self.original_hash)
        path.write_bytes(original)
        receipt_path = self.root / 'app-patch/receipt.json'
        receipt_path.write_text(json.dumps({**installed, 'patched_bundle_files': {}}))
        with self.assertRaisesRegex(ValueError, 'bundle hashes'):
            app_patch.verify_installed(self.app, self.root, self.original_hash)

    def test_framework_signature_kept_entitlements_unset_and_signed_one_bundle(self):
        with mock.patch.object(app_patch.subprocess, 'run',
                return_value=mock.Mock(returncode=0)) as run:
            self.real_codesign(self.app / app_patch.FRAMEWORK_BUNDLE, sign=True, framework=True)
        self.assertEqual(run.call_count, 1)
        command = run.call_args.args[0]
        self.assertNotIn('--deep', command)
        self.assertNotIn('--entitlements', command)
        self.assertEqual(command[-1], str(self.app / app_patch.FRAMEWORK_BUNDLE))

    def test_helpers_signed_inside_out_and_existing_service_preserved(self):
        receipt = self.prepare()
        stage = pathlib.Path(receipt['staged_app'])
        signed = [call.args[0] for call in self.codesign.call_args_list if call.kwargs.get('sign')]
        self.assertEqual(signed, [
            *[stage / app_patch.HELPER_DIRECTORY / (name + '.app')
              for name in app_patch.HELPER_BINARIES if name != 'Codex (Service)'],
            stage / app_patch.FRAMEWORK_BUNDLE, stage])
        service = app_patch.HELPER_DIRECTORY + '/Codex (Service).app/Contents/MacOS/Codex (Service)'
        self.assertEqual(receipt['original_bundle_files'][service], receipt['patched_bundle_files'][service])

    def test_explicit_hook_upgrade_verified_stopped_app_and_preserved_patch(self):
        installed = app_patch.install(self.prepare())
        path = self.root / 'app-patch/receipt.json'
        original_receipt = path.read_bytes()
        self.hook.write_text('exports.configure = function(root) { /* v2 */ };')
        with self.assertRaisesRegex(ValueError, 'hook changed'):
            self.prepare()
        self.stopped.side_effect = ValueError('Quit the native app')
        with self.assertRaisesRegex(ValueError, 'Quit the native app'):
            app_patch.prepare(self.app, self.root, update_hook=True)
        self.assertEqual(path.read_bytes(), original_receipt)
        self.stopped.side_effect = None
        self.exchange.reset_mock()
        upgraded = app_patch.prepare(self.app, self.root, update_hook=True)
        self.assertEqual(upgraded['state'], 'installed')
        self.assertEqual(upgraded['hook_sha256'], app_patch.digest(self.hook))
        self.assertEqual(upgraded['patched_bundle_files'], installed['patched_bundle_files'])
        self.assertEqual(app_patch.verify_installed(self.app, self.root, self.original_hash),
                         installed['patched_asar_sha256'])
        self.exchange.assert_not_called()

    def test_explicit_hook_upgrade_supported_previously_rolled_back_patch(self):
        installed = app_patch.install(self.prepare())
        app_patch.rollback(installed)
        self.hook.write_text('new hook version')
        with self.assertRaisesRegex(ValueError, 'hook changed'):
            self.prepare()
        upgraded = app_patch.prepare(self.app, self.root, update_hook=True)
        self.assertEqual(upgraded['state'], 'prepared')
        self.assertEqual(upgraded['hook_sha256'], app_patch.digest(self.hook))
        self.assertEqual(app_patch.install(upgraded)['state'], 'installed')

    def test_explicit_hook_upgrade_rejected_changed_bundle_before_receipt_write(self):
        installed = app_patch.install(self.prepare())
        path = self.root / 'app-patch/receipt.json'
        original_receipt = path.read_bytes()
        self.hook.write_text('new hook version')
        staged = pathlib.Path(installed['staged_app']) / 'Contents/MacOS/ChatGPT'
        staged.write_bytes(staged.read_bytes() + b'unknown')
        with self.assertRaisesRegex(ValueError, 'bundle is not'):
            app_patch.prepare(self.app, self.root, update_hook=True)
        self.assertEqual(path.read_bytes(), original_receipt)

    def test_source_bundle_symlink_rejected_before_copying(self):
        source = self.app / 'Contents/MacOS/ChatGPT'
        external = self.base / 'external-executable'
        source.rename(external)
        source.symlink_to(external)
        with self.assertRaisesRegex(ValueError, 'must not use symlinks'):
            self.prepare()
        self.assertFalse((self.root / 'app-patch/pristine.app').exists())

    def test_initial_signing_failure_removed_unpublished_copies_and_retry_worked(self):
        def fail_signing(app, sign=False, framework=False):
            if sign:
                raise ValueError('transient signing failure')
        self.codesign.side_effect = fail_signing
        with self.assertRaisesRegex(ValueError, 'transient signing failure'):
            self.prepare()
        directory = self.root / 'app-patch'
        self.assertFalse((directory / 'receipt.json').exists())
        self.assertFalse((directory / 'staged.app').exists())
        self.assertFalse((directory / 'pristine.app').exists())
        self.assertEqual(app_patch.digest(self.asar), self.original_hash)
        self.codesign.side_effect = None
        self.assertEqual(self.prepare()['state'], 'prepared')
        self.exchange.assert_not_called()

    def test_initial_copy_and_receipt_failure_allowed_clean_retry(self):
        copytree = shutil.copytree
        def fail_partial(source, destination, *args, **kwargs):
            if pathlib.Path(destination).name == 'staged.app':
                (pathlib.Path(destination) / 'partial-copy').write_text('partial')
                raise OSError('copy interrupted')
            return copytree(source, destination, *args, **kwargs)
        with mock.patch.object(app_patch.shutil, 'copytree', side_effect=fail_partial):
            with self.assertRaisesRegex(OSError, 'copy interrupted'):
                self.prepare()
        self.assertEqual(list((self.root / 'app-patch').iterdir()), [])
        with mock.patch.object(app_patch, '_save', side_effect=OSError('receipt write failed')):
            with self.assertRaisesRegex(OSError, 'receipt write failed'):
                self.prepare()
        self.assertEqual(list((self.root / 'app-patch').iterdir()), [])
        self.assertEqual(self.prepare()['state'], 'prepared')

    def test_preexisting_unpublished_directories_never_removed(self):
        directory = self.root / 'app-patch'
        directory.mkdir(mode=0o700)
        for name in ('staged.app', 'pristine.app'):
            path = directory / name
            path.mkdir()
            marker = path / 'unrelated-marker'
            marker.write_text('preserve this')
            with self.assertRaisesRegex(ValueError, 'not empty'):
                self.prepare()
            self.assertEqual(marker.read_text(), 'preserve this')
            shutil.rmtree(path)


if __name__ == '__main__':
    unittest.main()
