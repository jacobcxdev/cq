import test_support
import hashlib
import http.client
import http.server
import io
import json
import pathlib
import tempfile
import threading
import time
import unittest
from unittest.mock import MagicMock, patch

import probe


class NativeProbeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.main = dict(alias='11111111', account_id='main', token='stored-main', viewer_ids={'main-viewer'})
        self.worker = dict(alias='22222222', account_id='worker', token='stored-worker', viewer_ids={'worker-viewer'})
        self.profiles = [dict(id='main-dot', active_root_thread_id='main-root', messaging_room_id='main-room'),
                         dict(id='worker-dot', active_root_thread_id='worker-root', messaging_room_id='worker-room')]
        self.owners = {profile[key]: owner for profile, owner in zip(self.profiles, [self.main, self.worker])
                       for key in ['id', 'active_root_thread_id', 'messaging_room_id']}
        self.fixture = (self.main, self.profiles, self.owners, [])
        for name, value in [('INTEGRATION_MODE', 'native'), ('SYSTEM_ALIAS', self.main['alias']),
                            ('DOTS', {'main': ('main-dot', 'main-root'), 'worker': ('worker-dot', 'worker-root')}),
                            ('DOT_OWNERS', {'main': self.main['alias'], 'worker': self.worker['alias']})]:
            setting = patch.object(probe.settings, name, value, create=True)
            setting.start(); self.addCleanup(setting.stop)
        for name, value in [('CATALOG_FIXTURE', self.fixture), ('event', MagicMock()),
                            ('ROOT', self.root), ('CATALOG_GENERATION', 0), ('CHALLENGES', {}),
                            ('FILE_OWNERS', {}),
                            ('STATE', dict(probe.STATE, primary_probe_owner=None))]:
            setting = patch.object(probe, name, value)
            setting.start(); self.addCleanup(setting.stop)
        gate = patch.object(probe.NATIVE_GATE, 'require')
        self.gate = gate.start(); self.addCleanup(gate.stop)

    def handler(self, path, method='POST', body=b'unchanged\x00body'):
        handler = probe.Handler.__new__(probe.Handler)
        handler.path = path; handler.command = method
        handler.headers = {'Authorization': 'Bearer native-desktop', 'ChatGPT-Account-Id': 'main',
                           'Cookie': 'native-cookie', 'X-OpenAI-Orbit-Selection': 'native-selection',
                           'Accept-Encoding': 'gzip', 'Content-Length': str(len(body))}
        handler.connection = MagicMock(); handler.rfile = io.BytesIO(body); handler.wfile = io.BytesIO()
        handler.send_response = MagicMock(); handler.send_header = MagicMock(); handler.end_headers = MagicMock()
        return handler

    def connection(self, data=b'original response\x00', status=200, headers=()):
        connection = MagicMock(); response = connection.getresponse.return_value
        response.status = status; response.read.return_value = data
        response.getheaders.return_value = list(headers)
        response.getheader.side_effect = lambda name, default=None: next(
            (value for key, value in headers if key.lower() == name.lower()), default)
        response.read1.side_effect = [data, b'']
        def request(method, path, body=None, headers=None, **kwargs):
            connection.sent_body = body if isinstance(body, bytes) else b''.join(body or [])
        connection.request.side_effect = request
        return connection

    def test_primary_methods_preserve_native_auth_body_and_response(self):
        for method in ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']:
            with self.subTest(method=method):
                handler = self.handler('/backend-api/ordinary/resource?x=1', method)
                connection = self.connection(headers=[('Content-Encoding', 'gzip'),
                    ('Set-Cookie', 'native=value; Domain=chatgpt.com; Secure'), ('X-Upstream', 'original')])
                with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
                handler.send_response.assert_called_once_with(200)
                self.assertEqual(connection.request.call_args.args, (method, handler.path))
                sent = connection.request.call_args.kwargs
                self.assertEqual(connection.sent_body, b'unchanged\x00body')
                for name in ['Authorization', 'ChatGPT-Account-Id', 'Cookie', 'X-OpenAI-Orbit-Selection', 'Accept-Encoding']:
                    self.assertEqual(sent['headers'][name], handler.headers[name])
                self.assertEqual(handler.wfile.getvalue(), b'original response\x00')
                handler.send_header.assert_any_call('Content-Encoding', 'gzip')
                handler.send_header.assert_any_call('Set-Cookie', 'native=value; Secure')
                self.assertTrue(callable(getattr(handler, 'do_' + method)))

    def test_primary_room_attachments_and_profile_mutations_pass_unchanged(self):
        body = json.dumps({'content': {'attachments': [{'file_id': 'main-file'}]}}).encode()
        for path in ['/backend-api/messaging/rooms/main-room/messages', '/backend-api/tbo/main-dot/customisation']:
            handler = self.handler(path, body=body); connection = self.connection()
            with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
            handler.send_response.assert_called_once_with(200)
            self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer native-desktop')
            self.assertEqual(connection.sent_body, body)

    def test_secondary_file_uploads_keep_binary_body_and_use_room_owner(self):
        body = b'--cq-boundary\r\nContent-Disposition: form-data; name="file"; filename="proof.txt"\r\n\r\n' + b'x' * (1024 * 1024 + 1) + b'\r\n--cq-boundary--\r\n'
        for suffix in ['files', 'files/uploads/upload-1/complete']:
            handler = self.handler('/backend-api/messaging/rooms/worker-room/' + suffix, body=body)
            handler.headers['Content-Type'] = 'multipart/form-data; boundary=cq-boundary'
            connection = self.connection(b'{"id":"attachment-1","status":"ready"}')
            with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
            handler.send_response.assert_called_once_with(200)
            self.assertEqual(connection.sent_body, body)
            headers = connection.request.call_args.kwargs['headers']
            self.assertEqual(headers['Authorization'], 'Bearer stored-worker')
            self.assertEqual(headers['ChatGPT-Account-Id'], 'worker')
            self.assertNotIn('Cookie', headers)
            self.assertNotIn('X-OpenAI-Orbit-Selection', headers)

    def test_secondary_upload_reservation_uses_room_owner(self):
        body = b'{"name":"proof.txt","size_bytes":10,"mime_type":"text/plain"}'
        handler = self.handler('/backend-api/messaging/rooms/worker-room/files/uploads', body=body)
        handler.headers['Content-Type'] = 'application/json'
        connection = self.connection(b'{"id":"upload-1"}')
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(connection.sent_body, body)
        self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')

    def test_secondary_attachment_only_send_preserves_ids_and_owner_challenge(self):
        body = json.dumps({'content': {'text': '', 'attachments': [{'type': 'file', 'file_id': 'attachment-1'}]},
                           'request_id': 'request-1', 'idempotency_token': 'request-1',
                           'app_attest_challenge': 'native-challenge'}).encode()
        handler = self.handler('/backend-api/messaging/rooms/worker-room/messages', body=body)
        connection = self.connection(b'{"id":"message-1","account_user_id":"worker-viewer","content":{"text":""}}')
        probe.remember_challenge(b'{"attestation_challenge":"native-challenge"}',
                                 b'{"attestation_challenge":"worker-challenge"}', self.worker['alias'])
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        sent = json.loads(connection.sent_body)
        self.assertEqual(sent['content'], json.loads(body)['content'])
        self.assertEqual(sent['app_attest_challenge'], 'worker-challenge')
        self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')

    def test_secondary_upload_limit_and_unrelated_operations_stay_blocked(self):
        path = '/backend-api/messaging/rooms/worker-room/files'
        handler = self.handler(path, body=b'')
        handler.headers.update({'Content-Type': 'multipart/form-data; boundary=cq',
                               'Content-Length': str(probe.NATIVE_UPLOAD_LIMIT + 1)})
        with patch('probe.http.client.HTTPSConnection') as connect: handler.handle_request()
        handler.send_response.assert_called_once_with(413)
        connect.assert_not_called()
        for suffix in ['files/delete', 'files/uploads/upload-1/cancel']:
            handler = self.handler('/backend-api/messaging/rooms/worker-room/' + suffix)
            handler.headers['Content-Type'] = 'multipart/form-data; boundary=cq'
            with patch('probe.http.client.HTTPSConnection') as connect: handler.handle_request()
            handler.send_response.assert_called_once_with(503)
            connect.assert_not_called()
        handler = self.handler(path, body=b'file')
        handler.headers['Content-Type'] = 'multipart/form-data; boundary=cq'
        with patch.object(probe.NATIVE_GATE, 'require', side_effect=ValueError('unauthorised')), \
                patch('probe.http.client.HTTPSConnection') as connect: handler.handle_request()
        handler.send_response.assert_called_once_with(401)
        connect.assert_not_called()

    def test_secondary_file_metadata_pins_streamed_download_to_owner(self):
        url = 'https://chatgpt.com/backend-api/files/download/file_worker?token=opaque'
        handler = self.handler('/backend-api/messaging/rooms/worker-room/files/attachment-1', 'GET', b'')
        connection = self.connection(json.dumps({'id': 'attachment-1', 'download_url': url}).encode())
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        download = self.handler('/backend-api/files/download/file_worker?token=opaque', 'GET', b'')
        stream = self.connection(b'binary-file\x00', headers=[('Content-Type', 'application/octet-stream')])
        with patch('probe.http.client.HTTPSConnection', return_value=stream): download.handle_request()
        download.send_response.assert_called_once_with(200)
        stream.getresponse.return_value.read.assert_not_called()
        self.assertEqual(download.wfile.getvalue(), b'binary-file\x00')
        self.assertEqual(stream.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')
        self.assertNotIn('Cookie', stream.request.call_args.kwargs['headers'])
        self.assertIs(probe.download_owner('/backend-api/files/download/file_worker?inline=true', self.fixture), self.worker)

    def test_secondary_cdn_metadata_binds_native_file_lookup_without_rewriting_signed_url(self):
        value = {'id': 'attachment-1', 'file_id': 'attachment-1', 'asset_pointer': 'sediment://file_worker',
                 'download_url': 'https://files.oaiusercontent.com/synthetic?signature=opaque'}
        handler = self.handler('/backend-api/messaging/rooms/worker-room/files/attachment-1', 'GET', b'')
        connection = self.connection(json.dumps(value).encode())
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(json.loads(handler.wfile.getvalue()), value)
        self.assertIs(probe.download_owner('/backend-api/files/download/file_worker?inline=true', self.fixture), self.worker)
        self.assertNotIn('signature', repr(probe.FILE_OWNERS))

    def test_download_grants_expire_and_reject_wrong_origin_or_owner(self):
        path = '/backend-api/files/download/file_worker'
        url = 'https://chatgpt.com' + path
        probe.remember_download(url, self.worker, now=100)
        self.assertIs(probe.download_owner(path, self.fixture, now=101), self.worker)
        self.assertIsNone(probe.download_owner(path, self.fixture, now=1600))
        for value in ['https://other.example' + path, 'http://chatgpt.com' + path,
                      'https://chatgpt.com/backend-api/other', 'https://chatgpt.com:444' + path]:
            probe.remember_download(value, self.worker, now=100)
        self.assertFalse(probe.FILE_OWNERS)
        probe.remember_download(url, self.worker, now=100)
        with self.assertRaises(ValueError): probe.remember_download(url, self.main, now=101)
        changed = dict(self.worker, account_id='another-account')
        fixture = (self.main, self.profiles, {**self.owners, 'worker-dot': changed}, [])
        self.assertIsNone(probe.download_owner(path, fixture, now=102))

    def test_primary_reboot_does_not_use_research_gate(self):
        handler = self.handler('/backend-api/cloud-aeons/primary/reboot', body=b'')
        connection = self.connection(status=202)
        with patch('probe.http.client.HTTPSConnection', return_value=connection), patch.object(handler, 'reboot_probe') as research:
            handler.handle_request()
        research.assert_not_called(); handler.send_response.assert_called_once_with(202)
        self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer native-desktop')

    def test_secondary_activity_stream_is_not_buffered_until_eof(self):
        handler = self.handler('/backend-api/tbo/worker-dot/activity/stream', 'GET', b'')
        frames = b'event: ready\ndata: {}\n\n'
        connection = self.connection(frames, headers=[('Content-Type', 'text/event-stream'),
                                                      ('Set-Cookie', 'owner-cookie')])
        with patch('probe.http.client.HTTPSConnection', return_value=connection):
            handler.handle_request()
        connection.getresponse.return_value.read.assert_not_called()
        self.assertEqual(handler.wfile.getvalue(), frames)
        self.assertTrue(handler.close_connection)
        self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')
        self.assertFalse(any(call.args[0].lower() == 'set-cookie' for call in handler.send_header.call_args_list))

    def test_secondary_computer_controls_translate_only_managed_owner_environment(self):
        row = dict(profile_id='worker-dot', environment_id='worker-environment',
                   primary_environment_id='main-environment')
        for operation in ['connect', 'connect-and-replace', 'disconnect']:
            path = f'/backend-api/tbo/worker-dot/computers/main-environment/{operation}?expected_thread_id=worker-root'
            handler = self.handler(path, body=b''); connection = self.connection(b'', status=204)
            with patch('secondary_executors.mapping', return_value=row), \
                 patch('probe.http.client.HTTPSConnection', return_value=connection):
                handler.handle_request()
            handler.send_response.assert_called_once_with(204)
            self.assertEqual(connection.request.call_args.args[1], path.replace('/main-environment/', '/worker-environment/'))
            self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')
        with patch('secondary_executors.mapping', return_value=row):
            for path, body in [('/backend-api/tbo/worker-dot/computers/foreign/connect?expected_thread_id=worker-root', b''),
                               ('/backend-api/tbo/worker-dot/computers/main-environment/connect?expected_thread_id=foreign', b''),
                               ('/backend-api/tbo/worker-dot/computers/main-environment/connect?expected_thread_id=worker-root', b'{}')]:
                self.assertIsNone(probe.local_computer_target(path, body, self.fixture))

    def test_secondary_old_computer_can_be_revoked_without_managed_worker(self):
        path='/backend-api/tbo/worker-dot/computers/old-research/disconnect?expected_thread_id=worker-root'
        row={'kind':'user','environment_id':'old-research','can_disconnect':True}
        handler=self.handler(path,body=b'');connection=self.connection(b'',status=204)
        with patch('secondary_executors.mapping',return_value=None), \
             patch('probe.catalog.get',return_value={'computers':[row]}) as lookup, \
             patch('probe.http.client.HTTPSConnection',return_value=connection):
            handler.handle_request()
        lookup.assert_called_once_with(self.worker, '/backend-api/tbo/worker-dot/computers')
        handler.send_response.assert_called_once_with(204)
        self.assertEqual(connection.request.call_args.args[1],path)
        self.assertEqual(connection.request.call_args.kwargs['headers']['Authorization'],'Bearer stored-worker')
        for computer in [dict(row,kind='openai_cloud'),dict(row,can_disconnect=False),dict(row,environment_id='foreign')]:
            with patch('secondary_executors.mapping',return_value=None), \
                 patch('probe.catalog.get',return_value={'computers':[computer]}):
                self.assertIsNone(probe.local_computer_target(path,b'',self.fixture))

    def test_secondary_name_edit_uses_own_account(self):
        body = json.dumps({'display_name': 'Same name'}).encode()
        handler = self.handler('/backend-api/tbo/worker-dot', 'PATCH', body)
        connection = self.connection(b'{"id":"worker-dot","display_name":"Same name"}')
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(connection.sent_body, body)
        headers = connection.request.call_args.kwargs['headers']
        self.assertEqual(headers['Authorization'], 'Bearer stored-worker')
        self.assertEqual(headers['ChatGPT-Account-Id'], 'worker')
        self.assertNotIn('Cookie', headers)
        self.assertNotIn('X-OpenAI-Orbit-Selection', headers)

    def test_secondary_name_edit_rejects_other_writes(self):
        for path, value in [
                ('/backend-api/tbo/worker-dot', {}),
                ('/backend-api/tbo/worker-dot', {'display_name': ''}),
                ('/backend-api/tbo/worker-dot', {'display_name': 'x' * 25}),
                ('/backend-api/tbo/worker-dot', {'display_name': 'bad\\name'}),
                ('/backend-api/tbo/worker-dot', {'display_name': 'bad\nname'}),
                ('/backend-api/tbo/worker-dot', {'display_name': 42}),
                ('/backend-api/tbo/worker-dot', {'display_name': 'Name', 'status': 'paused'}),
                ('/backend-api/tbo/worker-dot?other=1', {'display_name': 'Name'}),
                ('/backend-api/tbo/worker-dot/other', {'display_name': 'Name'})]:
            with self.subTest(path=path, value=value):
                handler = self.handler(path, 'PATCH', json.dumps(value).encode())
                with patch('probe.http.client.HTTPSConnection') as connect: handler.handle_request()
                handler.send_response.assert_called_once_with(503)
                connect.assert_not_called()

    def test_primary_requests_work_when_dot_gate_is_unavailable(self):
        self.gate.side_effect = ValueError('No available primary Dot')
        for path in ['/backend-api/ordinary/resource', '/backend-api/tbo/primary']:
            handler = self.handler(path, 'GET', b'')
            connection = self.connection(b'{"selection":{"available":false}}')
            with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
            handler.send_response.assert_called_once_with(200)
            self.assertEqual(handler.wfile.getvalue(), b'{"selection":{"available":false}}')
        self.gate.assert_not_called()

    def rotated_fixture(self, owner):
        index = 0 if owner == self.main else 1
        profiles = [dict(profile) for profile in self.profiles]
        profile = profiles[index]
        profile['active_root_thread_id'] = owner['account_id'] + '-rebooted-root'
        profile['messaging_room_id'] = owner['account_id'] + '-rebooted-room'
        owners = {key: account for key, account in self.owners.items() if account != owner}
        owners.update({profile[key]: owner for key in ['id', 'active_root_thread_id', 'messaging_room_id']})
        return (self.main, profiles, owners, [])

    def assert_root_snapshot(self, fixture):
        snapshot = self.root / 'native-roots.json'
        self.assertEqual(snapshot.stat().st_mode & 0o777, 0o600)
        value = json.loads(snapshot.read_text())
        expected = {'version': 1, 'roots': [
            {'owner_alias': fixture[2][profile['id']]['alias'], 'profile_id': profile['id'],
             'root_thread_id': profile['active_root_thread_id']} for profile in fixture[1]]}
        self.assertEqual(value, expected)
        self.assertNotIn('stored-', snapshot.read_text())

    def test_native_catalog_refresh_publishes_rotated_verified_secondary_root(self):
        fresh = self.rotated_fixture(self.worker)
        connection = self.connection(json.dumps({'items': [self.profiles[0]], 'cursor': None}).encode())
        handler = self.handler('/backend-api/tbo?limit=25', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=connection), \
                patch('probe.catalog.fixture', return_value=fresh) as refresh:
            handler.handle_request()
        refresh.assert_called_once()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(json.loads(handler.wfile.getvalue())['items'], fresh[1])
        self.assertEqual(probe.CATALOG_FIXTURE, fresh)
        self.assert_root_snapshot(fresh)
        self.assertNotIn('worker-root', probe.CATALOG_FIXTURE[2])
        read = self.handler('/backend-api/tbo/by-thread/worker-rebooted-root', 'GET', b'')
        owner_connection = self.connection(b'{}')
        with patch('probe.http.client.HTTPSConnection', return_value=owner_connection): read.handle_request()
        self.assertEqual(owner_connection.request.call_args.kwargs['headers']['Authorization'], 'Bearer stored-worker')

    def test_native_catalog_refresh_rejects_changed_owner_without_publishing(self):
        fresh = self.rotated_fixture(self.worker)
        fresh[2]['worker-rebooted-root'] = self.main
        handler = self.handler('/backend-api/tbo', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(b'{"items":[],"cursor":null}')), \
                patch('probe.catalog.fixture', return_value=fresh):
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), b'{"items":[],"cursor":null}')
        self.assertIs(probe.CATALOG_FIXTURE, self.fixture)
        self.assertFalse((self.root / 'native-roots.json').exists())

    def test_inflight_catalog_refresh_does_not_reset_newer_primary_rotation(self):
        fresh = self.rotated_fixture(self.main)
        primary = {'selection': {'available': True, 'thread_id': 'main-rebooted-root'}, 'profile': fresh[1][0]}
        def refresh(*args):
            probe.observe_native_primary(dict(handler.headers), json.dumps(primary).encode())
            return self.fixture
        handler = self.handler('/backend-api/tbo', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(b'{"items":[],"cursor":null}')), \
                patch('probe.catalog.fixture', side_effect=refresh):
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(json.loads(handler.wfile.getvalue())['items'], fresh[1])
        self.assert_root_snapshot(fresh)

    def test_native_primary_owner_collision_preserves_original_success_without_publishing(self):
        data = json.dumps({'selection': {'available': True, 'thread_id': 'worker-root'},
                           'profile': {'id': 'main-dot', 'active_root_thread_id': 'worker-root'}}).encode()
        handler = self.handler('/backend-api/tbo/primary', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), data)
        self.assertIs(probe.CATALOG_FIXTURE, self.fixture)
        self.assertFalse((self.root / 'native-roots.json').exists())

    def test_native_primary_rotation_preserves_other_owner_and_original_response(self):
        fresh = self.rotated_fixture(self.main)
        primary = {'selection': {'available': True, 'thread_id': 'main-rebooted-root'}, 'profile': fresh[1][0]}
        data = json.dumps(primary, indent=2).encode()
        handler = self.handler('/backend-api/tbo/primary', 'GET', b'')
        connection = self.connection(data, headers=[('Content-Type', 'application/json')])
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), data)
        self.assertEqual(connection.request.call_args.kwargs['headers']['Accept-Encoding'], 'identity')
        self.assertEqual(probe.CATALOG_FIXTURE[1][1], self.profiles[1])
        self.assertEqual(probe.CATALOG_FIXTURE[2]['worker-root'], self.worker)
        self.assertNotIn('main-root', probe.CATALOG_FIXTURE[2])
        self.assert_root_snapshot(fresh)

    def test_native_primary_root_is_published_before_renderer_sees_response(self):
        fresh = self.rotated_fixture(self.main)
        data = json.dumps({'selection': {'available': True, 'thread_id': 'main-rebooted-root'},
                           'profile': fresh[1][0]}).encode()
        handler = self.handler('/backend-api/tbo/primary', 'GET', b'')
        def response_started(status):
            self.assertEqual(status, 200)
            self.assertEqual(probe.CATALOG_FIXTURE[1][0]['active_root_thread_id'], 'main-rebooted-root')
            self.assert_root_snapshot(fresh)
            self.assertEqual(handler.wfile.getvalue(), b'')
        handler.send_response.side_effect = response_started
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), data)

    def test_oversized_primary_metadata_streams_unchanged_without_publishing(self):
        data = b'x' * (1024 * 1024 + 128)
        stream = io.BytesIO(data)
        connection = self.connection()
        connection.getresponse.return_value.read1.side_effect = stream.read
        handler = self.handler('/backend-api/tbo/primary', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), data)
        self.assertIs(probe.CATALOG_FIXTURE, self.fixture)
        self.gate.assert_not_called()
        self.assertFalse((self.root / 'native-roots.json').exists())

    def test_native_catalog_offline_secondary_uses_only_matching_verified_cache(self):
        data = json.dumps({'items': [self.profiles[0]], 'cursor': None}).encode()
        handler = self.handler('/backend-api/tbo', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)), \
                patch('probe.catalog.fixture', side_effect=ValueError('Secondary offline')):
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(json.loads(handler.wfile.getvalue())['items'], self.profiles)
        self.assertIs(probe.CATALOG_FIXTURE, self.fixture)

    def test_native_catalog_offline_secondary_keeps_primary_without_valid_cache(self):
        rotated = self.rotated_fixture(self.main)[1][0]
        invalid_owners = {**self.owners, 'worker-root': self.main}
        for fixture, main in [(None, self.profiles[0]), (self.fixture, rotated),
                              ((self.main, self.profiles, invalid_owners, []), self.profiles[0])]:
            with self.subTest(cache_present=fixture is not None, primary_root=main['active_root_thread_id']):
                data = json.dumps({'items': [main], 'cursor': None}).encode()
                handler = self.handler('/backend-api/tbo', 'GET', b'')
                with patch.object(probe, 'CATALOG_FIXTURE', fixture), \
                        patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)), \
                        patch('probe.catalog.fixture', side_effect=ValueError('Secondary offline')):
                    handler.handle_request()
                handler.send_response.assert_called_once_with(200)
                self.assertEqual(handler.wfile.getvalue(), data)

    def test_native_catalog_deadline_keeps_primary_and_does_not_publish_late_fixture(self):
        release = threading.Event(); finished = threading.Event()
        fresh = self.rotated_fixture(self.worker)
        data = json.dumps({'items': [self.profiles[0]], 'cursor': None}).encode()
        def slow(*args):
            release.wait(2); finished.set()
            return fresh
        handler = self.handler('/backend-api/tbo', 'GET', b'')
        try:
            with patch.object(probe, 'NATIVE_OWNER_TIMEOUT', 0.03), \
                    patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)), \
                    patch('probe.catalog.fixture', side_effect=slow):
                started = time.monotonic(); handler.handle_request()
                self.assertLess(time.monotonic() - started, 0.5)
                handler.send_response.assert_called_once_with(200)
                self.assertEqual(json.loads(handler.wfile.getvalue())['items'], self.profiles)
                release.set(); self.assertTrue(finished.wait(1))
                self.assertIs(probe.CATALOG_FIXTURE, self.fixture)
                self.assertFalse((self.root / 'native-roots.json').exists())
        finally:
            release.set()

    def test_native_primary_observation_failure_does_not_fail_original_success(self):
        self.gate.side_effect = ValueError('Native identity unavailable')
        data = json.dumps({'selection': {'available': True, 'thread_id': 'other-root'},
                           'profile': {'id': 'main-dot', 'active_root_thread_id': 'other-root'}}).encode()
        handler = self.handler('/backend-api/tbo/primary', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), data)
        self.assertIs(probe.CATALOG_FIXTURE, self.fixture)
        self.assertFalse((self.root / 'native-roots.json').exists())

    def test_native_celsius_bootstrap_defers_secondary_lookup_and_pins_owner(self):
        data = b'{"websocket_url":"wss://ws.chatgpt.com/primary"}'
        handler = self.handler('/backend-api/celsius/ws/user', 'GET', b'')
        broker = MagicMock(); broker.issue.return_value = 'wss://ws.chatgpt.com/cq-owner-events/test'
        with patch.object(probe, 'CELSIUS', broker), \
                patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)), \
                patch('probe.catalog.get', return_value={'websocket_url': 'wss://ws.chatgpt.com/worker'}) as get:
            handler.handle_request()
            handler.send_response.assert_called_once_with(200)
            self.assertEqual(json.loads(handler.wfile.getvalue())['websocket_url'], broker.issue.return_value)
            get.assert_not_called()
            self.assertEqual(broker.issue.call_args.args, ({self.main['alias']: 'wss://ws.chatgpt.com/primary'}, self.owners))
            resolve = broker.issue.call_args.kwargs['resolve_url']
            self.assertEqual(resolve(self.worker['alias']), 'wss://ws.chatgpt.com/worker')
            account, path, headers = get.call_args.args
            self.assertEqual(account, self.worker)
            self.assertEqual(path, handler.path)
            self.assertEqual(headers['Authorization'], 'Bearer stored-worker')
            self.assertEqual(headers['ChatGPT-Account-Id'], 'worker')
            self.assertNotIn('Cookie', headers)
            with self.assertRaises(KeyError): resolve('unconfigured')

    def test_native_celsius_never_returns_misleading_primary_only_session(self):
        data = b'{"websocket_url":"wss://ws.chatgpt.com/primary"}'
        for fixture in [None, self.fixture]:
            handler = self.handler('/backend-api/celsius/ws/user', 'GET', b'')
            broker = MagicMock(); broker.issue.side_effect = ValueError('Broker unavailable')
            with patch.object(probe, 'CELSIUS', broker), patch.object(probe, 'CATALOG_FIXTURE', fixture), \
                    patch('probe.http.client.HTTPSConnection', return_value=self.connection(data)), \
                    patch('probe.catalog.get') as get:
                handler.handle_request()
            handler.send_response.assert_called_once_with(503)
            handler.send_header.assert_any_call('Retry-After', '1')
            self.assertEqual(handler.wfile.getvalue(), b'')
            get.assert_not_called()

    def test_native_challenge_deadline_does_not_publish_late_challenge(self):
        release = threading.Event(); finished = threading.Event()
        primary = b'{"attestation_challenge":"main-challenge","expire_after":540}'
        secondary = b'{"attestation_challenge":"worker-challenge","expire_after":540}'
        def slow(*args):
            release.wait(2); finished.set()
            return 200, secondary
        handler = self.handler('/backend-api/ios/attestation_challenge', 'GET', b'')
        handler.headers['X-Sentinel-DC'] = 'native-device-proof'
        try:
            with patch.object(probe, 'NATIVE_OWNER_TIMEOUT', 0.03), \
                    patch('probe.http.client.HTTPSConnection', return_value=self.connection(primary)), \
                    patch('probe.native_owner_challenge', side_effect=slow):
                started = time.monotonic(); handler.handle_request()
                self.assertLess(time.monotonic() - started, 0.5)
                handler.send_response.assert_called_once_with(200)
                self.assertEqual(handler.wfile.getvalue(), primary)
                release.set(); self.assertTrue(finished.wait(1))
                self.assertEqual(probe.CHALLENGES, {})
        finally:
            release.set()

    def test_native_challenges_are_collected_concurrently_and_only_success_is_published(self):
        started = threading.Barrier(3)
        extra = dict(alias='33333333', account_id='extra', token='stored-extra')
        main_data = b'{"attestation_challenge":"main-challenge"}'
        worker_data = b'{"attestation_challenge":"worker-challenge"}'
        def challenge(account, *args):
            started.wait(1)
            return (200, worker_data) if account == self.worker else (403, b'{}')
        handler = self.handler('/backend-api/ios/attestation_challenge', 'GET', b'')
        handler.headers['X-Sentinel-DC'] = 'native-device-proof'
        observer = threading.Thread(target=lambda: started.wait(1))
        observer.start()
        with patch('probe.known_owners', return_value={self.main['alias']: self.main, self.worker['alias']: self.worker, extra['alias']: extra}), \
                patch('probe.http.client.HTTPSConnection', return_value=self.connection(main_data)), \
                patch('probe.native_owner_challenge', side_effect=challenge):
            handler.handle_request()
        observer.join(1)
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(set(probe.CHALLENGES), {('main-challenge', self.worker['alias'])})

    def test_secondary_and_aggregate_requests_keep_native_gate(self):
        self.gate.side_effect = ValueError('Native account mismatch')
        for path in ['/backend-api/tbo/worker-dot', '/backend-api/tbo',
                     '/backend-api/celsius/ws/user', '/backend-api/ios/attestation_challenge',
                     '/backend-api/wham/statsig/bootstrap']:
            handler = self.handler(path, 'GET', b'')
            with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
            connection.assert_not_called(); handler.send_response.assert_called_once_with(401)

    def test_primary_executor_registration_never_reuses_research_executor(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(probe, 'ROOT', pathlib.Path(directory)):
            (pathlib.Path(directory) / 'native-secondary-executor.enabled').touch()
            handler = self.handler('/backend-api/flora/cca/executor'); connection = self.connection()
            with patch('probe.http.client.HTTPSConnection', return_value=connection), patch('native_executor_reuse.reuse') as research:
                handler.handle_request()
        research.assert_not_called(); handler.send_response.assert_called_once_with(200)
        self.assertEqual(connection.sent_body, b'unchanged\x00body')

    def test_unknown_secondary_mutations_never_connect(self):
        for path, method in [('/backend-api/tbo/worker-dot/customisation', 'PATCH'),
                             ('/backend-api/tbo/by-thread/worker-root/delete', 'DELETE'),
                             ('/backend-api/messaging/rooms/worker-room/files', 'POST'),
                             ('/backend-api/messaging/rooms/worker-room/messages', 'POST')]:
            handler = self.handler(path, method)
            with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
            connection.assert_not_called(); handler.send_response.assert_called_once_with(503)

    def test_secondary_read_routes_only_current_verified_owner(self):
        handler = self.handler('/backend-api/tbo/worker-dot', 'GET', b''); connection = self.connection(b'{}')
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        headers = connection.request.call_args.kwargs['headers']
        self.assertEqual(headers['Authorization'], 'Bearer stored-worker')
        self.assertEqual(headers['ChatGPT-Account-Id'], 'worker')
        self.assertNotIn('Cookie', headers); self.assertNotIn('X-OpenAI-Orbit-Selection', headers)

    def test_conflicting_secondary_catalog_owner_fails_before_network(self):
        probe.CATALOG_FIXTURE = (self.main, self.profiles, {**self.owners, 'worker-root': self.main}, [])
        handler = self.handler('/backend-api/tbo/worker-dot', 'GET', b'')
        with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
        connection.assert_not_called(); handler.send_response.assert_called_once_with(409)

    def test_native_primary_sse_stream_is_forwarded_without_buffering(self):
        handler = self.handler('/backend-api/conversation')
        chunks = [b'data: first\n\n', b'data: done\n\n']
        connection = self.connection(headers=[('Content-Type', 'text/event-stream'), ('X-Upstream', 'original')])
        connection.getresponse.return_value.read1.side_effect = [*chunks, b'']
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), b''.join(chunks))
        connection.getresponse.return_value.read.assert_not_called()
        self.assertTrue(handler.close_connection)

    def test_native_primary_sse_exceeds_research_response_limit(self):
        handler = self.handler('/backend-api/conversation')
        connection = self.connection(headers=[('Content-Type', 'text/event-stream')])
        chunks = [b'x' * 65536] * 257
        connection.getresponse.return_value.read1.side_effect = [*chunks, b'']
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), b''.join(chunks))
        self.assertTrue(handler.close_connection)

    def test_head_keeps_upstream_length_without_writing_response_body(self):
        handler = self.handler('/backend-api/ordinary/resource', 'HEAD', b'')
        connection = self.connection(b'', headers=[('Content-Length', '987')])
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(handler.wfile.getvalue(), b'')
        handler.send_header.assert_any_call('Content-Length', '987')
        self.assertTrue(callable(handler.do_HEAD))

    def test_primary_upload_and_download_stream_past_research_limits(self):
        upload = b'upload chunk' * 100000
        download = b'download chunk' * 1300000
        handler = self.handler('/backend-api/files/upload', body=upload)
        handler.rfile = MagicMock(wraps=io.BytesIO(upload))
        connection = self.connection()
        stream = io.BytesIO(download)
        connection.getresponse.return_value.read1.side_effect = stream.read
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(connection.sent_body, upload)
        self.assertEqual(handler.wfile.getvalue(), download)
        self.assertTrue(all(0 < call.args[0] <= 65536 for call in handler.rfile.read.call_args_list))
        connection.getresponse.return_value.read.assert_not_called()

    def test_large_native_transfer_over_real_http_preserves_payloads_and_cookie(self):
        upload = b'large native upload' * 60000
        download_chunk = b'd' * 65536
        download_chunks = 257
        seen = {}
        class Upstream(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_POST(self):
                remaining = int(self.headers['Content-Length'])
                digest = hashlib.sha256()
                seen.update(length=remaining, authorization=self.headers['Authorization'])
                while remaining:
                    chunk = self.rfile.read(min(remaining, 65536))
                    if not chunk: return
                    remaining -= len(chunk); digest.update(chunk)
                seen['digest'] = digest.digest()
                self.send_response(200)
                self.send_header('Content-Length', str(len(download_chunk) * download_chunks))
                self.send_header('Set-Cookie', 'native=value; Domain=chatgpt.com; Secure; HttpOnly')
                self.end_headers()
                for _ in range(download_chunks): self.wfile.write(download_chunk)
        upstream = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        proxy = http.server.ThreadingHTTPServer(('127.0.0.1', 0), probe.Handler)
        threads = [threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.01}, daemon=True)
                   for server in [upstream, proxy]]
        for thread in threads: thread.start()
        client = http.client.HTTPConnection('127.0.0.1', proxy.server_port, timeout=10)
        try:
            with patch('probe.http.client.HTTPSConnection', side_effect=lambda *_args, **kwargs:
                       http.client.HTTPConnection('127.0.0.1', upstream.server_port, **kwargs)):
                client.request('POST', '/backend-api/files/upload', body=upload,
                               headers={'Authorization': 'Bearer native-desktop', 'ChatGPT-Account-Id': 'main'})
                response = client.getresponse()
                self.assertEqual(response.status, 200)
                self.assertEqual(response.getheader('Set-Cookie'), 'native=value; Secure; HttpOnly')
                digest = hashlib.sha256(); received = 0
                while chunk := response.read(65536):
                    received += len(chunk); digest.update(chunk)
            self.assertEqual(seen, dict(length=len(upload), authorization='Bearer native-desktop',
                                        digest=hashlib.sha256(upload).digest()))
            self.assertEqual(received, len(download_chunk) * download_chunks)
            self.assertEqual(digest.digest(), hashlib.sha256(download_chunk * download_chunks).digest())
        finally:
            client.close()
            for server in [proxy, upstream]: server.shutdown(); server.server_close()
            for thread in threads: thread.join(timeout=2)

    def test_native_chunked_upload_streams_decoded_body_once(self):
        handler = self.handler('/backend-api/files/upload', body=b'3\r\none\r\n3;test=value\r\ntwo\r\n0\r\n\r\n')
        del handler.headers['Content-Length']; handler.headers['Transfer-Encoding'] = 'chunked'
        connection = self.connection()
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(connection.sent_body, b'onetwo')
        self.assertTrue(connection.request.call_args.kwargs['encode_chunked'])
        self.assertEqual(connection.request.call_count, 1)

    def test_ambiguous_or_invalid_native_upload_framing_is_rejected(self):
        for transfer in ['chunked', 'gzip']:
            handler = self.handler('/backend-api/files/upload')
            handler.headers['Transfer-Encoding'] = transfer
            with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
            connection.assert_not_called(); handler.send_response.assert_called_once_with(400)
        handler = self.handler('/backend-api/files/upload', body=b'3\r\nab')
        del handler.headers['Content-Length']; handler.headers['Transfer-Encoding'] = 'chunked'
        connection = self.connection()
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(502)
        self.assertEqual(connection.request.call_count, 1)

    def test_inspected_and_secondary_payloads_keep_research_limits(self):
        for path in ['/backend-api/wham/statsig/bootstrap', '/backend-api/messaging/rooms/worker-room/messages']:
            handler = self.handler(path, body=b'x' * (1024 * 1024 + 1))
            with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
            connection.assert_not_called(); handler.send_response.assert_called_once_with(413)
        handler = self.handler('/backend-api/tbo/worker-dot', 'GET', b'')
        connection = self.connection(b'x' * (16 * 1024 * 1024 + 1))
        with patch('probe.http.client.HTTPSConnection', return_value=connection): handler.handle_request()
        handler.send_response.assert_called_once_with(502)


if __name__ == '__main__': unittest.main()
