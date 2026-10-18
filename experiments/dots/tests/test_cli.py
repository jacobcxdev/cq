"""Fresh installation boundaries and catalogue reconciliation, without native access."""
import importlib.util
import contextlib
import fcntl
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

PATH = pathlib.Path(__file__).resolve().parents[1] / 'cli.py'
SPEC = importlib.util.spec_from_file_location('dots_cli', PATH)
cli = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cli)


class InstallationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.base = pathlib.Path(self.directory.name).resolve()
        self.root = self.base / 'installation'

    def test_fresh_provision_has_independent_home_data_and_certificates(self):
        self.root.mkdir(mode=0o700)
        commands = []

        def command(args, **kwargs):
            commands.append(args)
            if args[1:3] == ['-m', 'venv']:
                (pathlib.Path(args[3]) / 'bin').mkdir(parents=True)
            if args[0] == 'openssl':
                pathlib.Path(args[args.index('-keyout') + 1]).write_text('synthetic-key')
                pathlib.Path(args[args.index('-out') + 1]).write_text('synthetic-cert')
            return subprocess.CompletedProcess(args, 0, '')

        with mock.patch.object(cli, 'run_checked', side_effect=command), \
             mock.patch.object(cli.sys, 'version_info', (3, 14)):
            runtime = cli.provision(self.root)
        self.assertEqual(runtime, self.root / 'runtime')
        for name in ('user-data', 'codex-home', 'null-executor/work'):
            self.assertEqual((runtime / name).stat().st_mode & 0o777, 0o700)
        for name in ('leaf-key.pem', 'leaf-cert.pem', 'probe-cert.pem', 'realtime-cert.pem'):
            self.assertEqual((runtime / name).stat().st_mode & 0o777, 0o600)
        self.assertFalse((runtime / 'codex-home/auth.json').exists())
        self.assertEqual((runtime / 'port').read_text(), '8000')
        self.assertTrue(any('websockets==17.2' in args for args in commands))

    def test_native_provision_does_not_create_another_desktop_profile(self):
        self.root.mkdir(mode=0o700)
        runtime=self.root/'runtime';runtime.mkdir(mode=0o700)
        (runtime/'venv').mkdir(mode=0o700)
        for name in ('leaf-key.pem','leaf-cert.pem','probe-cert.pem','realtime-cert.pem'):
            (runtime/name).write_text('synthetic')
        with mock.patch.object(cli,'run_checked'):
            cli.provision(self.root, mode='native')
        for name in ('user-data','codex-home','null-executor'):
            self.assertFalse((runtime/name).exists())

    def test_fresh_install_records_native_mode_before_activation(self):
        import base64
        auth=self.base/'credentials'/'auth.json';auth.parent.mkdir(mode=0o700)
        claims={'https://api.openai.com/auth':{'chatgpt_account_id':'synthetic'}}
        token='header.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.signature'
        auth.write_text(json.dumps({'tokens':{'account_id':'synthetic','id_token':token}}));auth.chmod(0o600)
        args=types.SimpleNamespace(root=self.root,app=str(self.base/'app'),system_auth=str(auth),
            cq_accounts=str(self.base/'accounts'),account_source=None,mode='native')
        release=types.SimpleNamespace(activate=mock.Mock())
        settings=types.SimpleNamespace(validate_runtime_paths=mock.Mock(),reload=mock.Mock())
        with mock.patch.object(cli.sys,'platform','darwin'), \
             mock.patch.object(cli,'app_pin',return_value=cli.PIN), \
             mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.object(cli,'provision') as provision, \
             mock.patch.object(cli,'refresh'), \
             mock.patch.object(cli,'candidate',return_value='/candidate'), \
             mock.patch.object(cli,'start',return_value={'restart_required':True}), \
             mock.patch.dict(sys.modules,{'release':release,'dock':types.SimpleNamespace(
                 install=mock.Mock(return_value='/Applications/ChatGPT via CQ.app'))}):
            result=cli.install(args)
        self.assertTrue(result['restart_required'])
        self.assertEqual(result['dock_launcher'],'/Applications/ChatGPT via CQ.app')
        self.assertEqual(json.loads((self.root/'settings.json').read_text())['integration_mode'],'native')
        provision.assert_called_once_with(self.root,mode='native')
        release.activate.assert_called_once_with('/candidate')

    def test_provision_rejects_shared_python_and_partial_certificate_state(self):
        self.root.mkdir(mode=0o700)
        runtime = self.root / 'runtime'
        runtime.mkdir(mode=0o700)
        (runtime / 'venv').symlink_to(self.base)
        with self.assertRaisesRegex(ValueError, 'must not be a symlink'):
            cli.provision(self.root)

    def test_generated_certificate_is_server_leaf_for_native_tls(self):
        self.root.mkdir(mode=0o700)
        runtime = self.root / 'runtime'
        runtime.mkdir(mode=0o700)
        (runtime / 'venv').mkdir(mode=0o700)
        run_checked = cli.run_checked

        def command(args, **kwargs):
            if args[0] == 'openssl':
                return run_checked(args, **kwargs)
            return subprocess.CompletedProcess(args, 0, '')

        with mock.patch.object(cli, 'run_checked', side_effect=command):
            cli.provision(self.root)
        certificate = subprocess.run(['openssl', 'x509', '-in', str(runtime / 'leaf-cert.pem'),
                                      '-noout', '-text'], capture_output=True, text=True, check=True).stdout
        self.assertIn('CA:FALSE', certificate)
        self.assertNotIn('CA:TRUE', certificate)
        self.assertIn('DNS:localhost', certificate)
        self.assertIn('IP Address:127.0.0.1', certificate)

    def test_protected_paths_and_symlink_roots_reject_before_writes(self):
        auth = self.base / 'credentials/auth.json'
        for root in (auth.parent, auth.parent / 'nested', self.base):
            with self.assertRaisesRegex(ValueError, 'overlaps'):
                cli.validate_location(root, self.base / 'app', auth, self.base / 'accounts')
        self.root.symlink_to(self.base)
        with self.assertRaisesRegex(ValueError, 'non-symlink'):
            cli.validate_location(self.root, '/Applications/Fixture.app', auth, self.base / 'accounts')

    def test_install_rejects_protected_root_before_operation_lock(self):
        protected = self.base / 'credentials'
        protected.mkdir(mode=0o700)
        with mock.patch.object(cli.sys, 'stderr'), mock.patch.object(cli.sys, 'stdout'):
            result = cli.main(['install', '--root', str(protected), '--system-auth', str(protected / 'auth.json'),
                               '--cq-accounts', str(self.base / 'accounts')])
        self.assertEqual(result, 1)
        self.assertEqual(list(protected.iterdir()), [])

    def test_reconciliation_keeps_outage_owner_but_removes_absent_owner(self):
        config = {'dots': {'old': {'owner_alias': 'first'}, 'removed': {'owner_alias': 'gone'}}}
        discovery = {'dots': [{'handle': 'new', 'owner_alias': 'second', 'owner_account_id': 'account',
                              'profile_id': 'profile', 'root_thread_id': 'root', 'display_name': 'Same'}],
                     'accounts': [{'owner_alias': 'first', 'status': 'unavailable'},
                                  {'owner_alias': 'second', 'status': 'ready'}]}
        updated, _ = cli.discover_configuration(config, discovery)
        self.assertEqual(set(updated['dots']), {'old', 'new'})
        self.assertEqual(updated['dots']['new']['display_name'], 'Same')
        self.assertEqual(config['dots'].keys(), {'old', 'removed'})

    def test_no_dot_removes_previous_profile_without_new_setup(self):
        config = {'dots': {'old': {'owner_alias': 'first'}}}
        updated, _ = cli.discover_configuration(config, {'dots': [], 'accounts': [
            {'owner_alias': 'first', 'status': 'no_dot'}]})
        self.assertEqual(updated['dots'], {})

    def test_unknown_source_failure_does_not_remove_known_dot(self):
        config = {'dots': {'old': {'owner_alias': 'first'}}}
        updated, _ = cli.discover_configuration(config, {'dots': [], 'accounts': [
            {'source': 'cq', 'status': 'unavailable'}]})
        self.assertEqual(updated, config)

    def test_known_replacement_wins_over_unrelated_source_failure(self):
        config = {'dots': {'old': {'owner_alias': 'first'}}}
        replacement = {'handle': 'new', 'owner_alias': 'first', 'owner_account_id': 'account',
                       'profile_id': 'profile', 'root_thread_id': 'root', 'display_name': 'Same'}
        updated, _ = cli.discover_configuration(config, {'dots': [replacement], 'accounts': [
            {'source': 'cq', 'status': 'unavailable'}, {'owner_alias': 'first', 'status': 'ready'}]})
        self.assertEqual(set(updated['dots']), {'new'})
        updated, _ = cli.discover_configuration(config, {'dots': [], 'accounts': [
            {'source': 'cq', 'status': 'unavailable'}, {'owner_alias': 'first', 'status': 'no_dot'}]})
        self.assertEqual(updated['dots'], {})

    def test_failure_output_never_contains_subprocess_output(self):
        with mock.patch.object(cli.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, 'private-body', 'private-secret')):
            with self.assertRaisesRegex(ValueError, '^Private dependency or certificate provisioning failed$'):
                cli.run_checked(['fixture'])

    def test_dependency_process_does_not_write_global_python_or_pip_cache(self):
        inherited = {'PIP_TARGET': '/outside', 'PIP_PREFIX': '/outside',
                     'PIP_CONFIG_FILE': '/outside/pip.conf', 'PYTHONPATH': '/outside',
                     'PYTHONHOME': '/outside'}
        with mock.patch.dict(os.environ, inherited), \
             mock.patch.object(cli.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)) as run:
            cli.run_checked(['fixture'])
        environment = run.call_args.kwargs['env']
        self.assertEqual(environment['PIP_NO_CACHE_DIR'], '1')
        self.assertEqual(environment['PYTHONDONTWRITEBYTECODE'], '1')
        self.assertEqual(environment['PYTHONNOUSERSITE'], '1')
        self.assertEqual(environment['PIP_CONFIG_FILE'], os.devnull)
        for name in ('PIP_TARGET', 'PIP_PREFIX', 'PYTHONPATH', 'PYTHONHOME'):
            self.assertNotIn(name, environment)

    def test_save_refuses_symlink_destination(self):
        self.root.mkdir(mode=0o700)
        target = self.base / 'unchanged'
        target.write_text('preserved')
        (self.root / 'settings.json').symlink_to(target)
        with self.assertRaises(ValueError):
            cli.save(self.root / 'settings.json', {'unexpected': True})
        self.assertEqual(target.read_text(), 'preserved')


