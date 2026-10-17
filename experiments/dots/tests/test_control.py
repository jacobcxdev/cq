"""Lifecycle controls exercised with fake subprocesses and signals only."""
import json
import pathlib
import signal
import subprocess
import sys
import types
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import control
import release


class ControlTests(unittest.TestCase):
    def setUp(self):
        self.root = pathlib.Path('/private/isolated-fixture')
        settings = types.SimpleNamespace(ROOT=self.root, APP=pathlib.Path('/Applications/Fixture.app'),
            validate_runtime_paths=mock.Mock(),
            MODULE_ASSET='app-shared-fixture.js',
            PROOF_PROFILE_IDS={'first': 'profile-first', 'second': 'profile-second'})
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(release, 'settings', settings).start()

    def test_process_only_allows_verified_lifecycle_components(self):
        for identity, expected in [('probe.py', 'probe.py'), ('app', None),
                                   ('launch.py', None), (None, None)]:
            with mock.patch.object(release, 'process_identity', return_value=identity):
                self.assertEqual(control.process(123), expected)

    def test_stop_rechecks_process_before_signal(self):
        # PID 222 no longer matches by the time its identity is checked.
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)), \
             mock.patch.object(release, 'isolated_processes', side_effect=[[111, 222], [], []]), \
             mock.patch.object(release, 'process_identity', side_effect=lambda pid: 'probe.py' if pid == 111 else None), \
             mock.patch.object(control.os, 'kill') as kill:
            result = control.stop()
        kill.assert_called_once_with(111, signal.SIGTERM)
        self.assertTrue(result['stopped'])

    def test_stop_refuses_failed_app_validation_before_component_signal(self):
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)), \
             mock.patch.object(release, 'isolated_processes', return_value=[111]), \
             mock.patch.object(release, 'process_identity', return_value='app'), \
             mock.patch.object(control.os, 'kill') as kill:
            with self.assertRaisesRegex(ValueError, 'could not be stopped'):
                control.stop()
        kill.assert_not_called()

    def test_status_uses_configured_module_and_exact_profile_set(self):
        ready = {'profiles': ['profile-first', 'profile-second'], 'primary': 'profile-first',
                 'available': True, 'workspaceError': False}
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, json.dumps(ready))) as run, \
             mock.patch.object(release, 'isolated_processes', return_value=[]):
            value = control.status()
        self.assertTrue(value['native_catalog_ready'])
        self.assertEqual(value['profiles'], 2)
        self.assertIn('app-shared-fixture.js', run.call_args.args[0][-1])
        for update in [{'profiles': ['profile-first']}, {'primary': 'unknown'},
                       {'available': False}, {'workspaceError': True}]:
            with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, json.dumps({**ready, **update}))):
                with self.assertRaisesRegex(ValueError, 'not ready'):
                    control.status()

    def test_status_does_not_expose_failed_subprocess_output(self):
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, 'private-output', 'private-error')):
            with self.assertRaisesRegex(ValueError, '^Isolated native status unavailable$'):
                control.status()

    def test_start_revalidates_private_paths_before_creating_logs_or_processes(self):
        release.settings.validate_runtime_paths.side_effect = ValueError('unsafe private path')
        with mock.patch.object(release, 'verify', return_value={'files': {}}), \
             mock.patch.object(release, 'preflight'), \
             mock.patch.object(release, 'isolated_processes', return_value=[]), \
             mock.patch.object(control.os, 'open') as create, \
             mock.patch.object(control.subprocess, 'Popen') as spawn:
            with self.assertRaisesRegex(ValueError, 'unsafe private path'):
                control.start('/candidate')
        create.assert_not_called()
        spawn.assert_not_called()

    def test_start_checks_installed_hashes_before_spawning(self):
        manifest = {'files': {'probe.py': 'expected'}}
        with mock.patch.object(release, 'verify', return_value=manifest), \
             mock.patch.object(release, 'preflight'), \
             mock.patch.object(release, 'regular'), \
             mock.patch.object(release, 'digest', return_value='different'), \
             mock.patch.object(control.subprocess, 'Popen') as spawn:
            with self.assertRaisesRegex(ValueError, 'differ from candidate'):
                control.start('/candidate')
        spawn.assert_not_called()


if __name__ == '__main__':
    unittest.main()
