import test_support
import io
import socket
import ssl
import threading
import types
import unittest
from unittest.mock import MagicMock, patch

import probe

PATH = '/backend-api/wham/remote/control/server'


class RemoteControlTests(unittest.TestCase):
    def handler(self, path=PATH, method='GET', body=b''):
        handler = probe.Handler.__new__(probe.Handler)
        handler.path = path; handler.command = method; handler.request_version = 'HTTP/1.1'
        handler.raw_requestline = f'{method} {path} HTTP/1.1\r\n'.encode()
        handler.headers = {'Host': 'localhost:8000', 'Authorization': 'Bearer native-desktop',
                           'ChatGPT-Account-Id': 'native-account', 'Cookie': 'native-cookie',
                           'X-OpenAI-Account-Routing-Override': 'us', 'Connection': 'Upgrade',
                           'Upgrade': 'websocket', 'Sec-WebSocket-Key': 'native-key',
                           'Sec-WebSocket-Protocol': 'native-protocol', 'Content-Length': str(len(body))}
        handler.rfile = io.BytesIO(body); handler.wfile = io.BytesIO(); handler.connection = MagicMock()
        handler.send_response = MagicMock(); handler.end_headers = MagicMock()
        return handler

    def test_only_exact_native_primary_upgrade_enters_tunnel(self):
        cases = [('native', PATH, 'GET', True), ('isolated', PATH, 'GET', False),
                 ('native', PATH + '?other=1', 'GET', False),
                 ('native', '/backend-api/ordinary', 'GET', False),
                 ('native', '/backend-api/tbo/worker/runtime/pause', 'GET', False),
                 ('native', PATH, 'POST', False)]
        for mode, path, method, expected in cases:
            with self.subTest(mode=mode, path=path, method=method):
                handler = self.handler(path, method)
                with patch.object(probe.settings, 'INTEGRATION_MODE', mode, create=True), patch.object(probe, 'CATALOG_FIXTURE', None), patch.object(probe.NATIVE_GATE, 'require'), patch.object(probe.Handler, 'forward_remote_control') as tunnel, patch.object(probe.Handler, 'forward_primary'), patch.object(probe.http.client, 'HTTPSConnection'), patch.object(probe, 'event'):
                    handler.handle_request()
                self.assertEqual(tunnel.called, expected)

    def test_upgrade_with_body_or_chunked_encoding_rejected_before_tunnel(self):
        for headers, body in [({}, b'body'), ({'Transfer-Encoding': 'chunked'}, b'')]:
            handler = self.handler(body=body); handler.headers.update(headers)
            if 'Transfer-Encoding' in headers: handler.headers.pop('Content-Length')
            with patch.object(probe.settings, 'INTEGRATION_MODE', 'native', create=True), patch.object(probe, 'CATALOG_FIXTURE', None), patch.object(probe.Handler, 'forward_remote_control') as tunnel:
                handler.handle_request()
            tunnel.assert_not_called(); handler.send_response.assert_called_once_with(400)

    def test_missing_upgrade_uses_existing_http_forwarder(self):
        handler = self.handler(); handler.headers.pop('Upgrade')
        with patch.object(probe.settings, 'INTEGRATION_MODE', 'native', create=True), patch.object(probe, 'CATALOG_FIXTURE', None), patch.object(probe.Handler, 'forward_remote_control') as tunnel, patch.object(probe.Handler, 'forward_primary') as ordinary:
            handler.handle_request()
        tunnel.assert_not_called(); ordinary.assert_called_once_with(0, False)

    def test_public_tls_failure_fails_closed_without_forwarding_auth(self):
        handler = self.handler(); raw = MagicMock(); tls = MagicMock()
        tls.wrap_socket.side_effect = ssl.SSLCertVerificationError('untrusted upstream')
        with patch.object(probe.socket, 'create_connection', return_value=raw), patch.object(probe.ssl, 'create_default_context', return_value=tls), patch.object(probe, 'event'):
            handler.forward_remote_control()
        raw.sendall.assert_not_called(); raw.close.assert_called_once()
        handler.send_response.assert_called_once_with(502)

    def test_rejected_or_oversized_handshake_never_starts_frame_reader(self):
        rejected = b'HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n'
        for response, expected in [(rejected, rejected), (b'x' * 65536, b'')]:
            handler = self.handler(); upstream = MagicMock(); tls = MagicMock()
            tls.wrap_socket.return_value = upstream; upstream.recv.return_value = response
            with patch.object(probe.socket, 'create_connection', return_value=upstream), patch.object(probe.ssl, 'create_default_context', return_value=tls), patch.object(probe, 'event'), patch.object(probe.threading, 'Thread') as reader:
                handler.forward_remote_control()
            reader.assert_not_called(); self.assertEqual(handler.wfile.getvalue(), expected)
            if expected: handler.send_response.assert_not_called()
            else: handler.send_response.assert_called_once_with(502)
            upstream.close.assert_called_once()

    def test_client_eof_releases_upstream_and_proxy_handler(self):
        client, proxy = socket.socketpair(); upstream, backend = socket.socketpair()
        for sock in [client, proxy, upstream, backend]: self.addCleanup(sock.close)
        client.settimeout(2); backend.settimeout(2)
        tls = MagicMock(); tls.wrap_socket.return_value = upstream
        request = (f'GET {PATH} HTTP/1.1\r\nHost: localhost:8000\r\n'
                   'Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n').encode()
        response = b'HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n'
        with patch.object(probe.settings, 'INTEGRATION_MODE', 'native', create=True), patch.object(probe, 'CATALOG_FIXTURE', None), patch.object(probe.socket, 'create_connection', return_value=upstream), patch.object(probe.ssl, 'create_default_context', return_value=tls), patch.object(probe, 'event'):
            worker = threading.Thread(target=probe.Handler, args=(proxy, ('127.0.0.1', 1), types.SimpleNamespace()), daemon=True)
            worker.start(); client.sendall(request)
            received = b''
            while not received.endswith(b'\r\n\r\n'): received += backend.recv(65536)
            backend.sendall(response)
            received = b''
            while len(received) < len(response): received += client.recv(65536)
            client.shutdown(socket.SHUT_WR)
            self.assertEqual(backend.recv(65536), b'')
            worker.join(2)
        self.assertFalse(worker.is_alive())

    def test_upgrade_preserves_native_identity_and_opaque_frames_both_directions(self):
        client, proxy = socket.socketpair(); upstream, backend = socket.socketpair()
        for sock in [client, proxy, upstream, backend]: self.addCleanup(sock.close)
        client.settimeout(2); backend.settimeout(2)
        tls = MagicMock(); tls.wrap_socket.return_value = upstream
        # Ping, pong and close bytes stay opaque; masking and backpressure belong to native peers.
        first_frame = b'\x89\x84testping'; reply_frame = b'\x8a\x04pong'; close_frame = b'\x88\x00'
        request = (f'GET {PATH} HTTP/1.1\r\nHost: localhost:8000\r\n'
                   'Authorization: Bearer native-desktop\r\nChatGPT-Account-Id: native-account\r\n'
                   'Cookie: native-cookie\r\nX-OpenAI-Account-Routing-Override: us\r\n'
                   'Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: native-key\r\n'
                   'Sec-WebSocket-Protocol: native-protocol\r\n\r\n').encode()
        response = (b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n'
                    b'Connection: Upgrade\r\nSec-WebSocket-Protocol: native-protocol\r\n\r\n')
        with patch.object(probe.settings, 'INTEGRATION_MODE', 'native', create=True), patch.object(probe, 'CATALOG_FIXTURE', None), patch.object(probe.socket, 'create_connection', return_value=upstream) as connect, patch.object(probe.ssl, 'create_default_context', return_value=tls) as context, patch.object(probe, 'owner_headers', side_effect=AssertionError('native identity must not be substituted')), patch.object(probe.NATIVE_GATE, 'require') as gate, patch.object(probe.http.client, 'HTTPSConnection', side_effect=AssertionError('ordinary HTTP cannot carry upgrade')), patch.object(probe, 'event'):
            worker = threading.Thread(target=probe.Handler, args=(proxy, ('127.0.0.1', 1), types.SimpleNamespace()), daemon=True)
            worker.start(); client.sendall(request + first_frame)
            received = b''
            while not received.endswith(b'\r\n\r\n'): received += backend.recv(65536)
            self.assertEqual(received, request.replace(b'Host: localhost:8000', b'Host: chatgpt.com'))
            backend.sendall(response + reply_frame)
            result = b''
            while len(result) < len(response + reply_frame): result += client.recv(65536)
            self.assertEqual(result, response + reply_frame)
            self.assertEqual(backend.recv(65536), first_frame)
            client.sendall(close_frame); self.assertEqual(backend.recv(65536), close_frame)
            backend.sendall(close_frame); backend.shutdown(socket.SHUT_WR)
            tail = b''
            while data := client.recv(65536): tail += data
            worker.join(2)
        self.assertFalse(worker.is_alive()); self.assertEqual(tail, close_frame)
        connect.assert_called_once_with(('chatgpt.com', 443), timeout=25)
        context.assert_called_once_with(cafile=ssl.get_default_verify_paths().openssl_cafile)
        tls.wrap_socket.assert_called_once_with(upstream, server_hostname='chatgpt.com')
        gate.assert_not_called()


if __name__ == '__main__': unittest.main()
