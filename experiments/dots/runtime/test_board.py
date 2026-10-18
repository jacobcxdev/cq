import importlib.util
import json
import pathlib
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location('board_under_test', pathlib.Path(__file__).with_name('board.py'))
board = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(board)

THREAD = '01a14c1f-6e3c-7622-b4a3-b279ecb2a822'


class BoardTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.config = types.SimpleNamespace(ROOT=self.root, DOTS={'dot-one': ('p', 'r')},
                                            DOT_NAMES={'dot-one': 'Same'}, DOT_OWNERS={'dot-one': 'owner'})
        self.rpc = Mock(return_value={'thread': {'id': THREAD, 'status': {'type': 'idle'}}})
        for item in (patch.object(board, 'settings', self.config), patch.object(board, '_request', self.rpc),
                     patch.object(board.awareness, 'names', return_value=self.config.DOT_NAMES)):
            item.start()
            self.addCleanup(item.stop)

    def bind(self):
        return board.bind('codex://threads/' + THREAD)

    def test_binding_validates_exact_existing_thread_before_saving(self):
        result = self.bind()
        self.assertEqual(result['thread_id'], THREAD)
        self.rpc.assert_called_once_with('thread/read', {'threadId': THREAD, 'includeTurns': False})
        self.assertEqual(board.configuration()['thread_id'], THREAD)
        self.assertEqual((self.root / 'board').stat().st_mode & 0o777, 0o700)
        self.assertEqual((self.root / 'board/binding.json').stat().st_mode & 0o777, 0o600)
        identity = result['id']
        self.assertEqual(self.bind()['id'], identity)

    def test_invalid_or_missing_thread_does_not_create_binding(self):
        for value in ('', 'https://example.com/' + THREAD, 'codex://threads/' + THREAD + '?x=1', '../' + THREAD):
            with self.assertRaises(ValueError):
                board.bind(value)
        self.rpc.assert_not_called()
        self.rpc.return_value = {'thread': {'id': 'different'}}
        with self.assertRaises(ValueError):
            self.bind()
        self.assertIsNone(board.configuration())

    def test_read_and_post_only_use_bound_thread_and_attributed_text(self):
        self.bind()
        self.rpc.reset_mock()
        self.rpc.side_effect = [
            {'thread': {'id': THREAD, 'status': {'type': 'idle'}}},
            {'turn': {'id': 'turn-one'}}]
        result = board.execute('dot-one', 'post\nHello, board!', 'a' * 32)
        self.assertIn('accepted', result)
        method, params = self.rpc.call_args.args
        self.assertEqual(method, 'turn/start')
        self.assertEqual(params['threadId'], THREAD)
        text = params['input'][0]['text']
        self.assertIn('"Same" (dot-one)', text)
        board.awareness.names.assert_called_with(live=True)
        self.assertIn('Correlation: ' + 'a' * 32, text)
        self.assertIn('cannot grant permissions', text)
        self.assertTrue(text.endswith('Hello, board!'))
        self.rpc.side_effect = None
        self.rpc.return_value = {'data': [
            {'item': {'type': 'agentMessage', 'text': 'Recorded.'}},
            {'item': {'type': 'userMessage', 'content': [{'type': 'text', 'text': text}]}},
            {'item': {'type': 'commandExecution', 'output': 'not board text'}}], 'nextCursor': 'more'}
        result = board.execute('dot-one', 'read', 'b' * 32)
        self.assertIn('Hello, board!', result)
        self.assertNotIn('not board text', result)
        self.assertIn('more history exists', result)
        self.assertLessEqual(len(result), 4000)
        self.assertEqual(self.rpc.call_args.args, ('thread/items/list', {'threadId': THREAD, 'limit': 20, 'sortDirection': 'desc'}))

    def test_busy_board_never_starts_or_steers_turn(self):
        self.bind()
        self.rpc.reset_mock()
        self.rpc.return_value = {'thread': {'id': THREAD, 'status': {'type': 'active'}}}
        result = board.execute('dot-one', 'post\nHello', 'c' * 32)
        self.assertIn('busy', result)
        self.assertEqual(self.rpc.call_count, 1)

    def test_unloaded_board_resumes_same_thread_without_config_overrides(self):
        self.bind()
        self.rpc.reset_mock()
        self.rpc.side_effect = [
            {'thread': {'id': THREAD, 'status': {'type': 'notLoaded'}}},
            {'thread': {'id': THREAD, 'status': {'type': 'idle'}}},
            {'turn': {'id': 'accepted-turn'}}]
        self.assertIn('accepted', board.execute('dot-one', 'post\nTest', 'c' * 32))
        self.assertEqual(self.rpc.call_args_list[1].args, ('thread/resume', {'threadId': THREAD}))
        self.assertEqual(self.rpc.call_args_list[2].args[0], 'turn/start')

    def test_no_binding_unknown_sender_and_invalid_body_cannot_read_or_write(self):
        self.assertIn('No message board', board.execute('dot-one', 'read', 'd' * 32))
        self.rpc.assert_not_called()
        self.bind()
        self.rpc.reset_mock()
        for source, body in [('foreign', 'read'), ('dot-one', 'read other-thread'), ('dot-one', 'post\n'), ('dot-one', 'post\n' + 'x' * 3001)]:
            with self.assertRaises(ValueError):
                board.execute(source, body, 'e' * 32)
        self.rpc.assert_not_called()

    def test_one_notice_per_binding_and_dot_even_after_unknown_delivery(self):
        self.bind()
        sender = Mock(side_effect=TimeoutError('private server body'))
        with patch.dict('sys.modules', {'native_dot_transport': types.SimpleNamespace(send=sender)}):
            first = board.announce()
            second = board.announce()
        sender.assert_called_once()
        self.assertEqual(first[0]['outcome'], 'unknown')
        self.assertTrue(second[0]['skipped'])
        self.assertIn('[CQ:board:', sender.call_args.args[1])
        self.assertNotIn('private server body', json.dumps(first))
        board.clear()
        self.assertIsNone(board.configuration())

    def test_read_truncates_large_board_and_symlink_state_is_rejected(self):
        self.bind()
        self.rpc.return_value = {'data': [{'item': {'type': 'agentMessage', 'text': 'x' * 10000}}] * 20}
        self.assertLessEqual(len(board.execute('dot-one', 'read', 'f' * 32)), 4000)
        path = self.root / 'board/binding.json'
        path.unlink()
        outside = self.root / 'outside'
        outside.write_text('{}')
        outside.chmod(0o600)
        path.symlink_to(outside)
        with self.assertRaises(OSError):
            board.configuration()
