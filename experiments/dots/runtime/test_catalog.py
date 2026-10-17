"""Read-only owner discovery against synthetic credential and native API fixtures."""
import test_support
import base64
import copy
import hashlib
import json
import pathlib
import tempfile
import time
import unittest
from unittest.mock import Mock, patch
import catalog
import settings


def token(claims):
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')
    return 'synthetic.' + body + '.signature'


def document(owner, expiry=None):
    auth = {'chatgpt_account_id': owner, 'chatgpt_user_id': 'viewer-' + owner}
    return {'tokens': {'account_id': owner,
                       'id_token': token({'https://api.openai.com/auth': auth, 'email': owner + '@example.invalid'}),
                       'access_token': token({'https://api.openai.com/auth': auth,
                                              'exp': time.time() + 3600 if expiry is None else expiry}),
                       'refresh_token': 'synthetic-never-used'}}


def write(path, value):
    path.write_text(json.dumps(value))
    path.chmod(0o600)
    return path


def alias(owner):
    return hashlib.sha256(owner.encode()).hexdigest()[:8]


def primary(owner, name='Same', root=None):
    root = root or 'root-' + owner
    return {'selection': {'available': True, 'thread_id': root},
            'profile': {'id': 'profile-' + owner, 'active_root_thread_id': root,
                        'display_name': name, 'status': 'active'}}


class DynamicInventoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.root.chmod(0o700)
        self.system = write(self.root / 'auth.json', document('system'))
        self.accounts = self.root / 'accounts'
        self.accounts.mkdir(mode=0o700)
        for target, value in [('catalog.SYSTEM', self.system), ('catalog.CQ_ACCOUNTS', self.accounts),
                              ('catalog.SOURCE', None), ('settings.SYSTEM_ALIAS', alias('system'))]:
            patcher = patch(target, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_add_remove_and_rotate_are_observed_without_config_or_source_writes(self):
        before = self.system.read_bytes(), self.system.stat().st_mtime_ns
        self.assertEqual(len(catalog.inventory()[1]), 1)
        path = write(self.accounts / 'new.auth.json', document('added'))
        self.assertEqual({a['account_id'] for a in catalog.inventory()[1]}, {'system', 'added'})
        replacement = document('added')
        replacement['tokens']['access_token'] = token({'https://api.openai.com/auth': {
            'chatgpt_account_id': 'added', 'chatgpt_user_id': 'viewer-added'}, 'exp': time.time() + 7200})
        write(path, replacement)
        self.assertEqual(catalog.inventory()[1][1]['token'], replacement['tokens']['access_token'])
        path.unlink()
        self.assertEqual([a['account_id'] for a in catalog.inventory()[1]], ['system'])
        self.assertEqual((self.system.read_bytes(), self.system.stat().st_mtime_ns), before)
        self.assertEqual(list(self.accounts.iterdir()), [])

    def test_bad_and_expired_candidates_do_not_drop_healthy_owner(self):
        write(self.accounts / 'healthy.auth.json', document('healthy'))
        write(self.accounts / 'expired.auth.json', document('expired', expiry=0))
        write(self.accounts / 'broken.auth.json', {'tokens': []})
        with patch('catalog.get', side_effect=lambda account, *_: primary(account['account_id'])):
            result = catalog.discover()
        self.assertEqual({dot['owner_account_id'] for dot in result['dots']}, {'system', 'healthy'})
        expired = next(row for row in result['accounts'] if row.get('owner_account_id') == 'expired')
        self.assertEqual(expired['status'], 'unavailable')
        self.assertNotIn('synthetic-never-used', json.dumps(result))
        self.assertNotIn('signature', json.dumps(result))

    def test_system_identity_or_expiry_fails_closed(self):
        for value in [document('foreign'), document('system', expiry=0)]:
            write(self.system, value)
            with self.subTest(owner=value['tokens']['account_id']):
                with self.assertRaises(ValueError):
                    catalog.inventory()

    def test_symlink_and_permissive_source_fail_without_following_credentials(self):
        outside = write(self.root / 'outside.json', document('outside'))
        before = outside.read_bytes(), outside.stat().st_mode
        (self.accounts / 'outside.auth.json').symlink_to(outside)
        self.assertEqual([a['account_id'] for a in catalog.inventory()[1]], ['system'])
        self.accounts.chmod(0o755)
        with patch('catalog.get', return_value=primary('system')):
            result = catalog.discover()
        self.assertTrue(any(row.get('source') == 'cq' and row['status'] == 'unavailable' for row in result['accounts']))
        self.assertEqual((outside.read_bytes(), outside.stat().st_mode), before)

    def test_optional_codexbar_manifest_is_reread_and_fingerprint_verified(self):
        source = self.root / 'codexbar'
        source.mkdir(mode=0o755)
        source.chmod(0o755)
        managed = source / 'managed'
        managed.mkdir(mode=0o700)
        auth = write(managed / 'auth.json', document('external'))
        row = {'managedHomePath': str(managed), 'providerAccountID': 'external',
               'workspaceAccountID': 'external', 'authFingerprint': hashlib.sha256(auth.read_bytes()).hexdigest()}
        manifest = source / 'managed-codex-accounts.json'
        write(manifest, {'version': 3, 'accounts': [row]})
        with patch('catalog.SOURCE', source):
            self.assertEqual({a['account_id'] for a in catalog.inventory()[1]}, {'system', 'external'})
            row['authFingerprint'] = '0' * 64
            write(manifest, {'version': 3, 'accounts': [row]})
            self.assertEqual([a['account_id'] for a in catalog.inventory()[1]], ['system'])
            write(manifest, {'version': 3, 'accounts': []})
            self.assertEqual(len(catalog.inventory()[1]), 1)
        self.assertEqual(source.stat().st_mode & 0o777, 0o755)

    def test_writable_codexbar_source_is_rejected_without_permission_changes(self):
        source = self.root / 'codexbar'
        source.mkdir(mode=0o700)
        write(source / 'managed-codex-accounts.json', {'version': 3, 'accounts': []})
        for mode in (0o777, 0o775, 0o707):
            source.chmod(mode)
            with self.subTest(mode=mode), patch('catalog.SOURCE', source):
                _, accounts, failures = catalog._inventory()
                self.assertEqual([a['account_id'] for a in accounts], ['system'])
                self.assertTrue(any(row.get('source') == 'codexbar' for row in failures))
                self.assertEqual(source.stat().st_mode & 0o777, mode)

    def test_account_alias_collision_is_rejected(self):
        write(self.accounts / 'new.auth.json', document('added'))
        system = {'account_id': 'system', 'alias': '11111111'}
        added = {'account_id': 'added', 'alias': '11111111'}
        with patch('settings.SYSTEM_ALIAS', '11111111'), patch('catalog.credential', side_effect=[system, added]):
            with self.assertRaises(catalog.IdentityCollision):
                catalog.inventory()

    def test_duplicate_names_and_mutable_root_keep_owner_profile_handle(self):
        for owner in ['second', 'third']:
            write(self.accounts / (owner + '.auth.json'), document(owner))
        with patch('catalog.get', side_effect=lambda account, *_: primary(account['account_id'], 'same')):
            first = catalog.discover()
        with patch('catalog.get', side_effect=lambda account, *_: primary(account['account_id'], 'SAME', 'new-' + account['account_id'])):
            second = catalog.discover()
        self.assertEqual(len(first['dots']), 3)
        self.assertEqual({d['handle'] for d in first['dots']}, {d['handle'] for d in second['dots']})
        self.assertEqual({d['display_name'] for d in second['dots']}, {'SAME'})
        self.assertTrue(all(d['root_thread_id'].startswith('new-') for d in second['dots']))

    def test_no_dot_and_transient_owner_error_are_distinct(self):
        for owner in ['none', 'restricted']:
            write(self.accounts / (owner + '.auth.json'), document(owner))
        def get(account, *_):
            if account['account_id'] == 'none':
                return {'selection': None, 'profile': None}
            if account['account_id'] == 'restricted':
                raise catalog.CatalogHTTPError(403)
            return primary(account['account_id'])
        with patch('catalog.get', side_effect=get):
            result = catalog.discover()
        statuses = {row['owner_account_id']: row['status'] for row in result['accounts']}
        self.assertEqual(statuses, {'system': 'ready', 'none': 'no_dot', 'restricted': 'unavailable'})
        restricted = next(row for row in result['accounts'] if row['owner_account_id'] == 'restricted')
        self.assertEqual(restricted['http_status'], 403)

    def test_malformed_credential_claims_are_unavailable_without_losing_system(self):
        for bad in [[], {'https://api.openai.com/auth': []},
                    {'https://api.openai.com/auth': {'chatgpt_account_id': 'broken', 'chatgpt_user_id': []}, 'exp': time.time() + 3600}]:
            value = document('broken')
            value['tokens']['access_token'] = token(bad)
            write(self.accounts / 'broken.auth.json', value)
            with self.subTest(claims=bad):
                self.assertEqual([a['account_id'] for a in catalog.inventory()[1]], ['system'])

    def test_profile_collision_is_rejected(self):
        write(self.accounts / 'new.auth.json', document('added'))
        with patch('catalog.get', return_value=primary('shared')):
            with self.assertRaises(catalog.IdentityCollision):
                catalog.discover()


class DiscoveryRequestTests(unittest.TestCase):
    def test_headless_discovery_supplies_qualified_native_agent(self):
        for provided in ({}, {'user-agent': 'verified-native-agent'}):
            connection = Mock()
            connection.getresponse.return_value.status = 200
            connection.getresponse.return_value.read.return_value = b'{}'
            with patch('catalog.http.client.HTTPSConnection', return_value=connection):
                catalog.get({'token': 'synthetic', 'account_id': 'fixture'},
                            '/backend-api/tbo/primary', provided)
            headers = {key.lower(): value for key, value in connection.request.call_args.kwargs['headers'].items()}
            self.assertEqual(headers['user-agent'], provided.get('user-agent', catalog.NATIVE_USER_AGENT))
            self.assertEqual(headers['authorization'], 'Bearer synthetic')
            connection.close.assert_called_once()


class ManagedStateTests(unittest.TestCase):
    def fixture(self):
        # SHA-256 independently generated using encoding/json in Go, as ManagedStore does.
        return {'tokens': {'account_id': 'synthetic-owner', 'access_token': 'synthetic-access',
                           'id_token': 'synthetic-id', 'refresh_token': ''},
                '_cq': {'version': 1, 'account_key': 'acct_fixture', 'candidate_id': 'cand_fixture',
                        'lineage_id': 'lineage_fixture', 'generation': 1,
                        'revision': '7be84dd950cd8964793320b5d1a5a1b85dc36e954714fbbb3fcd9cdd58d989f6',
                        'provenance': 'cq_oauth', 'refresh_ownership': 'cq_owned_never_exported',
                        'operation_state': 'ready'}}

    def sign(self, value):
        unsigned = copy.deepcopy(value)
        unsigned['_cq']['revision'] = ''
        value['_cq']['revision'] = hashlib.sha256(json.dumps(unsigned, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        return value

    def test_go_revision_ready_record_and_legacy_records_are_accepted(self):
        self.assertTrue(catalog._managed_ready(self.fixture()))
        self.assertTrue(catalog._managed_ready(document('legacy')))

    def test_removed_candidate_is_excluded_and_uncertain_states_unavailable(self):
        value = self.fixture()
        value['_cq']['operation_state'] = 'removing'
        self.assertFalse(catalog._managed_ready(self.sign(value)))
        for state in ['refreshing', 'rotation_uncertain', 'activation_pending', 'unknown']:
            value['_cq']['operation_state'] = state
            with self.subTest(state=state):
                with self.assertRaisesRegex(ValueError, 'operation unavailable'):
                    catalog._managed_ready(self.sign(value))

    def test_metadata_and_revision_tampering_are_rejected(self):
        for field, content in [('revision', '0' * 64), ('version', 2), ('generation', 0),
                               ('candidate_id', ''), ('provenance', 'unknown-value')]:
            value = self.fixture()
            value['_cq'][field] = content
            with self.subTest(field=field):
                with self.assertRaises(ValueError):
                    catalog._managed_ready(value)


if __name__ == '__main__':
    unittest.main()
