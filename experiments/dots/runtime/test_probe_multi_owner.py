import test_support
import copy
import io
import hashlib
import time
import json
import pathlib
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import probe


class MultiOwnerProbeTests(unittest.TestCase):
    def setUp(self):
        self.accounts = [dict(alias=f'{index:08x}', account_id=f'account-{index}',
                              token=f'test-token-{index}', viewer_ids={f'viewer-{index}'})
                         for index in range(1, 5)]
        self.profiles = [dict(id=f'dot-{index}', active_root_thread_id=f'root-{index}',
                              messaging_room_id=f'room-{index}', display_name='Same name')
                         for index in range(1, 5)]
        self.owners = {profile[key]: owner for profile, owner in zip(self.profiles, self.accounts)
                       for key in ['id', 'active_root_thread_id', 'messaging_room_id']}
        self.fixture = (self.accounts[0], self.profiles, self.owners, [])
        self.targets = {f'handle-{index}': (f'dot-{index}', f'root-{index}') for index in range(1, 5)}
        self.aliases = {f'handle-{index}': account['alias'] for index, account in enumerate(self.accounts, 1)}
        for name, value in [('DOTS', self.targets), ('DOT_OWNERS', self.aliases),
                            ('SYSTEM_ALIAS', self.accounts[0]['alias'])]:
            mock = patch.object(probe.settings, name, value)
            mock.start(); self.addCleanup(mock.stop)
        for target, value in [('CATALOG_FIXTURE', self.fixture), ('STATE', dict(probe.STATE)),
                              ('event', MagicMock())]:
            mock = patch.object(probe, target, value)
            mock.start(); self.addCleanup(mock.stop)
        gate = patch.object(probe.NATIVE_GATE, 'require')
        gate.start(); self.addCleanup(gate.stop)
        probe.CHALLENGES.clear()
        self.addCleanup(probe.CHALLENGES.clear)

    def handler(self, path, method='GET', body=b''):
        handler = probe.Handler.__new__(probe.Handler)
        handler.path = path; handler.command = method
        handler.headers = {'Authorization': 'Bearer native-token', 'ChatGPT-Account-Id': 'account-1',
                           'X-Sentinel-DC': 'native-proof', 'Cookie': 'system-cookie',
                           'X-OpenAI-Orbit-Selection': 'system-selection', 'Content-Length': str(len(body))}
        handler.connection = MagicMock(); handler.rfile = io.BytesIO(body); handler.wfile = io.BytesIO()
        handler.send_response = MagicMock(); handler.send_header = MagicMock(); handler.end_headers = MagicMock()
        return handler

    def connection(self, value, status=200):
        connection = MagicMock(); response = connection.getresponse.return_value
        response.status = status; response.read.return_value = json.dumps(value).encode()
        response.getheaders.return_value = []
        return connection

    def test_duplicate_names_and_single_owner_catalogs_are_valid(self):
        probe.validate_profiles(self.fixture)
        self.assertEqual(set(probe.known_owners(self.fixture)), set(self.aliases.values()))
        with patch.object(probe.settings, 'DOTS', {'handle-1': self.targets['handle-1']}), patch.object(probe.settings, 'DOT_OWNERS', {'handle-1': self.aliases['handle-1']}):
            fixture = (self.accounts[0], self.profiles[:1], {key: self.accounts[0] for key in ['dot-1', 'root-1', 'room-1']}, [])
            probe.validate_profiles(fixture)
            self.assertEqual(probe.known_owners(fixture), {self.accounts[0]['alias']: self.accounts[0]})

    def test_catalog_ids_and_current_root_ownership_required(self):
        profiles = copy.deepcopy(self.profiles)
        profiles[3]['id'] = 'unconfigured'
        with self.assertRaises(ValueError): probe.validate_profiles((self.accounts[0], profiles, self.owners, []))
        for key in ['dot-4', 'root-4']:
            owners = {**self.owners, key: self.accounts[1]}
            with self.assertRaises(ValueError): probe.validate_profiles((self.accounts[0], self.profiles, owners, []))
        profiles = copy.deepcopy(self.profiles); profiles[3]['active_root_thread_id'] = 'new-root-4'
        probe.validate_profiles((self.accounts[0], profiles, {**self.owners, 'new-root-4': self.accounts[3]}, []))

    def test_catalog_handler_ignores_display_name_uniqueness(self):
        handler = self.handler('/backend-api/tbo')
        with patch('probe.http.client.HTTPSConnection', return_value=self.connection({'items': [], 'cursor': None})):
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(json.loads(handler.wfile.getvalue())['items'], self.profiles)

    def test_all_owner_challenges_fanned_out_with_native_proof(self):
        connections = [self.connection({'attestation_challenge': f'challenge-{index}', 'expire_after': 30})
                       for index in range(1, 5)]
        handler = self.handler('/backend-api/ios/attestation_challenge')
        with patch('probe.http.client.HTTPSConnection', side_effect=connections): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        for index, connection in enumerate(connections[1:], 2):
            headers = connection.request.call_args.kwargs['headers']
            self.assertEqual(headers['Authorization'], f'Bearer test-token-{index}')
            self.assertEqual(headers['X-Sentinel-DC'], 'native-proof')
            self.assertNotIn('Cookie', headers)
            self.assertEqual(probe.CHALLENGES[('challenge-1', self.accounts[index-1]['alias'])][0], f'challenge-{index}')

    def test_failed_owner_challenge_does_not_reuse_or_block_other_owners(self):
        connections = [self.connection({'attestation_challenge': 'native'}),
                       self.connection({'error': 'unavailable'}, status=503),
                       self.connection({'attestation_challenge': 'third'}),
                       self.connection({'attestation_challenge': 'fourth'})]
        handler = self.handler('/backend-api/ios/attestation_challenge')
        with patch('probe.http.client.HTTPSConnection', side_effect=connections): handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertNotIn(('native', self.accounts[1]['alias']), probe.CHALLENGES)
        self.assertEqual(probe.CHALLENGES[('native', self.accounts[2]['alias'])][0], 'third')
        self.assertEqual(probe.CHALLENGES[('native', self.accounts[3]['alias'])][0], 'fourth')

    def test_challenge_capacity_scales_past_sixteen_owners(self):
        aliases = {f'handle-{index}': f'{index:08x}' for index in range(1, 19)}
        native = json.dumps({'attestation_challenge': 'native'})
        with patch.object(probe.settings, 'DOT_OWNERS', aliases):
            for alias in list(aliases.values())[1:]:
                probe.remember_challenge(native, json.dumps({'attestation_challenge': alias}), alias)
            for alias in list(aliases.values())[1:]:
                result = json.loads(probe.owner_message_body(json.dumps({'app_attest_challenge': 'native'}), alias))
                self.assertEqual(result['app_attest_challenge'], alias)

    def test_fourth_owner_send_replaces_proof_and_projects_self(self):
        owner = self.accounts[3]
        probe.remember_challenge(json.dumps({'attestation_challenge': 'native'}),
                                 json.dumps({'attestation_challenge': 'fourth'}), owner['alias'])
        body = json.dumps({'content': {'text': 'hello'}, 'request_id': 'r', 'idempotency_token': 'r',
                           'app_attest_challenge': 'native'}).encode()
        handler = self.handler('/backend-api/messaging/rooms/room-4/messages', 'POST', body)
        connection = self.connection({'id': 'message'})
        with patch('probe.http.client.HTTPSConnection', return_value=connection), patch('probe.self_projection.project_response', return_value=1) as project:
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        sent = connection.request.call_args.kwargs
        self.assertEqual(sent['headers']['Authorization'], 'Bearer test-token-4')
        self.assertNotIn('X-OpenAI-Orbit-Selection', sent['headers'])
        self.assertEqual(json.loads(sent['body'])['app_attest_challenge'], 'fourth')
        self.assertEqual(project.call_args.args[1], owner['viewer_ids'])
        self.assertTrue(project.call_args.kwargs['direct'])

    def test_fourth_owner_send_without_owner_proof_never_connects(self):
        body = json.dumps({'content': {'text': 'hello'}, 'request_id': 'r', 'idempotency_token': 'r',
                           'app_attest_challenge': 'unissued'}).encode()
        handler = self.handler('/backend-api/messaging/rooms/room-4/messages', 'POST', body)
        with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
        connection.assert_not_called(); handler.send_response.assert_called_once_with(409)

    def test_all_celsius_owners_receive_separate_sessions(self):
        handler = self.handler('/backend-api/celsius/ws/user'); broker = MagicMock()
        broker.issue.return_value = 'ws://127.0.0.1/combined'
        with patch.object(probe, 'CELSIUS', broker), patch('probe.http.client.HTTPSConnection', return_value=self.connection({'websocket_url': 'wss://owner-1'})), patch('probe.catalog.get', side_effect=lambda owner, *_: {'websocket_url': 'wss://' + owner['alias']}) as get:
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(get.call_count, 3)
        urls = broker.issue.call_args.args[0]
        self.assertEqual(set(urls), set(self.aliases.values()))
        self.assertEqual(urls[self.accounts[0]['alias']], 'wss://owner-1')
        for owner in self.accounts[1:]: self.assertEqual(urls[owner['alias']], 'wss://' + owner['alias'])

    def test_fourth_owner_runtime_controls_remain_pinned(self):
        path = '/backend-api/tbo/dot-4/runtime/pause'
        self.assertEqual(probe.runtime_control_owner(path, b'', self.fixture), self.accounts[3])
        for invalid in [path + '?x=1', '/backend-api/tbo/dot-1/runtime/pause', '/backend-api/tbo/unknown/runtime/pause']:
            self.assertIsNone(probe.runtime_control_owner(invalid, b'', self.fixture))
        self.assertIsNone(probe.runtime_control_owner(path, b'{}', self.fixture))
        owners = {**self.owners, 'root-4': self.accounts[1]}
        self.assertIsNone(probe.runtime_control_owner(path, b'', (self.accounts[0], self.profiles, owners, [])))

    def test_fourth_owner_runtime_control_requires_system_attestation(self):
        handler = self.handler('/backend-api/tbo/dot-4/runtime/pause', 'POST')
        with patch.object(probe, 'NATIVE_REBOOT_AUTH', None), patch('probe.refresh_native_control_identity', side_effect=ValueError('unverified')), patch('probe.http.client.HTTPSConnection') as connection:
            handler.handle_request()
        connection.assert_not_called(); handler.send_response.assert_called_once_with(409)

    def test_attested_system_request_routes_control_to_fourth_owner(self):
        handler = self.handler('/backend-api/tbo/dot-4/runtime/pause', 'POST')
        authority = (hashlib.sha256(b'Bearer native-token').digest(), time.monotonic())
        connection = self.connection({'status': 'paused', 'thread_id': 'root-4'})
        with patch.object(probe, 'NATIVE_REBOOT_AUTH', authority), patch('probe.http.client.HTTPSConnection', return_value=connection):
            handler.handle_request()
        handler.send_response.assert_called_once_with(200)
        headers = connection.request.call_args.kwargs['headers']
        self.assertEqual(headers['Authorization'], 'Bearer test-token-4')
        self.assertEqual(headers['ChatGPT-Account-Id'], 'account-4')

    def test_secondary_message_switch_covers_every_non_system_owner(self):
        probe.STATE['secondary_messages'] = False
        body = json.dumps({'content': {'text': 'hello'}, 'request_id': 'r', 'idempotency_token': 'r'}).encode()
        handler = self.handler('/backend-api/messaging/rooms/room-4/messages', 'POST', body)
        with patch('probe.http.client.HTTPSConnection') as connection: handler.handle_request()
        connection.assert_not_called(); handler.send_response.assert_called_once_with(503)

    def test_reboot_stays_disarmed_for_every_owner(self):
        for alias in self.aliases.values():
            with self.assertRaises(ValueError):
                probe.claim_reboot_owner({}, b'', self.fixture, {'primary_probe_owner': alias, 'reboot_armed': False})


