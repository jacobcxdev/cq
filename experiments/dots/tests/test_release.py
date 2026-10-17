"""Filesystem-only release checks; never touch a running app or account."""
import json
import os
import pathlib
import plistlib
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import release


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = pathlib.Path(temporary.name).resolve()
        self.root = self.base / 'private-runtime'
        self.root.mkdir(mode=0o700)
        self.app = self.base / 'Native.app'
        (self.app / 'Contents/Resources').mkdir(parents=True)
        (self.app / 'Contents/Resources/app.asar').write_bytes(b'pinned-renderer')
        (self.app / 'Contents/Info.plist').write_bytes(plistlib.dumps({
            'CFBundleShortVersionString': '1.2.3', 'CFBundleVersion': '123'}))
        settings = types.SimpleNamespace(ROOT=self.root, APP=self.app,
            validate_runtime_paths=mock.Mock(),
            SOURCE=self.base / 'account-store', SYSTEM=self.base / 'system/auth.json',
            APP_VERSION='1.2.3', APP_BUILD='123',
            ASAR_SHA256=release.digest(self.app / 'Contents/Resources/app.asar'),
            MODULE_ASSET='app-shared-fixture.js',
            PROOF_PROFILE_IDS={'first': 'profile-first', 'second': 'profile-second'})
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(release, 'settings', settings).start()
        self.source = self.base / 'source'
        (self.source / 'runtime').mkdir(parents=True)
        for name in release.FILES:
            (self.source / 'runtime' / name).write_text('# public source ' + name)
        (self.source / 'runtime/private.json').write_text('private configuration')
        mock.patch.object(release, 'HERE', self.source).start()
        self.candidate = self.base / 'candidate'

    def prepare(self):
        release.prepare(self.candidate)
        return release.verify(self.candidate)

    def test_candidate_contains_only_hashed_public_sources(self):
        manifest = self.prepare()
        self.assertEqual(set(manifest['files']), set(release.FILES))
        self.assertIn('settings.py', manifest['files'])
        self.assertIn('awareness.py', manifest['files'])
        self.assertFalse((self.candidate / 'private.json').exists())
        self.assertEqual((self.candidate / 'manifest.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.candidate / 'cloud_stdio.py').stat().st_mode & 0o777, 0o700)
        (self.candidate / 'probe.py').write_text('changed')
        with self.assertRaisesRegex(ValueError, 'hash mismatch'):
            release.verify(self.candidate)

    def test_manifest_target_pin_and_extra_file_rejected(self):
        manifest = self.prepare()
        path = self.candidate / 'manifest.json'
        for key, replacement in [('target', str(self.base)), ('app_sha256', '0' * 64),
                                 ('module_asset', 'app-shared-other.js')]:
            changed = {**manifest, key: replacement}
            path.write_text(json.dumps(changed))
            with self.assertRaisesRegex(ValueError, 'scope invalid'):
                release.verify(self.candidate)
        path.write_text(json.dumps(manifest))
        (self.candidate / 'private.json').write_text('private')
        with self.assertRaisesRegex(ValueError, 'scope invalid'):
            release.verify(self.candidate)

    def test_candidate_symlink_and_app_write_rejected(self):
        self.prepare()
        (self.candidate / 'probe.py').unlink()
        (self.candidate / 'probe.py').symlink_to(self.source / 'runtime/probe.py')
        with self.assertRaisesRegex(ValueError, 'regular file'):
            release.verify(self.candidate)
        with self.assertRaisesRegex(ValueError, 'outside'):
            release.prepare(self.app / 'candidate')
        with self.assertRaisesRegex(ValueError, 'outside'):
            release.prepare(self.root / 'candidate')
        with self.assertRaisesRegex(ValueError, 'outside'):
            release.prepare(release.settings.SOURCE / 'candidate')
        with self.assertRaisesRegex(ValueError, 'outside'):
            release.prepare(release.settings.SYSTEM.parent / 'candidate')
        link = self.base / 'app-link'
        link.symlink_to(self.app, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'outside'):
            release.prepare(link / 'candidate')

    def test_app_pin_checks_build_and_renderer(self):
        release.app_check()
        release.settings.APP_BUILD = 'different'
        with self.assertRaisesRegex(ValueError, 'version'):
            release.app_check()
        release.settings.APP_BUILD = '123'
        (self.app / 'Contents/Resources/app.asar').write_bytes(b'updated')
        with self.assertRaisesRegex(ValueError, 'renderer'):
            release.app_check()

    def test_preflight_rejects_missing_venv_marker_before_running_interpreter(self):
        self.prepare()
        for name in ('leaf-key.pem', 'leaf-cert.pem', 'probe-cert.pem', 'realtime-cert.pem', 'port'):
            (self.root / name).write_text('fixture')
        with mock.patch.object(release.subprocess, 'check_output') as run:
            with self.assertRaises(FileNotFoundError):
                release.preflight(self.candidate)
        run.assert_not_called()

    def test_preflight_rejects_interpreter_that_escaped_venv(self):
        self.prepare()
        for name in ('leaf-key.pem', 'leaf-cert.pem', 'probe-cert.pem', 'realtime-cert.pem', 'port'):
            (self.root / name).write_text('fixture')
        (self.root / 'venv/bin').mkdir(parents=True)
        (self.root / 'venv/pyvenv.cfg').write_text('stale marker')
        (self.root / 'venv/bin/python').write_text(
            '#!' + sys.executable + '\n'
            'import os, sys\n'
            'os.execv(sys.executable, [sys.executable, "-I", *sys.argv[1:]])\n')
        (self.root / 'venv/bin/python').chmod(0o700)
        real_check_output = subprocess.check_output
        with mock.patch.object(release.subprocess, 'check_output',
                               side_effect=lambda *args, **kwargs: real_check_output(
                                   *args, **kwargs, stderr=subprocess.PIPE)):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                release.preflight(self.candidate)
        self.assertIn('AssertionError', caught.exception.stderr)

    def test_activation_and_rollback_preserve_private_state(self):
        self.prepare()
        (self.root / 'probe.py').write_text('old probe')
        (self.root / 'probe.py').chmod(0o640)
        private = self.root / 'credentials.json'
        private.write_text('private fixture')
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]):
            result = release.activate(self.candidate)
            self.assertEqual((self.root / 'probe.py').read_text(), '# public source probe.py')
            release.restore(result['rollback'])
        self.assertEqual((self.root / 'probe.py').read_text(), 'old probe')
        self.assertEqual((self.root / 'probe.py').stat().st_mode & 0o777, 0o640)
        self.assertFalse((self.root / 'awareness.py').exists())
        self.assertEqual(private.read_text(), 'private fixture')
        self.assertEqual((self.app / 'Contents/Resources/app.asar').read_bytes(), b'pinned-renderer')

    def test_activation_revalidates_private_paths_before_first_write(self):
        self.prepare()
        release.settings.validate_runtime_paths.side_effect = ValueError('unsafe private path')
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]):
            with self.assertRaisesRegex(ValueError, 'unsafe private path'):
                release.activate(self.candidate)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_restore_revalidates_private_paths_before_replacing_sources(self):
        self.prepare()
        (self.root / 'catalog.py').write_text('old catalog')
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]):
            result = release.activate(self.candidate)
            release.settings.validate_runtime_paths.side_effect = ValueError('unsafe private path')
            with self.assertRaisesRegex(ValueError, 'unsafe private path'):
                release.restore(result['rollback'])
        self.assertEqual((self.root / 'catalog.py').read_text(), '# public source catalog.py')

    def test_failed_activation_rolls_back_already_replaced_files(self):
        self.prepare()
        (self.root / 'catalog.py').write_text('old catalog')
        original = release.atomic_copy
        def fail_once(source, destination, mode):
            if source.parent == self.candidate and source.name == 'probe.py':
                raise OSError('simulated replacement failure')
            return original(source, destination, mode)
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]), mock.patch.object(release, 'atomic_copy', side_effect=fail_once):
            with self.assertRaisesRegex(OSError, 'simulated'):
                release.activate(self.candidate)
        self.assertEqual((self.root / 'catalog.py').read_text(), 'old catalog')
        self.assertFalse((self.root / 'self_projection.py').exists())

    def test_activation_refuses_running_process_or_symlink_destination(self):
        self.prepare()
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[123]):
            with self.assertRaisesRegex(ValueError, 'Stop isolated'):
                release.activate(self.candidate)
        victim = self.base / 'unrelated.py'
        victim.write_text('unrelated')
        (self.root / 'probe.py').symlink_to(victim)
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]):
            with self.assertRaisesRegex(ValueError, 'regular file'):
                release.activate(self.candidate)
        self.assertEqual(victim.read_text(), 'unrelated')
        self.assertFalse((self.root / 'catalog.py').exists())

    def test_rollback_outside_target_and_tampering_rejected_before_write(self):
        self.prepare()
        (self.root / 'catalog.py').write_text('old')
        with mock.patch.object(release, 'preflight'), mock.patch.object(release, 'isolated_processes', return_value=[]):
            result = release.activate(self.candidate)
            rollback = pathlib.Path(result['rollback'])
            (rollback / 'catalog.py').write_text('tampered')
            with self.assertRaisesRegex(ValueError, 'hash mismatch'):
                release.restore(rollback)
            with self.assertRaisesRegex(ValueError, 'outside'):
                release.restore(self.base / 'rollback-other')
        self.assertEqual((self.root / 'catalog.py').read_text(), '# public source catalog.py')

    def identity(self, command, uid=None, cwd=None):
        uid = os.getuid() if uid is None else uid
        responses = [subprocess.CompletedProcess([], 0, str(uid) + ' ' + command),
                     subprocess.CompletedProcess([], 0, 'p123\nn' + str(cwd or self.root) + '\n')]
        with mock.patch.object(release.subprocess, 'run', side_effect=responses):
            return release.process_identity(123)

    def test_process_identity_requires_exact_interpreter_script_cwd_and_uid(self):
        command = str(self.root / 'venv/bin/python') + ' -u ' + str(self.root / 'probe.py')
        self.assertEqual(self.identity(command), 'probe.py')
        self.assertIsNone(self.identity(command, uid=os.getuid() + 1))
        self.assertIsNone(self.identity(command, cwd=self.base))
        self.assertIsNone(self.identity('/usr/bin/python3 ' + str(self.root / 'probe.py')))
        self.assertIsNone(self.identity(str(self.root / 'venv/bin/python') + ' -c probe.py'))
        self.assertIsNone(self.identity(str(self.root / 'venv/bin/python') + ' ' + str(self.base / 'probe.py')))

    def test_app_identity_requires_exact_executable_and_single_isolated_data_dir(self):
        app = str(self.app / 'Contents/MacOS/ChatGPT')
        data = '--user-data-dir=' + str(self.root / 'user-data')
        self.assertEqual(self.identity(app + ' ' + data), 'app')
        self.assertIsNone(self.identity(app + ' ' + data + ' ' + data))
        self.assertIsNone(self.identity(app + ' --user-data-dir=' + str(self.base)))
        self.assertIsNone(self.identity('/other/ChatGPT ' + data))


if __name__ == '__main__':
    unittest.main()
