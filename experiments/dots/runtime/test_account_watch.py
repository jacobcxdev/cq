import fcntl
import importlib.util
import json
import os
import pathlib
import plistlib
import subprocess
import tempfile
import types
import unittest
from unittest import mock

CONFIG=types.SimpleNamespace(validate_runtime_paths=mock.Mock())
SPEC=importlib.util.spec_from_file_location('account_watch_under_test',pathlib.Path(__file__).with_name('account_watch.py'))
watcher=importlib.util.module_from_spec(SPEC)
with mock.patch.dict('sys.modules',{'settings':CONFIG}):SPEC.loader.exec_module(watcher)


class AccountWatchTests(unittest.TestCase):
    def setUp(self):
        self.directory=tempfile.TemporaryDirectory();self.addCleanup(self.directory.cleanup)
        self.root=pathlib.Path(self.directory.name)
        self.runtime=self.root/'runtime';self.runtime.mkdir(mode=0o700)
        self.accounts=self.root/'accounts';self.accounts.mkdir(mode=0o700)
        self.source=self.root/'codexbar';self.source.mkdir(mode=0o700)
        self.system=self.root/'system-auth.json';self.system.write_text('synthetic')
        self.patches=[mock.patch.object(CONFIG,name,value,create=True) for name,value in (
            ('ROOT',self.runtime),('CQ_ACCOUNTS',self.accounts),('SOURCE',self.source),('SYSTEM',self.system))]
        for item in self.patches:item.start();self.addCleanup(item.stop)

    def test_fingerprint_uses_metadata_and_detects_account_add_remove_refresh(self):
        initial=watcher.fingerprint()
        auth=self.accounts/'account.auth.json';auth.write_text('synthetic-account')
        added=watcher.fingerprint();self.assertNotEqual(initial,added)
        with mock.patch.object(pathlib.Path,'read_text',side_effect=AssertionError('No credential reads')), \
             mock.patch.object(pathlib.Path,'read_bytes',side_effect=AssertionError('No credential reads')):
            self.assertEqual(watcher.fingerprint(),added)
        auth.write_text('synthetic-updated-account')
        self.assertNotEqual(watcher.fingerprint(),added)
        refreshed=watcher.fingerprint()
        auth.unlink()
        self.assertNotEqual(watcher.fingerprint(),refreshed)

    def test_system_and_optional_manifest_changes_are_detected(self):
        initial=watcher.fingerprint()
        self.system.write_text('synthetic-updated-system')
        changed=watcher.fingerprint();self.assertNotEqual(initial,changed)
        manifest=self.source/'managed-codex-accounts.json';manifest.write_text('synthetic-manifest')
        self.assertNotEqual(watcher.fingerprint(),changed)
        with mock.patch.object(CONFIG,'SOURCE',None):
            self.assertIsInstance(watcher.fingerprint(),str)

    def test_unchanged_poll_never_launches_sync(self):
        with mock.patch.object(watcher,'fingerprint',return_value='same'),mock.patch.object(watcher,'sync') as sync:
            self.assertEqual(watcher.watch_once('same'),('same',None))
        sync.assert_not_called()

    def test_changed_poll_uses_private_cli_and_discards_all_output(self):
        with mock.patch.object(watcher.subprocess,'run',return_value=subprocess.CompletedProcess([],0,b'{"discovery_complete":true,"discovery_retryable":false}',b'secret')) as run:
            self.assertTrue(watcher.sync())
        self.assertEqual(run.call_args.args[0],[str(self.runtime/'venv/bin/python'),str(self.root/'payload/cli.py'),
                                              'sync','--root',str(self.root),'--watcher'])
        self.assertEqual(run.call_args.kwargs,{'cwd':self.runtime,'capture_output':True,'timeout':120})
        with mock.patch.object(watcher,'fingerprint',return_value='new'),mock.patch.object(watcher,'sync',return_value=True):
            result,event=watcher.watch_once('old')
        self.assertEqual(result,'new');self.assertEqual(event,{'event':'account_sync_complete'})

    def test_failed_sync_keeps_revision_pending_without_exposing_errors(self):
        for failure in (subprocess.TimeoutExpired(['private-command'],120,output='secret'),OSError('private-error')):
            with mock.patch.object(watcher.subprocess,'run',side_effect=failure):
                self.assertFalse(watcher.sync())
        with mock.patch.object(watcher,'fingerprint',return_value='new'),mock.patch.object(watcher,'sync',return_value=False) as sync:
            self.assertEqual(watcher.watch_once('old'),('old',{'event':'account_sync_failed'}))
            self.assertEqual(watcher.watch_once('old'),('old',{'event':'account_sync_failed'}))
        self.assertEqual(sync.call_count,2)

    def test_remote_unavailable_then_ready_retries_without_metadata_changes(self):
        results=[subprocess.CompletedProcess([],0,b'{"discovery_complete":false,"discovery_retryable":true}'),
                 subprocess.CompletedProcess([],0,b'{"discovery_complete":true,"discovery_retryable":false}')]
        with mock.patch.object(watcher,'fingerprint',return_value='unchanged-new-account'), \
             mock.patch.object(watcher.subprocess,'run',side_effect=results) as run:
            revision,event=watcher.watch_once('old')
            self.assertEqual(revision,'old');self.assertEqual(event,{'event':'account_sync_failed'})
            revision,event=watcher.watch_once(revision)
            self.assertEqual(revision,'unchanged-new-account')
            self.assertEqual(event,{'event':'account_sync_complete'})
            self.assertEqual(watcher.watch_once(revision),(revision,None))
        self.assertEqual(run.call_count,2)

    def test_invalid_local_credentials_settle_until_metadata_changes(self):
        result=subprocess.CompletedProcess([],0,b'{"discovery_complete":false,"discovery_retryable":false}')
        with mock.patch.object(watcher,'fingerprint',return_value='new'),mock.patch.object(watcher.subprocess,'run',return_value=result) as run:
            revision,event=watcher.watch_once('old')
            self.assertEqual(revision,'new')
            self.assertEqual(watcher.watch_once(revision),('new',None))
        run.assert_called_once()

    def test_failed_snapshot_backoff_is_bounded(self):
        with mock.patch.object(watcher.time,'sleep',side_effect=[None]*6+[KeyboardInterrupt]) as sleep, \
             mock.patch.object(watcher,'watch_once',return_value=(None,{'event':'account_sync_failed'})), \
             mock.patch('builtins.print'):
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        self.assertEqual([call.args[0] for call in sleep.call_args_list],[5,10,20,40,60,60,60])

    def test_stat_error_is_generic_and_does_not_run_sync(self):
        with mock.patch.object(watcher,'fingerprint',side_effect=PermissionError('secret-path')),mock.patch.object(watcher,'sync') as sync:
            self.assertEqual(watcher.watch_once('old'),('old',{'event':'account_sync_failed'}))
        sync.assert_not_called()

    def test_loop_polls_every_five_seconds_and_lock_is_private(self):
        with mock.patch.object(watcher,'fingerprint',return_value='initial'), \
             mock.patch.object(watcher.time,'sleep',side_effect=KeyboardInterrupt) as sleep:
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        sleep.assert_called_once_with(5)
        self.assertEqual((self.runtime/'account-watch.lock').stat().st_mode&0o777,0o600)

    def test_initial_revision_is_pending_and_first_poll_syncs_unconditionally(self):
        with mock.patch.object(watcher,'fingerprint',return_value='current'),mock.patch.object(watcher,'sync',return_value=True) as sync:
            self.assertEqual(watcher.watch_once(None),('current',{'event':'account_sync_complete'}))
        sync.assert_called_once()
        with mock.patch.object(watcher.time,'sleep',side_effect=[None,KeyboardInterrupt]), \
             mock.patch.object(watcher,'watch_once',return_value=(None,{'event':'account_sync_failed'})) as poll, \
             mock.patch('builtins.print'):
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        poll.assert_called_once_with(None)

    def test_initial_busy_operation_keeps_first_revision_pending_until_success(self):
        with mock.patch.object(watcher,'fingerprint',return_value='current'),mock.patch.object(watcher,'sync',side_effect=[False,True]) as sync:
            revision,event=watcher.watch_once(None)
            self.assertIsNone(revision)
            self.assertEqual(event,{'event':'account_sync_failed'})
            revision,event=watcher.watch_once(revision)
            self.assertEqual(revision,'current')
            self.assertEqual(event,{'event':'account_sync_complete'})
        self.assertEqual(sync.call_count,2)

    def test_second_watcher_cannot_start(self):
        lock=self.runtime/'account-watch.lock';lock.touch(mode=0o600)
        with lock.open('w') as stream:
            fcntl.flock(stream,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with mock.patch.object(watcher,'fingerprint') as fingerprint:
                with self.assertRaises(BlockingIOError):watcher.main()
            fingerprint.assert_not_called()

    def test_symlink_lock_is_not_followed(self):
        outside=self.root/'outside';outside.write_text('unchanged')
        (self.runtime/'account-watch.lock').symlink_to(outside)
        with mock.patch.object(watcher,'fingerprint') as fingerprint:
            with self.assertRaises(OSError):watcher.main()
        self.assertEqual(outside.read_text(),'unchanged');fingerprint.assert_not_called()


class NativePatchWatchTests(unittest.TestCase):
    def setUp(self):
        self.directory=tempfile.TemporaryDirectory();self.addCleanup(self.directory.cleanup)
        self.root=pathlib.Path(self.directory.name).resolve();self.root.chmod(0o700)
        self.runtime=self.root/'runtime';self.runtime.mkdir(mode=0o700)
        self.app=self.root/'fixture.app'
        (self.app/'Contents/Resources').mkdir(parents=True)
        self.info=self.app/'Contents/Info.plist'
        self.info.write_bytes(plistlib.dumps({'CFBundleShortVersionString':'1.2.3','CFBundleVersion':'123'}))
        self.asar=self.app/'Contents/Resources/app.asar';self.asar.write_text('original')
        (self.root/'app-patch').mkdir(mode=0o700)
        self.receipt_path=self.root/'app-patch/receipt.json'
        self.receipt_path.write_text('{}');self.receipt_path.chmod(0o600)
        self.receipt={'state':'installed','patched_asar_sha256':'patched'}
        self.patch_module=types.SimpleNamespace(
            PIN={'app_version':'1.2.3','app_build':'123','asar_sha256':'original'},
            _receipt=mock.Mock(side_effect=lambda *_:(self.receipt,self.root/'app-patch')),
            digest=mock.Mock(side_effect=lambda path:path.read_text()),
            verify_installed=mock.Mock(return_value='patched'),
            _ensure_stopped=mock.Mock(),status=mock.Mock(return_value={'automatic_reapply_allowed':True}),
            prepare=mock.Mock(return_value={'state':'prepared'}),install=mock.Mock(side_effect=self.install))
        patches=[mock.patch.object(CONFIG,'ROOT',self.runtime,create=True),
                 mock.patch.object(CONFIG,'APP',self.app,create=True),
                 mock.patch.object(CONFIG,'INTEGRATION_MODE','native',create=True),
                 mock.patch.dict('sys.modules',{'app_patch':self.patch_module})]
        for item in patches:item.start();self.addCleanup(item.stop)

    def install(self, *_):
        self.asar.write_text('patched')
        self.receipt_path.write_text('{"state":"installed"}')
        return {'state':'installed'}

    def test_same_pin_pristine_update_reapplies_once_after_verified_exit(self):
        state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status'],{'state':'installed','reason':'reapplied'})
        self.assertEqual(event['event'],'native_patch_installed')
        self.patch_module._ensure_stopped.assert_called_once_with(self.app)
        self.patch_module.status.assert_called_once_with(self.app,self.root)
        self.patch_module.prepare.assert_called_once_with(self.app,self.root)
        self.patch_module.install.assert_called_once_with({'state':'prepared'})
        self.assertEqual(watcher.watch_patch_once(state),(state,None))
        self.assertEqual(self.patch_module.install.call_count,1)
        status=self.runtime/'app-patch-status.json'
        self.assertEqual(status.stat().st_mode&0o777,0o600)
        self.assertEqual(json.loads(status.read_text()),state['status'])

    def test_running_app_is_left_untouched_then_reapplied_after_exit(self):
        self.patch_module._ensure_stopped.side_effect=[ValueError('still running'),None]
        state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status']['state'],'waiting')
        self.assertIn('will not stop or restart',state['status']['action'])
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()
        state,event=watcher.watch_patch_once(state)
        self.assertEqual(state['status']['reason'],'reapplied')
        self.patch_module.install.assert_called_once()

    def test_unknown_version_or_hash_settles_without_patch_or_retry_loop(self):
        for change in ('version','hash'):
            with self.subTest(change=change):
                self.info.write_bytes(plistlib.dumps({'CFBundleShortVersionString':'new' if change=='version' else '1.2.3','CFBundleVersion':'123'}))
                self.asar.write_text('unknown' if change=='hash' else 'original')
                with mock.patch.object(watcher,'_check_patch',wraps=watcher._check_patch) as check:
                    state,event=watcher.watch_patch_once(None)
                    self.assertEqual(state['status']['state'],'action_required')
                    self.assertEqual(watcher.watch_patch_once(state),(state,None))
                check.assert_called_once()
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()
        self.patch_module._ensure_stopped.assert_not_called()

    def test_prepared_rollback_and_transitional_receipts_never_auto_activate(self):
        for value in ('prepared','rolled_back','installing','rolling_back'):
            with self.subTest(state=value):
                self.receipt['state']=value
                state,event=watcher.watch_patch_once(None)
                self.assertEqual(state['status']['reason'],'explicit_start_required')
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()
        self.patch_module.status.assert_not_called()

    def test_patch_module_can_deny_reapplication_of_an_installed_receipt(self):
        self.patch_module.status.return_value={'state':'update_required','automatic_reapply_allowed':False}
        state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status']['reason'],'explicit_start_required')
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()

    def test_existing_patched_app_is_verified_without_preparing_or_installing(self):
        self.asar.write_text('patched')
        state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status'],{'state':'installed','reason':'verified'})
        self.patch_module.verify_installed.assert_called_once()
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()

    def test_operation_lock_defers_reapplication_without_mutating_app(self):
        path=self.root/'operation.lock';path.touch(mode=0o600)
        with path.open('r+') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status']['reason'],'operation_busy')
        self.patch_module.prepare.assert_not_called();self.patch_module.install.assert_not_called()
        state,event=watcher.watch_patch_once(state)
        self.assertEqual(state['status']['reason'],'reapplied')

    def test_failed_reapplication_is_generic_and_does_not_retry_automatically(self):
        self.patch_module.install.side_effect=ValueError('private path or signing details')
        state,event=watcher.watch_patch_once(None)
        self.assertEqual(state['status']['reason'],'patch_reapplication_failed')
        self.assertNotIn('private path',json.dumps(event))
        self.assertEqual(watcher.watch_patch_once(state),(state,None))
        self.patch_module.install.assert_called_once()

    def test_app_update_metadata_is_seen_without_reading_authentication(self):
        initial=watcher.patch_fingerprint()
        self.asar.write_text('changed archive')
        with mock.patch.object(pathlib.Path,'read_text',side_effect=AssertionError('No reads')), \
             mock.patch.object(pathlib.Path,'read_bytes',side_effect=AssertionError('No reads')):
            self.assertNotEqual(watcher.patch_fingerprint(),initial)

    def test_update_action_required_never_runs_account_sync_or_starts_app(self):
        state={'revision':'unknown','status':{'state':'action_required','reason':'unsupported_app_version'}}
        with mock.patch.object(watcher.time,'sleep',side_effect=[None,KeyboardInterrupt]),mock.patch.object(watcher,'watch_patch_once',return_value=(state,{'event':'native_patch_action_required'})),mock.patch.object(watcher,'watch_once') as sync,mock.patch('builtins.print'):
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        sync.assert_not_called()

    def test_successful_reapply_leaves_app_closed_without_account_sync(self):
        state={'revision':'changed','status':{'state':'installed','reason':'reapplied'}}
        with mock.patch.object(watcher.time,'sleep',side_effect=[None,None,KeyboardInterrupt]),mock.patch.object(watcher,'watch_patch_once',side_effect=[(state,{'event':'native_patch_installed','reason':'reapplied'}),(state,None)]),mock.patch.object(watcher,'_normal_app_open',return_value=False),mock.patch.object(watcher,'sync') as sync,mock.patch('builtins.print'):
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        sync.assert_not_called()

    def test_pending_account_changes_reconcile_when_user_opens_reapplied_app(self):
        state={'revision':'changed','status':{'state':'installed','reason':'reapplied'}}
        with mock.patch.object(watcher.time,'sleep',side_effect=[None,None,KeyboardInterrupt]),mock.patch.object(watcher,'watch_patch_once',return_value=(state,None)),mock.patch.object(watcher,'_normal_app_open',side_effect=[False,True]),mock.patch.object(watcher,'watch_once',return_value=('accounts',None)) as reconcile:
            with self.assertRaises(KeyboardInterrupt):watcher.main()
        reconcile.assert_called_once_with(None)

    def test_app_open_lookup_requires_exact_current_user_executable(self):
        executable=str(self.app/'Contents/MacOS/ChatGPT')
        for rows,expected in [(f'42 {os.getuid()} {executable}\n',True),
                              (f'41 -2 /usr/libexec/dhcp6d\n42 {os.getuid()} {executable}\n',True),
                              (f'42 {os.getuid()+1} {executable}\n',False),
                              (f'42 {os.getuid()} {executable}Helper\n',False),
                              (f'42 {os.getuid()} {executable} --user-data-dir=/research\n',False),
                              ('malformed process listing',False)]:
            with self.subTest(rows=rows),mock.patch.object(watcher.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=rows)):
                self.assertEqual(watcher._normal_app_open(),expected)


if __name__=='__main__':unittest.main()
