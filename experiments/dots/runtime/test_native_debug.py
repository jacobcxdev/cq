import test_support
import json
import os
import pathlib
import select
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
import native_debug


class NativeDebugTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.module = native_debug

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='cqd-')
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.root.chmod(0o700)

    def pipe(self, handler):
        peer_read, write_fd = os.pipe()
        read_fd, peer_write = os.pipe()
        self.addCleanup(lambda: [os.close(fd) for fd in (read_fd, write_fd)])
        requests = []

        def peer():
            pending = bytearray()
            try:
                while True:
                    chunk = os.read(peer_read, 65536)
                    if not chunk:
                        return
                    pending.extend(chunk)
                    while b'\0' in pending:
                        data, _, pending = pending.partition(b'\0')
                        request = json.loads(data)
                        requests.append(request)
                        response = handler(request)
                        if response is not None:
                            data = json.dumps(response).encode() + b'\0'
                            # Include an unsolicited event and split the reply.
                            os.write(peer_write, b'{"method":"Runtime.executionContextCreated"}\0' + data[:5])
                            os.write(peer_write, data[5:])
            except OSError:
                pass
            finally:
                os.close(peer_read)
                os.close(peer_write)

        threading.Thread(target=peer, daemon=True).start()
        return self.module.Pipe(read_fd, write_fd), requests

    def broker(self, evaluator=lambda expression: {'expression': expression}):
        broker = self.module.Broker(self.root, 12345, evaluator)
        self.addCleanup(broker.close)
        return broker

    def request(self, broker, value):
        worker = threading.Thread(target=broker.once)
        worker.start()
        try:
            return self.module._request(value, self.root)
        finally:
            worker.join(2)
            self.assertFalse(worker.is_alive())

    def test_evaluate_uses_fixed_native_page_and_flattened_session(self):
        def handler(request):
            method = request['method']
            result = {
                'Target.getTargets': {'targetInfos': [
                    {'type': 'page', 'url': 'https://evil.invalid', 'targetId': '0'},
                    {'type': 'page', 'url': 'app://-/index.html', 'targetId': 'b'},
                    {'type': 'page', 'url': 'app://-/index.html', 'targetId': 'a'}]},
                'Target.attachToTarget': {'sessionId': 'native-session'},
                'Runtime.evaluate': {'result': {'type': 'object', 'value': {'ready': True}}},
                'Target.detachFromTarget': {},
            }[method]
            return {'id': request['id'], 'result': result}

        pipe, requests = self.pipe(handler)
        self.assertEqual(pipe.evaluate('nativeApi()'), {'ready': True})
        self.assertEqual(requests[1]['params'], {'targetId': 'a', 'flatten': True})
        self.assertEqual(requests[2]['sessionId'], 'native-session')
        self.assertEqual(requests[2]['params'], {'expression': 'nativeApi()', 'awaitPromise': True, 'returnByValue': True})
        self.assertEqual(requests[-1]['method'], 'Target.detachFromTarget')

    def test_no_native_page_does_not_attach_to_foreign_target(self):
        pipe, requests = self.pipe(lambda request: {'id': request['id'], 'result': {'targetInfos': [
            {'type': 'page', 'url': 'https://chatgpt.com/', 'targetId': 'wrong'}]}})
        with self.assertRaisesRegex(ValueError, 'renderer unavailable'):
            pipe.evaluate('nativeApi()')
        self.assertEqual([request['method'] for request in requests], ['Target.getTargets'])

    def test_exception_is_generic_and_session_is_detached(self):
        def handler(request):
            result = {'targetInfos': [{'type': 'page', 'url': 'app://-/index.html', 'targetId': 'a'}]}
            if request['method'] == 'Target.attachToTarget': result = {'sessionId': 's'}
            if request['method'] == 'Runtime.evaluate': result = {'exceptionDetails': {'text': 'private contents'}}
            if request['method'] == 'Target.detachFromTarget': result = {}
            return {'id': request['id'], 'result': result}
        pipe, requests = self.pipe(handler)
        with self.assertRaisesRegex(ValueError, '^Native API invocation failed$'):
            pipe.evaluate('nativeApi()')
        self.assertEqual(requests[-1]['method'], 'Target.detachFromTarget')

    def test_command_timeout_does_not_close_pipe(self):
        pipe, _ = self.pipe(lambda request: None)
        with self.assertRaisesRegex(ValueError, 'timed out'):
            pipe.call('Target.getTargets', deadline=time.monotonic() + 0.01)
        os.fstat(pipe.read_fd)
        os.fstat(pipe.write_fd)

    def test_pipe_command_errors_do_not_leak_remote_error(self):
        pipe, _ = self.pipe(lambda request: {'id': request['id'], 'error': {'message': 'secret'}})
        with self.assertRaisesRegex(ValueError, '^Native debugger command failed$'):
            pipe.call('Target.getTargets')

    def test_oversized_unsolicited_frame_is_discarded_without_losing_reply(self):
        peer_read, write_fd = os.pipe()
        read_fd, peer_write = os.pipe()
        self.addCleanup(lambda: [os.close(fd) for fd in (read_fd, write_fd)])

        def peer():
            try:
                request = json.loads(os.read(peer_read, 4096).rstrip(b'\0'))
                # Cross multiple reads while the oversized frame is discarded.
                data = b'{"event":"' + b'x' * 70000 + b'"}\0'
                data += json.dumps({'id': request['id'], 'result': {'ready': True}}).encode() + b'\0'
                while data:
                    data = data[os.write(peer_write, data):]
            finally:
                os.close(peer_read)
                os.close(peer_write)

        with mock.patch.object(self.module, 'MAX_FRAME', 1024):
            pipe = self.module.Pipe(read_fd, write_fd)
            threading.Thread(target=peer, daemon=True).start()
            self.assertEqual(pipe.call('Target.getTargets'), {'ready': True})

    def test_spawn_maps_only_pipe_fds_and_has_no_http_flag(self):
        script = (
            'import json,os,sys; data=os.read(3,4096); '
            'os.write(4,json.dumps({"data":data.decode(),"args":sys.argv[1:]}).encode()+b"\\0")'
        )
        pid, read_fd, write_fd = self.module.spawn_app(sys.executable, ['-c', script], os.environ.copy())
        try:
            os.write(write_fd, b'pipe-only')
            self.assertTrue(select.select([read_fd], [], [], 3)[0])
            result = json.loads(os.read(read_fd, 65536).rstrip(b'\0'))
            self.assertEqual(result, {'data': 'pipe-only', 'args': ['--remote-debugging-pipe']})
            self.assertEqual(os.waitpid(pid, 0)[1], 0)
        finally:
            os.close(read_fd)
            os.close(write_fd)

    def test_socket_is_private_and_evaluation_binds_app_pid(self):
        broker = self.broker()
        self.assertEqual(stat.S_IMODE(broker.path.stat().st_mode), 0o600)
        result = self.request(broker, {'operation': 'evaluate', 'app_pid': 12345, 'expression': 'one()'})
        self.assertEqual(result, {'ok': True, 'app_pid': 12345, 'value': {'expression': 'one()'}})
        identity = self.request(broker, {'operation': 'identity'})
        self.assertEqual(identity, {'ok': True, 'app_pid': 12345, 'broker_pid': os.getpid()})

    def test_wrong_pid_and_arbitrary_cdp_method_rejected(self):
        evaluator = mock.Mock()
        broker = self.broker(evaluator)
        for request in ({'operation': 'evaluate', 'app_pid': 12346, 'expression': 'one()'},
                        {'operation': 'Browser.close'},
                        {'operation': 'identity', 'extra': True}):
            with self.assertRaisesRegex(ValueError, 'request failed'):
                self.request(broker, request)
        evaluator.assert_not_called()

    def test_public_client_helpers_bind_app_identity(self):
        broker = self.broker()
        worker = threading.Thread(target=broker.once)
        worker.start()
        self.assertEqual(self.module.identity(self.root), {'app_pid': 12345, 'broker_pid': os.getpid()})
        worker.join(2)
        worker = threading.Thread(target=broker.once)
        worker.start()
        self.assertEqual(self.module.evaluate('one()', 12345, self.root), {'expression': 'one()'})
        worker.join(2)
        self.assertFalse(worker.is_alive())

    def test_wrong_peer_uid_rejected_before_expression(self):
        evaluator = mock.Mock()
        broker = self.broker(evaluator)
        with mock.patch.object(self.module, '_peer_uid', return_value=os.getuid() + 1):
            with self.assertRaisesRegex(ValueError, 'peer mismatch'):
                self.module._request({'operation': 'identity'}, self.root)
        evaluator.assert_not_called()

    def test_constructor_failure_cleans_own_socket_only(self):
        with mock.patch.object(self.module.os, 'chmod', side_effect=PermissionError):
            with self.assertRaises(PermissionError):
                self.module.Broker(self.root, 12345, None)
        self.assertFalse((self.root / self.module.SOCKET_NAME).exists())

    def test_directory_symlink_or_broad_permissions_rejected(self):
        self.root.chmod(0o755)
        with self.assertRaisesRegex(ValueError, '0700'):
            self.module.Broker(self.root, 12345, None)
        self.root.chmod(0o700)
        link = self.root / 'linked'
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, '0700'):
            self.module.Broker(link, 12345, None)

    def test_existing_socket_or_symlink_is_never_replaced(self):
        path = self.root / self.module.SOCKET_NAME
        path.symlink_to(self.root / 'elsewhere')
        with self.assertRaisesRegex(ValueError, 'already exists'):
            self.module.Broker(self.root, 12345, None)
        self.assertTrue(path.is_symlink())

    def test_client_rejects_socket_symlink_and_broad_permissions(self):
        broker = self.broker()
        broker.path.chmod(0o666)
        with self.assertRaisesRegex(ValueError, '0600'):
            self.module._request({'operation': 'identity'}, self.root)
        broker.path.unlink()
        broker.path.symlink_to(self.root / 'other')
        with self.assertRaisesRegex(ValueError, '0600'):
            self.module._request({'operation': 'identity'}, self.root)

    def test_oversized_request_rejected(self):
        broker = self.broker()
        with self.assertRaisesRegex(ValueError, 'frame too large'):
            self.module._request({'expression': 'x' * self.module.MAX_REQUEST}, self.root)

    def test_sigterm_stops_ipc_but_waits_for_app_own_exit(self):
        self.check_app_lifetime()

    def test_ipc_and_cleanup_failure_keep_live_app_pipe_open(self):
        self.check_app_lifetime(fail_ipc=True)

    def check_app_lifetime(self, fail_ipc=False):
        app = self.root / 'Official.app'
        executable = app / 'Contents/MacOS/ChatGPT'
        executable.parent.mkdir(parents=True)
        finish = self.root / 'finish'
        disconnected = self.root / 'disconnected'
        executable.write_text('#!' + sys.executable + '\n' +
            'import os,pathlib,select\n' +
            'while not pathlib.Path(' + repr(str(finish)) + ').exists():\n' +
            '    if select.select([3],[],[],0.05)[0] and not os.read(3,65536):\n' +
            '        pathlib.Path(' + repr(str(disconnected)) + ').write_text("pipe closed")\n' +
            '        break\n')
        executable.chmod(0o700)
        code = ('import pathlib,sys,types; '
                'sys.modules["settings"]=types.SimpleNamespace(ROOT=pathlib.Path(' + repr(str(self.root)) + '),APP=pathlib.Path(' + repr(str(app)) + ')); '
                'import native_debug\n')
        if fail_ipc:
            code += ('class BrokenBroker(native_debug.Broker):\n'
                     '    def once(self): raise OSError("simulated IPC failure")\n'
                     '    def close(self):\n'
                     '        super().close()\n'
                     '        raise OSError("simulated cleanup failure")\n'
                     'native_debug.Broker=BrokenBroker\n')
        code += 'native_debug.serve(pathlib.Path(' + repr(str(executable)) + '),[],__import__("os").environ.copy())'
        log = self.root / 'broker.log'
        with log.open('wb') as errors:
            process = subprocess.Popen([sys.executable, '-c', code], cwd=str(pathlib.Path(__file__).parent),
                                       stdout=subprocess.DEVNULL, stderr=errors)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        deadline = time.monotonic() + 5
        while not (self.root / self.module.STATE_NAME).exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertIsNone(process.poll(), log.read_text() if process.poll() is not None else None)
        state = json.loads((self.root / self.module.STATE_NAME).read_text())
        self.assertEqual(stat.S_IMODE((self.root / self.module.STATE_NAME).stat().st_mode), 0o600)
        if not fail_ipc:
            process.send_signal(signal.SIGTERM)
        deadline = time.monotonic() + 3
        while (self.root / self.module.SOCKET_NAME).exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertFalse((self.root / self.module.SOCKET_NAME).exists())
        self.assertIsNone(process.poll())
        os.kill(state['app_pid'], 0)
        self.assertFalse(disconnected.exists())
        finish.write_text('exit now')
        process.wait(timeout=5)
        self.assertEqual(process.returncode, 1 if fail_ipc else 0, log.read_text())
        self.assertFalse(disconnected.exists())
        self.assertFalse((self.root / self.module.STATE_NAME).exists())


if __name__ == '__main__':
    unittest.main()
