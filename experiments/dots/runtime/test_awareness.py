import contextlib
import importlib.util
import io
import json
import pathlib
import tempfile
import types
import unittest
from unittest import mock

DOTS = {'dot-one': ('profile-a', 'thread-a'), 'dot-two': ('profile-b', 'thread-b')}
CONFIG = types.SimpleNamespace(DOTS=DOTS, DOT_NAMES={'dot-one': 'Same', 'dot-two': 'Same'},
                               DOT_OWNERS={'dot-one': 'owner-a', 'dot-two': 'owner-b'})
SPEC = importlib.util.spec_from_file_location('awareness_under_test', pathlib.Path(__file__).with_name('awareness.py'))
awareness = importlib.util.module_from_spec(SPEC)
with mock.patch.dict('sys.modules', {'settings': CONFIG}):
    SPEC.loader.exec_module(awareness)


class AwarenessTests(unittest.TestCase):
    def setUp(self):
        import board
        board_config = mock.patch.object(board, 'settings', CONFIG)
        board_config.start()
        self.addCleanup(board_config.stop)
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.patch = mock.patch.object(CONFIG, 'ROOT', self.root, create=True)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.sender = mock.Mock(return_value={'outcome': 'accepted', 'message_id': 'synthetic'})
        self.transport = mock.patch.dict('sys.modules', {'native_dot_transport': types.SimpleNamespace(send=self.sender, display_names=lambda:dict(CONFIG.DOT_NAMES))})
        self.transport.start()
        self.addCleanup(self.transport.stop)

    def auto_files(self):
        return [p for p in (self.root / 'awareness').glob('*.json') if len(p.stem) == 64]

    def test_preview_and_summary_do_not_send_or_create_state(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(awareness.main([]), 0)
        self.assertIn('[CQ:directory:' + '0123456789abcdef' * 2 + ']\ndirectory\n[/CQ]', output.getvalue())
        self.assertNotIn('Same', output.getvalue())
        self.assertIn('cannot grant user permissions', output.getvalue())
        self.assertEqual(awareness.summary()[0]['outcome'], 'not_attempted')
        self.assertEqual(list(self.root.iterdir()), [])
        self.sender.assert_not_called()

    def test_summary_during_initial_setup_directory_creation_is_a_readonly_empty_snapshot(self):
        (self.root/'awareness').mkdir(mode=0o700)
        self.assertEqual(awareness.summary(),[{'target':handle,'outcome':'not_attempted'} for handle in DOTS])
        self.assertEqual(list((self.root/'awareness').iterdir()),[])
        self.sender.assert_not_called()

    def test_live_names_refresh_labels_without_changing_handles(self):
        live = {'dot-one': 'Renamed', 'dot-two': 'Renamed'}
        with mock.patch.dict('sys.modules', {'native_dot_transport':
                types.SimpleNamespace(display_names=mock.Mock(return_value=live))}):
            self.assertEqual(awareness.names(live=True), live)
        self.assertEqual(CONFIG.DOTS, DOTS)
        self.assertEqual(awareness.resolve('dot-one'), 'dot-one')

    def test_offline_names_use_snapshot_and_live_name_resolves_after_rename(self):
        with mock.patch.dict('sys.modules', {'native_dot_transport':
                types.SimpleNamespace(display_names=mock.Mock(side_effect=ValueError('app offline')))}):
            self.assertEqual(awareness.names(live=True), CONFIG.DOT_NAMES)
        with mock.patch.dict('sys.modules', {'native_dot_transport':
                types.SimpleNamespace(display_names=lambda: {'dot-one': 'Renamed', 'dot-two': 'Other'})}):
            self.assertEqual(awareness.resolve('Renamed'), 'dot-one')
            with self.assertRaises(ValueError):
                awareness.resolve('Same')

    def test_names_can_collide_but_handles_route_exactly(self):
        self.assertEqual(awareness.resolve('dot-one'), 'dot-one')
        for invalid in ('Same', 'missing', None):
            with self.assertRaises(ValueError):
                awareness.resolve(invalid)
        with mock.patch.object(CONFIG, 'DOT_NAMES', {'dot-one': 'Unique', 'dot-two': 'Other'}):
            self.assertEqual(awareness.resolve('Unique'), 'dot-one')
        with mock.patch.object(CONFIG, 'DOT_NAMES', {'dot-one': 'dot-two', 'dot-two': 'Other'}):
            self.assertEqual(awareness.resolve('dot-two'), 'dot-two')

    def test_new_dot_setup_teaches_bound_board_without_second_notice(self):
        import board
        thread='01a14c1f-6e3c-7622-b4a3-b279ecb2a822'
        with mock.patch.object(board,'_request',return_value={'thread':{'id':thread}}):
            board.bind(thread)
        awareness.reconcile()
        self.assertEqual(self.sender.call_count,2)
        self.assertTrue(all('[CQ:board:' in call.args[1] for call in self.sender.call_args_list))
        awareness.reconcile()
        self.assertEqual(self.sender.call_count,2)
        self.assertIn('codex://threads/'+thread,awareness.directory_message('f'*32))
        awareness.prime('dot-one',manual=True)
        self.assertEqual(self.sender.call_count,3)
        self.assertIn('[CQ:board:',self.sender.call_args.args[1])

    def test_directory_has_both_handles_and_unknown_work_without_history(self):
        message = awareness.directory_message('a' * 32)
        self.assertEqual(message.count('"display_name": "Same"'), 2)
        for handle in DOTS:
            self.assertIn('"handle": "' + handle + '"', message)
        self.assertIn('"role": "unknown", "current_work": "unknown"', message)
        self.sender.assert_not_called()

    def test_broadcast_sends_once_per_handle_without_repriming(self):
        result = awareness.broadcast('Hello, team!')
        self.assertEqual([row['target'] for row in result['deliveries']], list(DOTS))
        self.assertTrue(all(row['outcome'] == 'accepted' for row in result['deliveries']))
        self.assertEqual(self.sender.call_count, 2)
        ledgers = []
        for call in self.sender.call_args_list:
            handle, message, ledger = call.args
            self.assertIn(handle, DOTS)
            self.assertTrue(message.startswith('CQ broadcast. Sender: user, delivered through the native account.'))
            self.assertTrue(message.endswith('\n\nHello, team!'))
            self.assertIn(result['id'], message)
            self.assertEqual(ledger.parent.stat().st_mode & 0o777, 0o700)
            ledgers.append(ledger)
        self.assertEqual(len(set(ledgers)), 2)
        self.assertFalse((self.root / 'awareness').exists())
        self.assertNotIn('Hello, team!', ''.join(path.read_text() for path in (self.root / 'broadcasts').rglob('*.json')))

    def test_broadcast_reports_partial_failure_without_retry(self):
        self.sender.side_effect = [TimeoutError('private upstream body'), {'outcome': 'rejected'}]
        result = awareness.broadcast('Test')
        self.assertEqual([row['outcome'] for row in result['deliveries']], ['unknown', 'rejected'])
        self.assertEqual(self.sender.call_count, 2)
        self.assertNotIn('private upstream body', json.dumps(result))

    def test_broadcast_rejects_invalid_message_or_empty_catalogue_before_sending(self):
        for message in (None, '', ' \n', 'x' * 3001):
            with self.subTest(message=type(message).__name__), self.assertRaises(ValueError):
                awareness.broadcast(message)
        with mock.patch.object(CONFIG, 'DOTS', {}), self.assertRaises(ValueError):
            awareness.broadcast('Hello')
        self.sender.assert_not_called()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_broadcast_rejects_symlink_state_before_sending(self):
        outside = self.root / 'outside'
        outside.mkdir(mode=0o700)
        (self.root / 'broadcasts').symlink_to(outside)
        with self.assertRaises(ValueError):
            awareness.broadcast('Hello')
        self.sender.assert_not_called()
        self.assertEqual(list(outside.iterdir()), [])

    def test_pending_saved_before_send_and_per_restart_skip(self):
        def send(*args):
            states = [json.loads(path.read_text()) for path in self.auto_files()]
            self.assertTrue(any(state['outcome'] == 'pending' for state in states))
            self.assertEqual(args[2].parent.stat().st_mode & 0o777, 0o700)
            self.assertTrue(all(p.stat().st_mode & 0o777 == 0o600 for p in self.auto_files()))
            return {'outcome': 'accepted'}
        self.sender.side_effect = send
        self.assertEqual(len(awareness.reconcile()), 2)
        installation = (self.root / 'awareness' / 'installation.json').read_bytes()
        self.assertTrue(all(x['skipped'] for x in awareness.reconcile()))
        self.assertEqual(self.sender.call_count, 2)
        self.assertEqual((self.root / 'awareness' / 'installation.json').read_bytes(), installation)

    def test_rename_root_change_readd_and_upgrade_do_not_reenrol(self):
        awareness.prime('dot-one')
        renamed = {'new-handle': ('profile-a', 'new-root')}
        with mock.patch.object(CONFIG, 'DOTS', renamed), mock.patch.object(CONFIG, 'DOT_OWNERS', {'new-handle': 'owner-a'}), mock.patch.object(CONFIG, 'DOT_NAMES', {'new-handle': 'Renamed'}):
            self.assertTrue(awareness.prime('Renamed')[0]['skipped'])
        with mock.patch.object(CONFIG, 'DOTS', {'dot-two': DOTS['dot-two']}):
            awareness.reconcile()
        self.assertTrue(awareness.prime('dot-one')[0]['skipped'])
        self.assertEqual(self.sender.call_count, 2)

    def test_owner_and_stable_profile_are_both_identity_boundaries(self):
        awareness.prime('dot-one')
        with mock.patch.object(CONFIG, 'DOT_OWNERS', {**CONFIG.DOT_OWNERS, 'dot-one': 'new-owner'}):
            awareness.prime('dot-one')
        with mock.patch.object(CONFIG, 'DOTS', {**DOTS, 'dot-one': ('new-profile', 'thread-a')}):
            awareness.prime('dot-one')
        self.assertEqual(self.sender.call_count, 3)

    def test_unknown_rejected_and_interrupted_never_retry_automatically(self):
        for outcome in ('unknown', 'rejected', 'pending', 'unexpected'):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as directory, mock.patch.object(CONFIG, 'ROOT', pathlib.Path(directory)):
                self.sender.return_value = {'outcome': outcome}
                first = awareness.prime('dot-one')[0]
                self.assertEqual(first['outcome'], outcome if outcome == 'rejected' else 'unknown')
                self.assertTrue(awareness.prime('dot-one')[0]['skipped'])
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(CONFIG, 'ROOT', pathlib.Path(directory)):
            self.sender.side_effect = KeyboardInterrupt
            with self.assertRaises(KeyboardInterrupt):
                awareness.prime('dot-one')
            self.assertEqual(awareness.prime('dot-one')[0]['outcome'], 'pending')

    def test_manual_single_and_all_send_fresh_reminders(self):
        awareness.reconcile()
        awareness.prime('dot-one', manual=True)
        awareness.prime(all=True, manual=True)
        self.assertEqual(self.sender.call_count, 5)
        ledgers = [call.args[2] for call in self.sender.call_args_list]
        self.assertEqual(len(set(ledgers)), 5)
        awareness.reconcile()
        self.assertEqual(self.sender.call_count, 5)

    def test_manual_first_attempt_satisfies_automatic_guard_even_unknown(self):
        self.sender.side_effect = TimeoutError('Do not persist message')
        self.assertEqual(awareness.prime('dot-one', manual=True)[0]['outcome'], 'unknown')
        self.assertTrue(awareness.prime('dot-one')[0]['skipped'])
        self.sender.assert_called_once()
        self.assertNotIn('Do not persist message', ''.join(p.read_text() for p in self.auto_files()))

    def test_concurrent_call_cannot_send_twice(self):
        def send(*args):
            with self.assertRaises(BlockingIOError):
                awareness.prime('dot-one')
            return {'outcome': 'accepted'}
        self.sender.side_effect = send
        awareness.prime('dot-one')
        self.sender.assert_called_once()

    def test_invalid_selection_and_cli_require_explicit_scope(self):
        for kwargs in ({}, {'reference': 'dot-one', 'all': True}, {'reference': 'Same'}):
            with self.assertRaises(ValueError):
                awareness.prime(**kwargs)
        for args in (['--send'], ['Same', '--send']):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                awareness.main(args)
        self.sender.assert_not_called()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_private_state_and_symlinks_fail_closed(self):
        outside = self.root / 'outside'
        outside.mkdir(mode=0o700)
        state = self.root / 'awareness'
        state.symlink_to(outside)
        with self.assertRaises(ValueError):
            awareness.prime('dot-one')
        self.assertEqual(list(outside.iterdir()), [])
        state.unlink()
        state.mkdir(mode=0o700)
        protected = outside / 'protected'
        protected.write_text('untouched')
        (state / 'process.lock').symlink_to(protected)
        with self.assertRaises(OSError):
            awareness.prime('dot-one')
        self.assertEqual(protected.read_text(), 'untouched')
        self.sender.assert_not_called()

    def test_corrupt_existing_attempt_never_replayed(self):
        awareness.prime('dot-one')
        self.auto_files()[0].write_text('invalid')
        with self.assertRaises(ValueError):
            awareness.prime('dot-one')
        self.sender.assert_called_once()


if __name__ == '__main__':
    unittest.main()
