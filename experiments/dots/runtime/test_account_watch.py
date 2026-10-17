import fcntl
import importlib.util
import pathlib
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


if __name__=='__main__':unittest.main()