class EmptyCatalogueWatcherTests(unittest.TestCase):
    def test_start_without_dots_keeps_discovery_watcher_active(self):
        settings=types.SimpleNamespace(DOTS={})
        watcher={'account_watcher_started':True,'account_watcher_existing':False}
        control=types.SimpleNamespace(ensure_watcher=mock.Mock(return_value=watcher),start=mock.Mock())
        with mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.dict(sys.modules,{'control':control}), \
             mock.patch.object(cli,'candidate') as candidate:
            result=cli.start(pathlib.Path('/private/install'))
        control.ensure_watcher.assert_called_once_with()
        control.start.assert_not_called();candidate.assert_not_called()
        self.assertFalse(result['started']);self.assertTrue(result['account_watcher_started'])

    def test_start_after_last_dot_removed_reuses_preserved_watcher(self):
        settings=types.SimpleNamespace(DOTS={})
        control=types.SimpleNamespace(ensure_watcher=mock.Mock(return_value={
            'account_watcher_started':True,'account_watcher_existing':True}))
        with mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.dict(sys.modules,{'control':control}):
            result=cli.start(pathlib.Path('/private/install'),preserve_watcher=True)
        self.assertTrue(result['account_watcher_existing'])
        control.ensure_watcher.assert_called_once_with()

    def test_interactive_sync_starts_new_dot_from_empty_watcher_only_install(self):
        root=pathlib.Path('/private/install')
        settings=types.SimpleNamespace(DOTS={},SYSTEM_ALIAS='system',DOT_OWNERS={})
        pids=[]
        def ensure_watcher():
            pids.append(123)
            return {'account_watcher_started':True}
        def refresh(_root):
            settings.DOTS={'dot-system':('profile','root')}
            settings.DOT_OWNERS={'dot-system':'system'}
            return True,[{'owner_alias':'system','status':'ready'}]
        def native_start(_candidate,preserve_watcher=False):
            if pids and not preserve_watcher:
                raise ValueError('Isolated research processes already running')
            return {'started':True,'account_watcher_started':True}
        control=types.SimpleNamespace(ensure_watcher=mock.Mock(side_effect=ensure_watcher),
                                     start=mock.Mock(side_effect=native_start),stop=mock.Mock())
        release=types.SimpleNamespace(isolated_processes=lambda:list(pids),
                                     process_identity=lambda pid:'account_watch.py' if pid==123 else None)
        with mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.dict(sys.modules,{'control':control,'release':release}), \
             mock.patch.object(cli,'candidate',return_value='/candidate'), \
             mock.patch.object(cli,'refresh',side_effect=refresh):
            self.assertFalse(cli.start(root)['started'])
            result=cli.sync(root)
        self.assertTrue(result['started']);self.assertTrue(result['catalogue_changed'])
        self.assertTrue(result['discovery_complete'])
        control.start.assert_called_once_with('/candidate',preserve_watcher=True)
        control.stop.assert_not_called()
        self.assertEqual(pids,[123])


class NativeLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.settings=types.SimpleNamespace(INTEGRATION_MODE='native',DOTS={'main':('profile','thread')},
            SYSTEM_ALIAS='main',DOT_OWNERS={'main':'main'})
        self.release=types.SimpleNamespace(normal_app_running=mock.Mock(return_value=True),
            isolated_processes=mock.Mock(return_value=[]),process_identity=lambda pid:'app')
        self.control=types.SimpleNamespace(ensure_watcher=mock.Mock(return_value={'account_watcher_started':True}),
            start=mock.Mock(),finish_start=mock.Mock(),stop=mock.Mock())
        for item in (mock.patch.object(cli,'configure',return_value=self.settings),
                     mock.patch.dict(sys.modules,{'release':self.release,'control':self.control})):
            item.start();self.addCleanup(item.stop)

    def test_running_stock_app_waits_for_user_restart_without_spawning_or_stopping(self):
        with mock.patch.object(cli,'candidate') as candidate:
            result=cli.start(pathlib.Path('/private/install'))
        self.assertTrue(result['restart_required'])
        self.control.start.assert_not_called();self.control.stop.assert_not_called();candidate.assert_not_called()

    def test_account_change_does_not_restart_managed_normal_app(self):
        self.release.isolated_processes.return_value=[123]
        with mock.patch.object(cli,'refresh',return_value=(True,[{'status':'ready'}])):
            result=cli.sync(pathlib.Path('/private/install'))
        self.assertTrue(result['restart_required']);self.assertTrue(result['catalogue_changed'])
        self.control.stop.assert_not_called();self.control.start.assert_not_called()
        self.control.finish_start.assert_not_called()

    def test_quit_normal_app_allows_sidecar_cleanup_and_same_profile_start(self):
        self.release.normal_app_running.return_value=False
        self.release.isolated_processes.return_value=[123]
        self.control.start.return_value={'started':True}
        with mock.patch.object(cli,'candidate',return_value='/candidate'):
            result=cli.start(pathlib.Path('/private/install'))
        self.assertTrue(result['started'])
        self.control.stop.assert_called_once_with(preserve_watcher=True)
        self.control.start.assert_called_once_with('/candidate',preserve_watcher=True)


