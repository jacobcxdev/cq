import test_support
import copy
import json
import pathlib
import tempfile
import threading
import unittest
from unittest.mock import MagicMock, patch

import secondary_executors as executors


class SecondaryExecutorTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.main = {'alias': '11111111', 'account_id': 'main'}
        self.owner = {'alias': '22222222', 'account_id': 'secondary', 'token': 'old-token'}
        for name, value in [('ROOT', self.root), ('SYSTEM_ALIAS', self.main['alias']),
                            ('DOTS', {'main': ('main-profile', 'main-root'), 'other': ('secondary-profile', 'secondary-root')}),
                            ('DOT_OWNERS', {'main': self.main['alias'], 'other': self.owner['alias']})]:
            setting = patch.object(executors.settings, name, value, create=True)
            setting.start(); self.addCleanup(setting.stop)
        self.record = dict(owner_alias=self.owner['alias'], profile_id='secondary-profile',
                           environment_id='secondary-environment', primary_environment_id='primary-environment',
                           active=True, registered=True)

    def save(self, row):
        root = executors.directory()
        executors.awareness._save(root / 'state.json', {'owners': {self.owner['alias']: row}})

    def test_mapping_requires_private_active_matching_owner_and_profile(self):
        self.assertIsNone(executors.mapping(self.owner['alias']))
        self.save(self.record)
        self.assertEqual(executors.mapping(self.owner['alias']), self.record)
        self.assertIsNone(executors.mapping(self.main['alias']))
        for field, value in [('active', False), ('registered', False), ('owner_alias', 'foreign'),
                             ('profile_id', 'main-profile'), ('environment_id', '../bad'),
                             ('primary_environment_id', '\u00e9')]:
            with self.subTest(field=field):
                self.save(dict(self.record, **{field: value}))
                self.assertIsNone(executors.mapping(self.owner['alias']))
        self.save(self.record)
        (self.root / 'executors/state.json').chmod(0o644)
        self.assertIsNone(executors.mapping(self.owner['alias']))

    def test_projection_changes_only_matching_id_and_keeps_real_offline_status(self):
        self.save(self.record)
        value = {'thread_id': 'secondary-root', 'computers': [
            {'environment_id': 'cloud', 'kind': 'openai_cloud', 'is_attached': True},
            {'environment_id': 'secondary-environment', 'kind': 'user', 'status': 'offline',
             'is_attached': False, 'can_use_for_tasks': False, 'can_connect': True},
            {'environment_id': 'another-mac', 'kind': 'user', 'status': 'connected'}]}
        before = json.dumps(value).encode()
        expected = copy.deepcopy(value)
        expected['computers'][1]['environment_id'] = 'primary-environment'
        self.assertEqual(json.loads(executors.project_computers(self.owner['alias'], 'secondary-profile', before)), expected)
        self.assertEqual(executors.project_computers(self.owner['alias'], 'main-profile', before), before)
        self.assertEqual(executors.project_computers(self.main['alias'], 'main-profile', before), before)

    def test_owner_request_rereads_credentials_and_rejects_changed_principal(self):
        fresh = dict(self.owner, token='fresh-token')
        with patch.object(executors.catalog, 'inventory', return_value=(self.main, [self.main, fresh])), \
             patch.object(executors.local_executor, 'owner_request', return_value=(200, 'application/json', b'{}')) as send:
            executors.owner_request(self.owner, 'chatgpt.com', 'POST', '/bound', b'body')
        send.assert_called_once_with(fresh, 'chatgpt.com', 'POST', '/bound', b'body', expected_alias=self.owner['alias'])
        for accounts in [[self.main], [dict(fresh, account_id='foreign')], [fresh, fresh]]:
            with patch.object(executors.catalog, 'inventory', return_value=(self.main, accounts)), \
                 patch.object(executors.local_executor, 'owner_request') as send, self.assertRaises(ValueError):
                executors.owner_request(self.owner, 'chatgpt.com', 'POST', '/bound')
            send.assert_not_called()

    def test_child_uses_private_home_and_only_local_registry_capability(self):
        broker = MagicMock(capability='local-capability')
        broker.server.server_port = 12345
        response = (200, 'application/json', b'{"executor_id":"secondary-environment"}')
        with patch.object(executors, 'owner_request', return_value=response) as request, \
             patch.object(executors.local_executor, 'RegistryBroker', return_value=broker) as create, \
             patch.object(executors.subprocess, 'Popen') as popen:
            worker = executors.start_owner(self.owner, 'primary-environment')
            first = json.loads(request.call_args.args[4])['installation_id']
            executors.start_owner(self.owner, 'primary-environment')
            second = json.loads(request.call_args.args[4])['installation_id']
        self.assertEqual(first, second)
        self.assertEqual(create.call_args.kwargs['expected_alias'], self.owner['alias'])
        args, options = popen.call_args.args[0], popen.call_args.kwargs
        self.assertNotIn('/usr/bin/sandbox-exec', args)
        self.assertIn('--exit-on-stdin-close', args)
        self.assertEqual(args[args.index('--environment-id') + 1], 'secondary-environment')
        self.assertEqual(options['env']['CODEX_API_KEY'], 'local-capability')
        self.assertEqual(options['env']['CODEX_HOME'], str(self.root / 'executors/22222222/home'))
        self.assertNotIn('old-token', json.dumps(options['env']))
        self.assertEqual(worker['record']['primary_environment_id'], 'primary-environment')
        with self.assertRaises(ValueError):
            executors.start_owner(self.main, 'primary-environment')

    def test_registration_failure_never_launches_child(self):
        for response in [(403, 'application/json', b'{}'), (200, 'application/json', b'{"executor_id":"../bad"}')]:
            with patch.object(executors, 'owner_request', return_value=response), \
                 patch.object(executors.subprocess, 'Popen') as popen, self.assertRaises(ValueError):
                executors.start_owner(self.owner, 'primary-environment')
            popen.assert_not_called()

    def test_attach_happens_once_after_real_registry_success(self):
        broker = MagicMock(events=[])
        broker.lock = threading.Lock(); broker.stopped.is_set.return_value = False
        child = MagicMock(); child.poll.return_value = None
        worker = dict(owner=self.owner, broker=broker, child=child,
                      record=dict(self.record, registered=False), connect_attempted=False)
        with patch.object(executors.catalog, 'get', return_value={'profile': {'id': 'secondary-profile',
                             'active_root_thread_id': 'fresh-root'}}), \
             patch.object(executors, 'owner_request', return_value=(204, '', b'')) as request:
            self.assertFalse(executors.refresh(worker)['registered'])
            request.assert_not_called()
            broker.events.append({'operation': 'register', 'status': 200})
            self.assertEqual(executors.refresh(worker)['connect_http'], 204)
            executors.refresh(worker)
        request.assert_called_once_with(self.owner, 'chatgpt.com', 'POST',
            '/backend-api/tbo/secondary-profile/computers/secondary-environment/connect?expected_thread_id=fresh-root')
        child.poll.return_value = 1
        self.assertFalse(executors.refresh(worker)['active'])


if __name__ == '__main__':
    unittest.main()
