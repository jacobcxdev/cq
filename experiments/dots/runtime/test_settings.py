"""Offline configuration checks using synthetic paths and identifiers."""
import copy
import json
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch
import settings


class SettingsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = pathlib.Path(self.temp.name)
        self.directory.chmod(0o700)
        self.path = self.directory / 'settings.json'
        self.data = json.loads(pathlib.Path(__file__).with_name('settings.example.json').read_text())
        self.data['runtime_root'] = str(self.directory / 'runtime')
        self.write()

    def write(self):
        self.path.write_text(json.dumps(self.data))
        self.path.chmod(0o600)

    def test_valid_private_settings(self):
        value = settings.load(self.path)
        self.assertEqual(value.ROOT, (self.directory / 'runtime').resolve())
        self.assertEqual(value.DOTS['Secondary'], ('secondary-profile-example', 'secondary-root-example'))
        self.assertEqual(value.PROOF_PROFILE_IDS, {'11111111': 'primary-profile-example', '22222222': 'secondary-profile-example'})
        self.assertEqual(value.DOT_OWNERS['Primary'], value.SYSTEM_ALIAS)
        self.assertEqual(value.DEBUG_PORT, 0)

    def test_environment_path_and_cached_constants(self):
        settings.get_settings.cache_clear()
        self.addCleanup(settings.get_settings.cache_clear)
        with patch.dict(os.environ, {'CQ_DOTS_CONFIG': str(self.path)}):
            self.assertEqual(settings.ROOT, (self.directory / 'runtime').resolve())
            self.assertEqual(settings.SYSTEM_ALIAS, '11111111')

    def test_missing_config_fails_without_discovery(self):
        with patch.dict(os.environ, {}, clear=True):
            with self.assertRaisesRegex(ValueError, 'CQ_DOTS_CONFIG'):
                settings.load()

    def test_file_and_parent_permissions(self):
        for mode in [0o644, 0o640, 0o400, 0o700]:
            with self.subTest(mode=mode):
                self.path.chmod(mode)
                with self.assertRaisesRegex(ValueError, '0600'):
                    settings.load(self.path)
        self.path.chmod(0o600)
        self.directory.chmod(0o755)
        with self.assertRaisesRegex(ValueError, '0700'):
            settings.load(self.path)
        self.directory.chmod(0o700)

    def test_foreign_owner_is_rejected(self):
        with patch('settings.os.getuid', return_value=os.getuid() + 1):
            with self.assertRaisesRegex(ValueError, 'owned'):
                settings.load(self.path)

    def test_symlink_config_and_parent_are_rejected(self):
        link = self.directory / 'link.json'
        link.symlink_to(self.path)
        with self.assertRaises(OSError):
            settings.load(link)
        directory_link = self.directory / 'link'
        directory_link.symlink_to(self.directory, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, '0700'):
            settings.load(directory_link / self.path.name)

    def test_non_regular_and_oversize_config_are_rejected(self):
        fifo = self.directory / 'fifo'
        os.mkfifo(fifo, 0o600)
        with self.assertRaisesRegex(ValueError, 'regular file'):
            settings.load(fifo)
        self.path.write_bytes(b' ' * (1024 * 1024 + 1))
        with self.assertRaisesRegex(ValueError, 'regular file'):
            settings.load(self.path)

    def test_invalid_json_does_not_echo_contents(self):
        self.path.write_text('synthetic-private-value')
        with self.assertRaises(ValueError) as error:
            settings.load(self.path)
        self.assertNotIn('synthetic-private-value', str(error.exception))

    def test_invalid_fields_and_account_scope(self):
        changes = [{'runtime_root': 'relative'}, {'account_source': '/tmp/../private'},
                   {'system_alias': 'invalid'}, {'secondary_alias': '11111111'},
                   {'secondary_selector': ''}, {'asar_sha256': 'invalid'},
                   {'module_asset': '../app-shared-example.js'}, {'debug_port': True},
                   {'debug_port': 65536}, {'unknown': 'ignored'}, {'dots': []}]
        for change in changes:
            with self.subTest(change=change):
                original = self.data
                self.data = dict(original, **change)
                self.write()
                with self.assertRaises(ValueError):
                    settings.load(self.path)
                self.data = original

    def test_profile_authority_collisions(self):
        for field, value in [('owner_alias', '11111111'), ('owner_alias', 'invalid'),
                             ('profile_id', 'primary-profile-example'),
                             ('root_thread_id', 'primary-root-example')]:
            with self.subTest(field=field, value=value):
                original = copy.deepcopy(self.data)
                self.data['dots']['Secondary'][field] = value
                self.write()
                with self.assertRaisesRegex(ValueError, 'ownership'):
                    settings.load(self.path)
                self.data = original

    def test_runtime_stays_private_and_outside_source(self):
        runtime = self.directory / 'runtime'
        runtime.mkdir(mode=0o755)
        with self.assertRaisesRegex(ValueError, '0700'):
            settings.load(self.path)
        runtime.chmod(0o700)
        self.assertEqual(settings.load(self.path).ROOT, runtime.resolve())
        self.data['runtime_root'] = str(pathlib.Path(settings.__file__).parent)
        self.write()
        with self.assertRaisesRegex(ValueError, 'outside the source tree'):
            settings.load(self.path)

    def test_runtime_excludes_application_and_credential_paths(self):
        home = pathlib.Path.home()
        protected = [home, home / '.codex' / 'dots',
                     home / 'Library' / 'Application Support' / 'ChatGPT' / 'dots',
                     pathlib.Path(self.data['app_path']) / 'runtime',
                     pathlib.Path(self.data['account_source']) / 'runtime',
                     pathlib.Path(self.data['system_auth']).parent / 'runtime']
        for root in protected:
            with self.subTest(root=str(root)):
                self.data['runtime_root'] = str(root)
                self.write()
                with self.assertRaises(ValueError):
                    settings.load(self.path)

    def test_first_read_atime_change_is_allowed(self):
        before = self.path.stat()
        fields = {name: getattr(before, name) for name in
                  ('st_dev', 'st_ino', 'st_mode', 'st_uid', 'st_gid',
                   'st_size', 'st_mtime_ns', 'st_ctime_ns', 'st_atime_ns')}
        fields['st_atime_ns'] += 1
        with patch('settings.os.fstat', side_effect=[before, SimpleNamespace(**fields)]):
            self.assertEqual(settings.load(self.path).SYSTEM_ALIAS, '11111111')
        fields['st_mtime_ns'] += 1
        with patch('settings.os.fstat', side_effect=[before, SimpleNamespace(**fields)]):
            with self.assertRaisesRegex(ValueError, 'changed during read'):
                settings.load(self.path)

    def test_shallow_installed_path_does_not_assume_ancestors(self):
        with patch('settings.__file__', '/settings.py'):
            self.assertEqual(settings.load(self.path).SYSTEM_ALIAS, '11111111')

    def test_mutable_state_symlinks_and_permissions_are_rejected(self):
        runtime = self.directory / 'runtime'
        runtime.mkdir(mode=0o700)
        foreign = self.directory / 'foreign'
        foreign.mkdir(mode=0o700)
        for name in ('user-data', 'codex-home', 'null-executor'):
            with self.subTest(name=name):
                target = runtime / name
                target.symlink_to(foreign, target_is_directory=True)
                with self.assertRaisesRegex(ValueError, '0700'):
                    settings.load(self.path)
                target.unlink()
                target.mkdir(mode=0o755)
                with self.assertRaisesRegex(ValueError, '0700'):
                    settings.validate_runtime_paths(runtime)
                target.chmod(0o700)
                settings.validate_runtime_paths(runtime)
        launch = runtime / 'launch.json'
        launch.symlink_to(self.path)
        with self.assertRaisesRegex(ValueError, 'launch state'):
            settings.load(self.path)
        launch.unlink()
        launch.write_text('{}')
        launch.chmod(0o644)
        with self.assertRaisesRegex(ValueError, 'launch state'):
            settings.load(self.path)
        launch.chmod(0o600)
        settings.validate_runtime_paths(runtime)

    def test_single_account_and_optional_sources(self):
        self.data.pop('secondary_alias')
        self.data.pop('secondary_selector')
        self.data.pop('account_source')
        self.data['dots'] = {'Primary': self.data['dots']['Primary']}
        self.write()
        value = settings.load(self.path)
        self.assertIsNone(value.SOURCE)
        self.assertIsNone(value.SECONDARY_ALIAS)
        self.assertEqual(value.CQ_ACCOUNTS, value.SYSTEM.parent / 'accounts')
        self.assertEqual(len(value.DOTS), 1)
        self.data['dots'] = {}
        self.write()
        self.assertEqual(settings.load(self.path).DOTS, {})

    def test_many_accounts_and_duplicate_names_have_stable_identity(self):
        import hashlib
        self.data['dots'] = {}
        for index, name in enumerate(['Same', 'Same', 'sAME', 'directory']):
            owner = 'synthetic-owner-' + str(index)
            alias = hashlib.sha256(owner.encode()).hexdigest()[:8]
            profile = 'synthetic-profile-' + str(index)
            handle = settings.stable_handle(owner, profile)
            self.data['dots'][handle] = {'owner_alias': alias, 'owner_account_id': owner,
                                        'profile_id': profile, 'root_thread_id': 'root-' + str(index),
                                        'display_name': name}
        self.write()
        value = settings.load(self.path)
        self.assertEqual(len(value.DOTS), 4)
        self.assertEqual(list(value.DOT_NAMES.values()), ['Same', 'Same', 'sAME', 'directory'])
        handle, row = next(iter(self.data['dots'].items()))
        row['display_name'] = 'Renamed'
        row['root_thread_id'] = 'changed-root'
        self.write()
        self.assertEqual(settings.load(self.path).DOT_NAMES[handle], 'Renamed')
        row['owner_account_id'] = 'foreign-owner'
        self.write()
        with self.assertRaisesRegex(ValueError, 'stable identity'):
            settings.load(self.path)

    def test_legacy_directory_routing_key_reserved(self):
        self.data['dots'] = {'directory': self.data['dots']['Primary']}
        self.write()
        with self.assertRaisesRegex(ValueError, 'profile configuration'):
            settings.load(self.path)

    def test_full_system_identity_pin_is_verified(self):
        import hashlib
        owner = 'synthetic-system-owner'
        self.data['system_alias'] = hashlib.sha256(owner.encode()).hexdigest()[:8]
        self.data['system_account_id'] = owner
        self.write()
        self.assertEqual(settings.load(self.path).SYSTEM_ACCOUNT_ID, owner)
        self.data['system_account_id'] = 'foreign-owner'
        self.write()
        with self.assertRaisesRegex(ValueError, 'system account identity'):
            settings.load(self.path)

    def test_reload_refreshes_cached_configuration(self):
        settings.get_settings.cache_clear()
        self.addCleanup(settings.get_settings.cache_clear)
        with patch.dict(os.environ, {'CQ_DOTS_CONFIG': str(self.path)}):
            self.assertEqual(settings.DEBUG_PORT, 0)
            self.data['debug_port'] = 12345
            self.write()
            self.assertEqual(settings.DEBUG_PORT, 0)
            self.assertEqual(settings.reload().DEBUG_PORT, 12345)

    def test_debug_port_default_and_upper_bound(self):
        del self.data['debug_port']
        self.write()
        self.assertEqual(settings.load(self.path).DEBUG_PORT, 0)
        self.data['debug_port'] = 65535
        self.write()
        self.assertEqual(settings.load(self.path).DEBUG_PORT, 65535)


class CatalogSettingsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import test_support
        settings.get_settings.cache_clear()
        import catalog
        cls.catalog = catalog

    def test_primary_roots_use_configured_profiles(self):
        accounts = {alias: {'alias': alias} for alias in settings.PROOF_PROFILE_IDS}
        responses = [
            {'selection': {'available': True, 'thread_id': root},
             'profile': {'id': profile, 'active_root_thread_id': root}}
            for profile, root in settings.DOTS.values()
        ]
        with patch.object(self.catalog, 'get', side_effect=responses):
            roots = self.catalog.primary_roots(accounts, {})
        self.assertEqual(set(roots), {root for _, root in settings.DOTS.values()})

    def test_fixture_uses_all_configured_owner_profiles(self):
        system = {'account_id': 'primary-account', 'alias': settings.SYSTEM_ALIAS, 'email': 'primary@example.invalid'}
        secondary = {'account_id': 'secondary-account', 'alias': settings.SECONDARY_ALIAS,
                     'email': settings.SECONDARY_SELECTOR}
        primary_profile = {'id': 'primary-profile-example', 'display_name': 'Primary'}
        secondary_profile = {'id': 'secondary-profile-example', 'display_name': 'Secondary'}
        with patch.object(self.catalog, 'inventory', return_value=(system, [system, secondary])), \
                patch.object(self.catalog, 'catalog', return_value=[secondary_profile]) as fetch:
            _, profiles, owners, summary = self.catalog.fixture({}, [primary_profile])
        fetch.assert_called_once_with(secondary, {})
        self.assertEqual(len(profiles), 2)
        self.assertEqual(owners['secondary-profile-example']['alias'], settings.SECONDARY_ALIAS)
        self.assertTrue(summary[1]['requested_secondary'])

    def test_credential_first_read_atime_change_is_allowed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'auth.json'
            path.write_bytes(b'synthetic-credential-fixture')
            path.chmod(0o600)
            before = path.stat()
            fields = {name: getattr(before, name) for name in
                      ('st_dev', 'st_ino', 'st_mode', 'st_uid', 'st_gid',
                       'st_size', 'st_mtime_ns', 'st_ctime_ns', 'st_atime_ns')}
            fields['st_atime_ns'] += 1
            with patch('catalog.os.fstat', side_effect=[before, SimpleNamespace(**fields)]):
                self.assertEqual(self.catalog.read_owned(path), b'synthetic-credential-fixture')
            fields['st_ctime_ns'] += 1
            with patch('catalog.os.fstat', side_effect=[before, SimpleNamespace(**fields)]):
                with self.assertRaisesRegex(ValueError, 'revision changed'):
                    self.catalog.read_owned(path)

    def test_inventory_rejects_system_principal_drift_before_manifest(self):
        with patch.object(self.catalog, 'read_owned', return_value=b'{}') as read, \
                patch.object(self.catalog, 'credential', return_value={'alias': '33333333'}):
            with self.assertRaisesRegex(ValueError, 'system credential principal changed'):
                self.catalog.inventory()
        read.assert_called_once_with(self.catalog.SYSTEM)


if __name__ == '__main__':
    unittest.main()
