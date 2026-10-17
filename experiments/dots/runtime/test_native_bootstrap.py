import test_support
import contextlib
import io
import json
import os
import pathlib
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

import native_bootstrap as bootstrap
import native_debug
import settings


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name).resolve()
        self.root.chmod(0o700)
        self.patch = patch.object(settings, 'ROOT', self.root)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        for name, value in [('port', '8000'), ('realtime-port', '12346')]:
            path = self.root / name
            path.write_text(value)
            path.chmod(0o600)
        self.identities = {}
        self.children = {}
        self.release = Mock()
        self.release.isolated_processes.return_value = []
        self.release.process_identity.side_effect = self.identities.get
        self.plan = {'env': {'CQ_DOTS_CONFIG': str(self.root.parent / 'settings.json')}, 'switches': {}}

    def spawn(self, name, arguments=()):
        child = Mock(pid=len(self.children) + 100)
        child.poll.return_value = None
        self.children[name] = child
        self.identities[child.pid] = name
        return child

    def listeners(self, port):
        name = 'probe.py' if port == 8000 else 'realtime_probe.py'
        return {self.children[name].pid} if name in self.children else set()

    def prepare(self, **changes):
        with patch.object(bootstrap, 'parent_identity') as parent,patch.object(bootstrap, 'payload', return_value=(self.release, changes.get('manifest', 'manifest'))),patch.object(bootstrap, 'spawn', side_effect=self.spawn) as spawn,patch.object(bootstrap, 'listeners', side_effect=changes.get('listeners', self.listeners)),patch('subprocess.run', return_value=Mock(returncode=0, stdout=json.dumps(self.plan).encode())) as run:
            result = bootstrap.prepare(42)
        return result, parent, spawn, run

    def test_fresh_start_waits_for_verified_sidecars_then_detaches_finish(self):
        result, parent, spawn, run = self.prepare()
        self.assertEqual(result, self.plan)
        parent.assert_called_once_with(42, direct=True)
        self.assertEqual([call.args[0] for call in spawn.call_args_list],
                         ['probe.py', 'realtime_probe.py', 'native_bootstrap.py'])
        self.assertEqual(spawn.call_args.args[1], ('--finish', '--parent-pid', '42'))
        self.assertEqual(run.call_args.args[0], [str(self.root / 'venv/bin/python'),
                         str(self.root / 'launch.py'), '--prepare-native', '--parent-pid=42'])
        state = json.loads((self.root / 'native-startup.json').read_text())
        self.assertEqual(state['sidecars'], {'probe.py': 100, 'realtime_probe.py': 101})
        self.assertEqual(state['app_pid'], 42)
        self.assertEqual((self.root / 'native-startup.json').stat().st_mode & 0o777, 0o600)
        for name in bootstrap.SIDECARS:
            self.children[name].terminate.assert_not_called()

    def test_verified_sidecars_are_preserved_on_subsequent_app_launch(self):
        self.prepare()
        self.release.isolated_processes.return_value = [100, 101]
        _, _, spawn, _ = self.prepare()
        self.assertEqual([call.args[0] for call in spawn.call_args_list], ['native_bootstrap.py'])
        self.children['probe.py'].terminate.assert_not_called()

    def test_changed_manifest_or_missing_metadata_does_not_reuse_stale_sidecars(self):
        self.prepare()
        self.release.isolated_processes.return_value = [100, 101]
        with self.assertRaisesRegex(ValueError, 'stale'):
            self.prepare(manifest='changed')
        (self.root / 'native-startup.json').unlink()
        with self.assertRaises(FileNotFoundError):
            self.prepare()
        for name in bootstrap.SIDECARS:
            self.children[name].terminate.assert_not_called()

    def test_foreign_listener_is_never_adopted_or_signalled(self):
        with self.assertRaisesRegex(ValueError, 'already in use'):
            self.prepare(listeners=lambda _: {999})
        self.assertEqual(self.children, {})
        self.assertFalse((self.root / 'native-startup.json').exists())

    def test_partial_failure_stops_only_newly_spawned_children(self):
        def occupied(port):
            return {999} if port == 12346 else self.listeners(port)
        with self.assertRaisesRegex(ValueError, 'already in use'):
            self.prepare(listeners=occupied)
        self.assertEqual(set(self.children), {'probe.py'})
        self.children['probe.py'].terminate.assert_called_once()

    def test_stale_port_file_is_not_readiness_and_times_out_without_finish(self):
        with patch('time.monotonic', side_effect=[0, 9]):
            with self.assertRaisesRegex(ValueError, 'readiness timed out'):
                self.prepare(listeners=lambda _: set())
        self.assertEqual(set(self.children), set(bootstrap.SIDECARS))
        for child in self.children.values():
            child.terminate.assert_called_once()

    def test_payload_preflights_candidate_app_and_every_installed_runtime_hash(self):
        installation = self.root / 'installation'
        runtime = installation / 'runtime'
        payload = installation / 'payload'
        candidate = installation / 'candidate'
        for path in (installation, runtime, payload, candidate):
            path.mkdir(mode=0o700)
        release_path = payload / 'release.py'
        release_path.write_text('trusted controller')
        release_path.chmod(0o600)
        release = Mock(__file__=str(release_path))
        release.verify.return_value = {'files': {'launch.py': 'matching'}}
        release.regular.side_effect = lambda path: path
        release.digest.side_effect = lambda path: 'manifest' if path.name == 'manifest.json' else 'matching'
        with patch.object(settings, 'ROOT', runtime),patch.object(settings, 'INTEGRATION_MODE', 'native'),patch.object(settings, 'validate_runtime_paths'),patch.object(bootstrap, '__file__', str(runtime / 'native_bootstrap.py')),patch.dict(os.environ, {'CQ_DOTS_CONFIG': str(installation / 'settings.json')}),patch('importlib.import_module', return_value=release),patch.object(sys, 'path', list(sys.path)):
            self.assertEqual(bootstrap.payload(), (release, 'manifest'))
            release.preflight.assert_called_once_with(candidate)
            release.digest.side_effect = lambda _: 'changed'
            with self.assertRaisesRegex(ValueError, 'differs from candidate'):
                bootstrap.payload()

    def test_payload_rejects_wrong_mode_configuration_and_execution_path(self):
        for change in ('mode', 'config', 'script'):
            with self.subTest(change=change),patch.object(settings, 'INTEGRATION_MODE', 'isolated' if change == 'mode' else 'native'),patch.object(bootstrap, '__file__', str(self.root / ('other.py' if change == 'script' else 'native_bootstrap.py'))),patch.dict(os.environ, {'CQ_DOTS_CONFIG': str(self.root.parent / ('other.json' if change == 'config' else 'settings.json'))}),patch('importlib.import_module') as imported:
                with self.assertRaisesRegex(ValueError, 'installation invalid'):
                    bootstrap.payload()
            imported.assert_not_called()

    def test_missing_realtime_port_is_chosen_privately(self):
        (self.root / 'realtime-port').unlink()
        self.prepare()
        value = int((self.root / 'realtime-port').read_text())
        self.assertGreater(value, 1024)
        self.assertLess(value, 65536)
        self.assertEqual((self.root / 'realtime-port').stat().st_mode & 0o777, 0o600)

    def test_failed_or_oversized_launch_plan_never_starts_finish(self):
        self.plan['env']['PATH'] = 'x' * 65536
        with self.assertRaisesRegex(ValueError, 'preparation failed'):
            self.prepare()
        self.assertNotIn('native_bootstrap.py', self.children)
        for child in self.children.values():
            child.terminate.assert_called_once()

    def test_parent_must_be_direct_current_user_official_normal_app(self):
        executable = str(settings.APP / 'Contents/MacOS/ChatGPT')
        row = f'42 {os.getuid()} {executable}\n'
        with patch('os.getppid', return_value=42), patch('subprocess.check_output', return_value=row):
            bootstrap.parent_identity(42, direct=True)
        invalid = [f'42 {os.getuid()+1} {executable}\n',
                   row.rstrip() + ' --user-data-dir=/private/other\n',
                   row + f'43 {os.getuid()} {executable}\n',
                   f'42 {os.getuid()} /another/ChatGPT\n']
        for rows in invalid:
            with self.subTest(rows=rows),patch('os.getppid', return_value=42),patch('subprocess.check_output', return_value=rows):
                with self.assertRaises(ValueError):
                    bootstrap.parent_identity(42, direct=True)
        with patch('os.getppid', return_value=43),patch('subprocess.check_output') as lookup:
            with self.assertRaises(ValueError):
                bootstrap.parent_identity(42, direct=True)
        lookup.assert_not_called()

    def test_parent_lookup_ignores_helpers_other_users_and_separate_profiles(self):
        executable = str(settings.APP / 'Contents/MacOS/ChatGPT')
        rows = f'42 {os.getuid()} {executable}\n43 {os.getuid()} {executable}Helper\n44 {os.getuid()+1} {executable}\n45 {os.getuid()} {executable} --user-data-dir=/research\n'
        with patch('subprocess.check_output', return_value=rows):
            bootstrap.parent_identity(42)

    def test_listener_requires_exact_loopback_pid_and_bounded_lookup(self):
        with patch('subprocess.run', return_value=Mock(returncode=0, stdout='p123\nn127.0.0.1:8000\n')) as run:
            self.assertEqual(bootstrap.listeners(8000), {123})
            self.assertEqual(run.call_args.kwargs['timeout'], 3)
        for result in [Mock(returncode=0, stdout='p123\nn*:8000\n'),Mock(returncode=2, stdout='')]:
            with patch('subprocess.run', return_value=result):
                with self.assertRaises(ValueError):
                    bootstrap.listeners(8000)

    def test_private_port_rejects_symlinks_and_unbounded_input(self):
        port = self.root / 'realtime-port'
        port.unlink()
        port.symlink_to(self.root / 'port')
        with self.assertRaises(OSError):
            bootstrap.port('realtime-port')
        port.unlink()
        port.write_text('8' * 17)
        port.chmod(0o600)
        with self.assertRaises(ValueError):
            bootstrap.port('realtime-port')

    def test_finish_waits_for_matching_embedded_bridge_before_setup(self):
        control = types.SimpleNamespace(status=Mock(return_value={'native_catalog_ready': True}),
                                        finish_start=Mock(return_value={'done': True}))
        identities = [{'app_pid': 99, 'debug_transport': 'embedded'},
                      {'app_pid': 42, 'debug_transport': 'embedded'}]
        with patch.object(bootstrap, 'parent_identity'),patch.object(bootstrap, 'payload'),patch.object(native_debug, 'identity', side_effect=identities),patch.dict('sys.modules', {'control': control}),patch('time.sleep') as sleep:
            self.assertEqual(bootstrap.finish(42), {'done': True})
        sleep.assert_called_once_with(0.5)
        control.status.assert_called_once()
        control.finish_start.assert_called_once_with({'native_catalog_ready': True})

    def test_sync_failure_has_generic_error_and_fifteen_second_deadline(self):
        error = io.StringIO()
        with patch('sys.argv', ['native_bootstrap.py', '--parent-pid', '42']),patch.object(bootstrap, 'prepare', side_effect=ValueError('sensitive text')),patch('signal.signal'),patch('signal.setitimer') as timer,contextlib.redirect_stderr(error):
            self.assertEqual(bootstrap.main(), 1)
        self.assertEqual([call.args[1] for call in timer.call_args_list], [15, 0])
        self.assertNotIn('sensitive', error.getvalue())
        self.assertIn('desktop app can continue', error.getvalue())


if __name__ == '__main__':
    unittest.main()