class DiscoveryRetryTests(unittest.TestCase):
    def test_sync_classifies_transient_remote_and_terminal_source_failures(self):
        cases=[([{'status':'ready'}],True,False),
               ([{'status':'no_dot'}],True,False),
               ([{'status':'unavailable','owner_alias':'owner'}],False,True),
               ([{'status':'unavailable','owner_alias':'owner','http_status':503}],False,True),
               ([{'status':'unavailable','owner_alias':'owner','http_status':429}],False,True),
               ([{'status':'unavailable','owner_alias':'owner','http_status':401}],False,False),
               ([{'status':'unavailable','source':'cq','failure_type':'ValueError'}],False,False)]
        release=types.SimpleNamespace(isolated_processes=lambda:[])
        for rows,complete,retryable in cases:
            with self.subTest(rows=rows),mock.patch.object(cli,'configure'), \
                 mock.patch.dict(sys.modules,{'release':release,'control':types.SimpleNamespace()}), \
                 mock.patch.object(cli,'refresh',return_value=(False,rows)), \
                 mock.patch.object(cli,'start',return_value={'started':False}):
                result=cli.sync(pathlib.Path('/private/install'),preserve_watcher=True)
            self.assertEqual(result['discovery_complete'],complete)
            self.assertEqual(result['discovery_retryable'],retryable)

    def test_secondary_only_catalogue_fails_before_candidate_or_native_launch(self):
        settings=types.SimpleNamespace(DOTS={'dot-second':('profile','root')},SYSTEM_ALIAS='system',
                                       DOT_OWNERS={'dot-second':'second'})
        control=types.SimpleNamespace(start=mock.Mock(),ensure_watcher=mock.Mock())
        with mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.dict(sys.modules,{'control':control}),mock.patch.object(cli,'candidate') as candidate:
            with self.assertRaisesRegex(ValueError,'Onboard the system account Dot'):
                cli.start(pathlib.Path('/private/install'))
        candidate.assert_not_called();control.start.assert_not_called();control.ensure_watcher.assert_not_called()

    def test_system_owned_dot_catalogue_can_launch(self):
        settings=types.SimpleNamespace(DOTS={'dot-system':('profile','root')},SYSTEM_ALIAS='system',
                                       DOT_OWNERS={'dot-system':'system'})
        control=types.SimpleNamespace(start=mock.Mock(return_value={'started':True}))
        with mock.patch.object(cli,'configure',return_value=settings), \
             mock.patch.dict(sys.modules,{'control':control}),mock.patch.object(cli,'candidate',return_value='/candidate'):
            self.assertTrue(cli.start(pathlib.Path('/private/install'))['started'])
        control.start.assert_called_once_with('/candidate',preserve_watcher=False)


