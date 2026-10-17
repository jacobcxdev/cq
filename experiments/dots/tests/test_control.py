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
            PROOF_PROFILE_IDS={'first': 'profile-first', 'second': 'profile-second'},
            DOTS={'dot-first':('profile-first','root-first')})
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

    def test_onboarding_status_checks_dom_before_importing_api(self):
        pending={'native_catalog_ready':False,'onboarding_required':True}
        with mock.patch.object(control.subprocess,'run',return_value=subprocess.CompletedProcess([],0,json.dumps(pending))) as run:
            self.assertEqual(control.status(),pending)
        code=run.call_args.args[0][-1]
        self.assertLess(code.index('onboarding-progress-bar'),code.index('await import'))

    def test_pending_onboarding_does_not_reconcile_or_start_relay(self):
        pending={'native_catalog_ready':False,'onboarding_required':True}
        with mock.patch.object(control,'ensure_watcher',return_value={'account_watcher_started':True}) as watcher, \
             mock.patch.object(control,'reconcile_awareness') as awareness, \
             mock.patch.object(control,'_spawn') as spawn:
            result=control.finish_start(pending)
        self.assertTrue(result['onboarding_required'])
        watcher.assert_called_once();awareness.assert_not_called();spawn.assert_not_called()

    def test_pending_start_keeps_private_app_and_proxies_alive(self):
        pending={'native_catalog_ready':False,'onboarding_required':True}
        with mock.patch.object(release,'verify',return_value={'files':{}}), \
             mock.patch.object(release,'preflight'), \
             mock.patch.object(control,'running',return_value=[]), \
             mock.patch.object(control,'_spawn',return_value=types.SimpleNamespace(poll=lambda:None)) as spawn, \
             mock.patch.object(control.subprocess,'run',return_value=subprocess.CompletedProcess([],0)), \
             mock.patch.object(control,'status',side_effect=[ValueError('warming up'),pending]), \
             mock.patch.object(control,'ensure_watcher',return_value={}), \
             mock.patch.object(control,'reconcile_awareness') as awareness, \
             mock.patch.object(control,'stop') as stop, \
             mock.patch.object(control.time,'monotonic',side_effect=[100,131]), \
             mock.patch.object(control.time,'sleep'):
            result=control.start('/candidate')
        self.assertTrue(result['onboarding_required'])
        self.assertEqual([call.args[1] for call in spawn.call_args_list],['probe.py','realtime_probe.py'])
        awareness.assert_not_called();stop.assert_not_called()

    def test_ready_completion_starts_relay_once_and_reuses_verified_process(self):
        ready={'native_catalog_ready':True}
        with mock.patch.object(release,'isolated_processes',side_effect=[[],[123]]), \
             mock.patch.object(release,'process_identity',return_value='room_coordination.py'), \
             mock.patch.object(control,'ensure_watcher',return_value={}), \
             mock.patch.object(control,'reconcile_awareness',return_value=[{'outcome':'accepted'}]) as awareness, \
             mock.patch.object(control,'_spawn',return_value=types.SimpleNamespace(poll=lambda:None)) as spawn, \
             mock.patch.object(control.time,'sleep'):
            self.assertTrue(control.finish_start(ready)['relay_started'])
            self.assertTrue(control.finish_start(ready)['relay_existing'])
        spawn.assert_called_once_with(release.TARGET,'room_coordination.py')
        self.assertEqual(awareness.call_count,2)  # Persisted awareness guards decide whether a send is needed.

    def test_sync_continues_pending_onboarding_without_metadata_change(self):
        import cli
        pending={'native_catalog_ready':False,'onboarding_required':True}
        with mock.patch.object(cli,'configure'), \
             mock.patch.object(cli,'refresh',return_value=(False,[{'status':'ready'}])), \
             mock.patch.object(release,'isolated_processes',return_value=[123]), \
             mock.patch.object(release,'process_identity',return_value='app'), \
             mock.patch.object(control,'finish_start',return_value=pending) as finish, \
             mock.patch.dict(sys.modules,{'control':control,'release':release}):
            result=cli.sync(self.root,preserve_watcher=True)
        finish.assert_called_once_with()
        self.assertTrue(result['discovery_retryable'])

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

    def test_preserved_watcher_is_not_signalled_or_waited_for(self):
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)), \
             mock.patch.object(release, 'isolated_processes', side_effect=[[111, 222], [222], [222]]), \
             mock.patch.object(release, 'process_identity', side_effect=lambda pid: 'probe.py' if pid == 111 else 'account_watch.py'), \
             mock.patch.object(control.os, 'kill') as kill:
            self.assertTrue(control.stop(preserve_watcher=True)['stopped'])
        kill.assert_called_once_with(111, signal.SIGTERM)

    def test_normal_stop_signals_verified_watcher(self):
        with mock.patch.object(control.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)), \
             mock.patch.object(release, 'isolated_processes', side_effect=[[222], [], []]), \
             mock.patch.object(release, 'process_identity', return_value='account_watch.py'), \
             mock.patch.object(control.os, 'kill') as kill:
            control.stop()
        kill.assert_called_once_with(222, signal.SIGTERM)

    def test_preservation_never_ignores_app_or_other_component(self):
        for identity in ('app', 'probe.py', None):
            with self.subTest(identity=identity), \
                 mock.patch.object(release, 'verify', return_value={'files': {}}), \
                 mock.patch.object(release, 'preflight'), \
                 mock.patch.object(release, 'isolated_processes', return_value=[222]), \
                 mock.patch.object(release, 'process_identity', return_value=identity), \
                 mock.patch.object(control.subprocess, 'Popen') as spawn:
                with self.assertRaisesRegex(ValueError,'already running'):
                    control.start('/candidate',preserve_watcher=True)
                spawn.assert_not_called()

    def test_native_ready_then_awareness_then_relay_then_watcher(self):
        self._start_order(preserve=False)

    def test_preserved_verified_watcher_is_not_spawned_twice(self):
        self._start_order(preserve=True)

    def _start_order(self,preserve):
        import tempfile
        events=[]
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory)
            def spawn(command,**kwargs):
                events.append(pathlib.Path(command[-1]).name)
                return types.SimpleNamespace(poll=lambda:None)
            def ready():events.append('native-ready');return {'native_catalog_ready':True}
            def awareness(*args):events.append('awareness');return [{'target':'dot-first','outcome':'unknown'}]
            with mock.patch.object(release.settings,'ROOT',root), \
                 mock.patch.object(release,'verify',return_value={'files':{}}), \
                 mock.patch.object(release,'preflight'), \
                 mock.patch.object(release,'regular'), \
                 mock.patch.object(release,'isolated_processes',return_value=[222] if preserve else []), \
                 mock.patch.object(release,'process_identity',return_value='account_watch.py'), \
                 mock.patch.object(control,'status',side_effect=ready), \
                 mock.patch.object(control,'reconcile_awareness',side_effect=awareness), \
                 mock.patch.object(control.subprocess,'Popen',side_effect=spawn), \
                 mock.patch.object(control.subprocess,'run',return_value=subprocess.CompletedProcess([],0)), \
                 mock.patch.object(control.time,'sleep'):
                result=control.start('/candidate',preserve_watcher=preserve)
        self.assertEqual(events[:5],['probe.py','realtime_probe.py','native-ready','awareness','room_coordination.py'])
        self.assertEqual(events.count('account_watch.py'),0 if preserve else 1)
        self.assertEqual(result['awareness'][0]['outcome'],'unknown')

    def test_awareness_subprocess_failure_returns_unknown_without_retry_or_output(self):
        for failure in (subprocess.CompletedProcess([],1,'private-output','secret-error'),
                        subprocess.CompletedProcess([],0,'not-json','secret-error')):
            with mock.patch.object(control.subprocess,'run',return_value=failure) as run:
                result=control.reconcile_awareness(self.root,self.root/'venv/bin/python')
            run.assert_called_once()
            self.assertEqual(result,[{'target':'dot-first','outcome':'unknown','error':'awareness_reconciliation_failed'}])
            self.assertNotIn('secret',json.dumps(result))
            self.assertEqual(run.call_args.kwargs['timeout'],120)


    def test_empty_catalogue_can_start_discovery_without_native_components(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory)
            with mock.patch.object(release.settings,'ROOT',root), \
                 mock.patch.object(release.settings,'DOTS',{}), \
                 mock.patch.object(release,'regular'), \
                 mock.patch.object(release,'isolated_processes',return_value=[]), \
                 mock.patch.object(control,'_spawn',return_value=types.SimpleNamespace(poll=lambda:None)) as spawn, \
                 mock.patch.object(control.time,'sleep'):
                result=control.ensure_watcher()
            spawn.assert_called_once_with(root,'account_watch.py')
            self.assertTrue(result['account_watcher_started'])
            self.assertFalse(result['account_watcher_existing'])

    def test_existing_watcher_is_reused_but_duplicate_verified_watchers_fail(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(release.settings,'ROOT',pathlib.Path(directory)), \
                 mock.patch.object(release,'regular'), \
                 mock.patch.object(release,'process_identity',return_value='account_watch.py'), \
                 mock.patch.object(control,'_spawn') as spawn:
                with mock.patch.object(release,'isolated_processes',return_value=[111]):
                    self.assertTrue(control.ensure_watcher()['account_watcher_existing'])
                with mock.patch.object(release,'isolated_processes',return_value=[111,222]):
                    with self.assertRaisesRegex(ValueError,'Multiple verified'):
                        control.ensure_watcher()
                spawn.assert_not_called()

    def test_concurrent_watcher_launch_is_guarded(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory)
            def spawn(*args):
                with self.assertRaises(BlockingIOError):control.ensure_watcher()
                return types.SimpleNamespace(poll=lambda:None)
            with mock.patch.object(release.settings,'ROOT',root), \
                 mock.patch.object(release,'regular'), \
                 mock.patch.object(release,'isolated_processes',return_value=[]), \
                 mock.patch.object(control,'_spawn',side_effect=spawn) as started, \
                 mock.patch.object(control.time,'sleep'):
                control.ensure_watcher()
            started.assert_called_once()



if __name__ == '__main__':
    unittest.main()
