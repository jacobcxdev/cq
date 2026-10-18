"""Dock bundle installation and launch boundaries; no real app or account access."""
import json
import os
import pathlib
import plistlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(HERE))
import cli
import dock


class DockTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = pathlib.Path(temporary.name).resolve()
        self.root = self.home / "CQ with 'quoted' paths"
        self.app = self.home / 'Original.app'
        self.cq = self.home / 'cq'
        self.cq.write_text('#!/bin/sh\nexit 0\n')
        self.cq.chmod(0o700)
        resources = self.app / 'Contents/Resources'
        resources.mkdir(parents=True)
        (resources / 'original.icns').write_bytes(b'synthetic-icon')
        (self.app / 'Contents/Info.plist').write_bytes(plistlib.dumps({
            'CFBundleIconFile': 'original.icns'}))
        patch = mock.patch.object(dock.pathlib.Path, 'home', return_value=self.home)
        patch.start()
        self.addCleanup(patch.stop)
        patch = mock.patch.dict(os.environ, {'CQ_DOTS_CQ_PATH': str(self.cq)})
        patch.start()
        self.addCleanup(patch.stop)

    def test_installed_bundle_uses_private_controller_and_survives_reinstall(self):
        bundle = dock.install(self.root, self.app)
        contents = bundle / 'Contents'
        info = plistlib.loads((contents / 'Info.plist').read_bytes())
        self.assertEqual(bundle, self.home / 'Applications/ChatGPT via CQ.app')
        self.assertTrue(info['LSUIElement'])
        self.assertEqual(info['CQDotsRoot'], str(self.root))
        self.assertNotEqual(info['CFBundleIdentifier'], 'com.openai.codex')
        self.assertEqual((contents / 'Resources/ChatGPT.icns').read_bytes(), b'synthetic-icon')
        executable = contents / 'MacOS/launch'
        self.assertTrue(os.access(executable, os.X_OK))
        # Execute generated shell with a fake interpreter; proves literal argv and no PATH dependency.
        python = self.root / 'runtime/venv/bin/python'
        python.parent.mkdir(parents=True)
        python.symlink_to(sys.executable)
        script = self.root / 'payload/dock.py'
        script.parent.mkdir()
        script.write_text('import sys; print(repr(sys.argv[1:]))')
        result = subprocess.run([str(executable)], capture_output=True, text=True, check=True,
                                env={'PATH': '/usr/bin:/bin', 'PYTHONHOME': '/invalid'})
        self.assertEqual(result.stdout.strip(), repr([str(self.root), str(self.cq)]))
        self.assertEqual(dock.install(self.root, self.app), bundle)

    def test_rejects_unrelated_bundle_other_installation_and_symlink_paths(self):
        bundle = dock.install(self.root, self.app)
        info_path = bundle / 'Contents/Info.plist'
        original = info_path.read_bytes()
        info = plistlib.loads(original)
        for key, value in (('CFBundleIdentifier', 'another.app'), ('CQDotsRoot', '/another/root')):
            changed = {**info, key: value}
            info_path.write_bytes(plistlib.dumps(changed))
            with self.assertRaisesRegex(ValueError, 'another app or installation'):
                dock.install(self.root, self.app)
            self.assertEqual(plistlib.loads(info_path.read_bytes()), changed)
        info_path.write_bytes(original)
        executable = bundle / 'Contents/MacOS/launch'
        executable.unlink()
        victim = self.home / 'victim'
        victim.write_text('unchanged')
        executable.symlink_to(victim)
        with self.assertRaisesRegex(ValueError, 'owned|symlink'):
            dock.install(self.root, self.app)
        self.assertEqual(victim.read_text(), 'unchanged')

    def test_symlink_applications_and_unsafe_icon_are_rejected(self):
        outside = self.home / 'outside'
        outside.mkdir()
        (self.home / 'Applications').symlink_to(outside)
        with self.assertRaises(ValueError):
            dock.install(self.root, self.app)
        self.assertEqual(list(outside.iterdir()), [])
        (self.home / 'Applications').unlink()
        (self.app / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIconFile': '../outside'}))
        with self.assertRaises(ValueError):
            dock.install(self.root, self.app)

    def test_launch_starts_adapter_before_activating_original_app(self):
        settings = mock.Mock(INTEGRATION_MODE='native', APP=self.app)
        started = subprocess.CompletedProcess([], 0, json.dumps({'native_catalog_ready': True}), '')
        with mock.patch.object(cli, 'configure', return_value=settings), \
             mock.patch.object(dock.subprocess, 'run', return_value=started) as run:
            self.assertEqual(dock.controller(str(self.root), str(self.cq)), (0, ''))
        self.assertEqual(run.call_args_list[0].args[0], [str(self.cq), 'codex', 'dots', '--root', str(self.root), 'start', '--json'])
        # Native warm-up and one-time setup have their own bounds; do not kill
        # a healthy CLI startup partway through those sequential operations.
        self.assertNotIn('timeout', run.call_args_list[0].kwargs)
        self.assertEqual(run.call_args_list[1], mock.call(['/usr/bin/open', '-a', str(self.app)], check=True, timeout=15))

    def test_launch_never_opens_stock_app_when_restart_or_onboarding_needed(self):
        for result in ({'restart_required': True}, {'started': False, 'reason': 'No onboarded Dots found'}):
            started = subprocess.CompletedProcess([], 0, json.dumps(result), '')
            with mock.patch.object(dock.subprocess, 'run', return_value=started) as run:
                self.assertEqual(dock.controller(str(self.root), str(self.cq))[0], 1)
                self.assertEqual(run.call_count, 1)

    def test_desktop_failure_uses_literal_dialog_argument(self):
        with mock.patch.object(dock, 'controller', return_value=(1, 'Quit ChatGPT, then reopen ChatGPT via CQ.')), \
             mock.patch.object(dock.subprocess, 'run') as run:
            self.assertEqual(dock.main([str(self.root), str(self.cq)]), 1)
        args = run.call_args.args[0]
        self.assertEqual(args[-1], 'Quit ChatGPT, then reopen ChatGPT via CQ.')
        self.assertNotIn(args[-1], args[2])


if __name__ == '__main__':
    unittest.main()