class ConcurrentOperationTests(unittest.TestCase):
    def setUp(self):
        directory=tempfile.TemporaryDirectory();self.addCleanup(directory.cleanup)
        self.root=pathlib.Path(directory.name)
        self.lock=(self.root/'operation.lock').open('w')
        (self.root/'operation.lock').chmod(0o600)
        self.addCleanup(self.lock.close)
        fcntl.flock(self.lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        settings=types.SimpleNamespace(APP='/fixture.app',SYSTEM='/fixture/auth.json',
                                       CQ_ACCOUNTS='/fixture/accounts',SOURCE=None)
        for item in (mock.patch.object(cli,'configure',return_value=settings),
                     mock.patch.object(cli,'validate_location')):
            item.start();self.addCleanup(item.stop)

    def test_readonly_list_and_status_proceed_while_watcher_holds_operation_lock(self):
        control=types.SimpleNamespace(status=mock.Mock(return_value={'native_catalog_ready':True}),stop=mock.Mock())
        with mock.patch.dict(sys.modules,{'control':control}), \
             mock.patch.object(cli,'list_dots',return_value={'dots':[{'id':'stable','setup':'accepted'}]}) as listing:
            for operation in ('list','status'):
                with contextlib.redirect_stdout(io.StringIO()) as output:
                    self.assertEqual(cli.main([operation,'--root',str(self.root),'--json']),0)
                self.assertIsInstance(json.loads(output.getvalue()),dict)
        listing.assert_called_once_with(self.root.resolve())
        control.status.assert_called_once_with()

    def test_manual_prime_waits_for_watcher_then_sends_once(self):
        def release_watcher(_seconds):fcntl.flock(self.lock,fcntl.LOCK_UN)
        awareness=types.SimpleNamespace(prime=mock.Mock(return_value=[{'outcome':'accepted'}]))
        with mock.patch.dict(sys.modules,{'awareness':awareness}), \
             mock.patch.object(cli.time,'sleep',side_effect=release_watcher) as sleep, \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(['prime','stable','--root',str(self.root)]),0)
        sleep.assert_called_once_with(0.1)
        awareness.prime.assert_called_once_with('stable',all=False,manual=True)

    def test_manual_prime_times_out_with_clear_busy_message_without_sending(self):
        awareness=types.SimpleNamespace(prime=mock.Mock())
        with mock.patch.dict(sys.modules,{'awareness':awareness}), \
             mock.patch.object(cli.time,'monotonic',side_effect=[0,46]), \
             contextlib.redirect_stderr(io.StringIO()) as output:
            self.assertEqual(cli.main(['prime','stable','--root',str(self.root)]),1)
        self.assertEqual(json.loads(output.getvalue())['error'],
                         'Another Dots operation is in progress; try again shortly')
        awareness.prime.assert_not_called()

    def test_broadcast_waits_for_watcher_and_preserves_message(self):
        def release_watcher(_seconds):fcntl.flock(self.lock,fcntl.LOCK_UN)
        awareness=types.SimpleNamespace(broadcast=mock.Mock(return_value={'id':'test','deliveries':[{'outcome':'accepted'}]}))
        message='--json $(literal) `literal`\nHello, team!'
        with mock.patch.dict(sys.modules,{'awareness':awareness}), \
             mock.patch.object(cli.time,'sleep',side_effect=release_watcher), \
             contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(cli.main(['broadcast','--root',str(self.root),'--json','--',message]),0)
        awareness.broadcast.assert_called_once_with(message)
        self.assertEqual(json.loads(output.getvalue())['broadcast']['id'],'test')

    def test_broadcast_partial_delivery_returns_failure_without_retry(self):
        fcntl.flock(self.lock,fcntl.LOCK_UN)
        awareness=types.SimpleNamespace(broadcast=mock.Mock(return_value={'deliveries':[{'outcome':'accepted'},{'outcome':'unknown'}]}))
        with mock.patch.dict(sys.modules,{'awareness':awareness}), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(['broadcast','Hello','--root',str(self.root)]),1)
        awareness.broadcast.assert_called_once_with('Hello')

    def test_board_bind_announces_once_and_status_never_sends(self):
        fcntl.flock(self.lock,fcntl.LOCK_UN)
        board=types.SimpleNamespace(bind=mock.Mock(return_value={'thread_id':'local-thread'}),
                                    announce=mock.Mock(return_value=[{'outcome':'accepted'}]),
                                    configuration=mock.Mock(return_value={'thread_id':'local-thread'}),
                                    clear=mock.Mock(return_value={'bound':False}))
        with mock.patch.dict(sys.modules,{'board':board}),contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(['board','codex://threads/local-thread','--root',str(self.root)]),0)
            self.assertEqual(cli.main(['board','--root',str(self.root)]),0)
            self.assertEqual(cli.main(['board','--clear','--root',str(self.root)]),0)
        board.bind.assert_called_once_with('codex://threads/local-thread')
        board.announce.assert_called_once_with()
        board.configuration.assert_called_once_with()
        board.clear.assert_called_once_with()

    def test_board_unknown_notice_delivery_is_not_reported_successful(self):
        fcntl.flock(self.lock,fcntl.LOCK_UN)
        board=types.SimpleNamespace(bind=mock.Mock(return_value={'thread_id':'local-thread'}),
                                    announce=mock.Mock(return_value=[{'outcome':'unknown'}]))
        with mock.patch.dict(sys.modules,{'board':board}),contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(['board','local-thread','--root',str(self.root)]),1)
        board.bind.assert_called_once()
        board.announce.assert_called_once()

    def test_watcher_mutation_remains_nonblocking_and_serialized(self):
        with mock.patch.object(cli,'sync') as sync, \
             mock.patch.object(cli.time,'sleep') as sleep, \
             contextlib.redirect_stderr(io.StringIO()) as output:
            self.assertEqual(cli.main(['sync','--root',str(self.root),'--watcher']),1)
        self.assertIn('Another Dots operation is in progress',output.getvalue())
        sync.assert_not_called();sleep.assert_not_called()


if __name__ == '__main__':
    unittest.main()
