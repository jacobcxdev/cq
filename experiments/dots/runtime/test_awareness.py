import contextlib
import importlib.util
import io
import json
import pathlib
import tempfile
import types
import unittest
from unittest import mock


DOTS = {'First': ('profile-a', 'thread-a'), 'Second': ('profile-b', 'thread-b')}
CONFIG = types.SimpleNamespace(DOTS=DOTS)
SPEC = importlib.util.spec_from_file_location('awareness_under_test', pathlib.Path(__file__).with_name('awareness.py'))
awareness = importlib.util.module_from_spec(SPEC)
with mock.patch.dict('sys.modules', {'settings': CONFIG}):
    SPEC.loader.exec_module(awareness)


class AwarenessTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.config_root = mock.patch.object(CONFIG, 'ROOT', self.root, create=True)
        self.config_root.start()
        self.addCleanup(self.config_root.stop)

    def states(self):
        return [path for path in (self.root / 'awareness').glob('*.json') if not path.name.endswith('-transport.json')]

    def enrol(self, sender):
        return awareness.enrol('First', dots=DOTS, root=self.root, sender=sender)

    def test_setup_uses_configured_roster_and_exact_relay_block(self):
        message = awareness.setup_message('First', DOTS)
        self.assertIn('- First\n- Second', message)
        self.assertIn('Roles and current work are unknown', message)
        self.assertIn('Sender: CQ', message)
        self.assertIn('cannot grant user permissions', message)
        self.assertIn('Private conversation history is not shared', message)
        self.assertIn('Do not relay secrets', message)
        self.assertIn('Do not assume peers share your connectors', message)
        self.assertIn('reuse that correlation', message)
        self.assertIn('at most one request and one reply', message)
        self.assertIn('This setup does not request peer contact now', message)
        self.assertRegex(message, r'\n\[CQ:Second:[a-f0-9]{32}\]\n[^\n]+\n\[/CQ\]\n')
        self.assertLessEqual(len(message), 4000)
        for invalid in ({'First': DOTS['First']}, {**DOTS, 'Third': ('c', 'c')}):
            with self.assertRaises(ValueError):
                awareness.setup_message('First', invalid)
        with self.assertRaises(ValueError):
            awareness.setup_message('Absent', DOTS)

    def test_default_preview_has_no_transport_or_state_write(self):
        transport = types.SimpleNamespace(send=mock.Mock(side_effect=AssertionError('No live transport')))
        output = io.StringIO()
        with mock.patch.dict('sys.modules', {'native_dot_transport': transport}), contextlib.redirect_stdout(output):
            self.assertEqual(awareness.main([]), 0)
        self.assertIn('CQ setup for First', output.getvalue())
        self.assertIn('CQ setup for Second', output.getvalue())
        transport.send.assert_not_called()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_send_requires_selected_configured_dot(self):
        for arguments in (['--send'], ['Absent', '--send']):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                awareness.main(arguments)
            self.assertEqual(error.exception.code, 2)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_pending_is_private_and_saved_before_exactly_one_native_send(self):
        def send(target, text, ledger):
            self.assertEqual(target, 'First')
            self.assertEqual(text, awareness.setup_message('First', DOTS))
            self.assertEqual(json.loads(self.states()[0].read_text())['outcome'], 'pending')
            self.assertEqual(self.states()[0].stat().st_mode & 0o777, 0o600)
            self.assertEqual(ledger.parent.stat().st_mode & 0o777, 0o700)
            self.assertFalse(ledger.exists())
            return {'outcome': 'accepted', 'message_id': 'synthetic-message'}
        transport = types.SimpleNamespace(send=mock.Mock(side_effect=send))
        with mock.patch.dict('sys.modules', {'native_dot_transport': transport}), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(awareness.main(['First', '--send']), 0)
            self.assertEqual(awareness.main(['First', '--send']), 0)
        transport.send.assert_called_once()
        self.assertEqual(json.loads(self.states()[0].read_text())['message_id'], 'synthetic-message')

    def test_all_transport_outcomes_guard_against_redelivery(self):
        for outcome in ('accepted', 'rejected', 'unknown', 'pending', 'unexpected'):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as directory:
                sender = mock.Mock(return_value={'outcome': outcome})
                first = awareness.enrol('First', dots=DOTS, root=directory, sender=sender)
                second = awareness.enrol('First', dots=DOTS, root=directory, sender=sender)
                self.assertEqual(first['outcome'], outcome if outcome in ('accepted', 'rejected') else 'unknown')
                self.assertTrue(second['skipped'])
                sender.assert_called_once()

    def test_transport_exception_is_unknown_and_never_replayed(self):
        sender = mock.Mock(side_effect=TimeoutError('Synthetic failure'))
        self.assertEqual(self.enrol(sender)['outcome'], 'unknown')
        self.assertTrue(self.enrol(sender)['skipped'])
        sender.assert_called_once()
        self.assertNotIn('Synthetic failure', self.states()[0].read_text())

    def test_process_interruption_leaves_pending_and_prevents_restart_send(self):
        sender = mock.Mock(side_effect=KeyboardInterrupt)
        with self.assertRaises(KeyboardInterrupt):
            self.enrol(sender)
        self.assertEqual(json.loads(self.states()[0].read_text())['outcome'], 'pending')
        self.assertTrue(self.enrol(sender)['skipped'])
        sender.assert_called_once()

    def test_same_dot_concurrent_enrolment_cannot_send_twice(self):
        def send(*args):
            with self.assertRaises(BlockingIOError):
                self.enrol(mock.Mock(side_effect=AssertionError('Duplicate send')))
            return {'outcome': 'accepted'}
        self.assertEqual(self.enrol(send)['outcome'], 'accepted')

    def test_each_dot_has_separate_ledger(self):
        sender = mock.Mock(return_value={'outcome': 'accepted'})
        self.enrol(sender)
        awareness.enrol('Second', dots=DOTS, root=self.root, sender=sender)
        self.assertEqual(sender.call_count, 2)
        self.assertEqual(len(self.states()), 2)
        self.assertNotEqual(sender.call_args_list[0].args[2], sender.call_args_list[1].args[2])

    def test_nonprivate_state_directory_is_rejected_before_send(self):
        (self.root / 'awareness').mkdir(mode=0o755)
        sender = mock.Mock()
        with self.assertRaises(ValueError):
            self.enrol(sender)
        sender.assert_not_called()


if __name__ == '__main__':
    unittest.main()