class ListenPortTests(unittest.TestCase):
    def test_only_qualified_native_authentication_port_is_allowed(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(probe, 'ROOT', pathlib.Path(directory)):
            self.assertEqual(probe.listen_port(), 8000)
            (pathlib.Path(directory) / 'port').write_text('8000')
            self.assertEqual(probe.listen_port(), 8000)
            for invalid in ['0', '12345', '-1', '65536', 'invalid', ' ' * 32 + '8000']:
                (pathlib.Path(directory) / 'port').write_text(invalid)
                with self.assertRaises(ValueError): probe.listen_port()

    def test_occupied_native_port_fails_without_fallback_or_listener_changes(self):
        with patch.object(probe,'listen_port',return_value=8000), \
             patch.object(probe.http.server,'ThreadingHTTPServer',side_effect=OSError(probe.errno.EADDRINUSE,'private error')) as server:
            with self.assertRaisesRegex(ValueError,'^Native Dots proxy requires free loopback port 8000; existing listener was left running$'):
                probe.create_server()
        server.assert_called_once_with(('127.0.0.1',8000),probe.Handler)

    def test_port_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(probe, 'ROOT', pathlib.Path(directory)):
            path = pathlib.Path(directory); (path / 'value').write_text('12345'); (path / 'port').symlink_to(path / 'value')
            with self.assertRaises(ValueError): probe.listen_port()


if __name__ == '__main__': unittest.main()
