import test_support
import http.client, json, unittest
from local_executor import RegistryBroker, ALIAS, cli_environment, validate_registry_response

class ExecutorBoundaryTests(unittest.TestCase):
    def test_rejects_main_owner(self):
        with self.assertRaises(ValueError): RegistryBroker({'alias': test_support.CONFIG['system_alias']}, 'test')

    def test_child_has_no_inherited_credentials_or_plugin_pipe(self):
        env = cli_environment('local-only')
        self.assertEqual(env['CODEX_API_KEY'], 'local-only')
        for key in ['CODEX_ACCESS_TOKEN', 'CODEX_AUTH', 'BROWSER_USE_ACCESS_PIPE_PATH',
                    'CODEX_CLI_PATH', 'HTTPS_PROXY', 'SSL_CERT_FILE']:
            self.assertNotIn(key, env)
        self.assertTrue(env['CODEX_HOME'].endswith('/null-executor/home'))

    def test_registry_rejects_wrong_environment_and_untrusted_url(self):
        for value in [{'environment_id': 'main', 'url': 'wss://chatgpt.com/x'},
                      {'environment_id': 'test', 'url': 'wss://chatgpt.com.evil.test/x'},
                      {'environment_id': 'test', 'url': 'wss://user@chatgpt.com/x'}]:
            with self.assertRaises(ValueError): validate_registry_response(json.dumps(value), 'test')

    def test_broker_blocks_wrong_capability_and_foreign_environment_without_forwarding(self):
        calls = []
        broker = RegistryBroker({'alias': ALIAS}, 'test', lambda *args: calls.append(args))
        broker.start()
        try:
            for path, capability in [('/api/cloud/environment/main/register', broker.capability),
                                     ('/api/cloud/environment/test/register', 'wrong')]:
                conn = http.client.HTTPConnection('127.0.0.1', broker.server.server_port)
                conn.request('POST', path, b'{}', {'Authorization': 'Bearer ' + capability})
                response = conn.getresponse(); response.read(); conn.close()
                self.assertEqual(response.status, 403)
            self.assertEqual(calls, [])
        finally: broker.close()

if __name__ == '__main__': unittest.main()
