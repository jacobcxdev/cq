import test_support
import unittest
from unittest.mock import Mock
from native_gate import NativeGate
import settings

class NativeGateTests(unittest.TestCase):
    def setUp(self):
        self.now=0
        self.system={'alias':settings.SYSTEM_ALIAS,'account_id':'system-account','token':'stored-token'}
        self.loader=Mock(return_value=self.system)
        self.upstream=Mock(return_value={'profile':{'id':settings.PROOF_PROFILE_IDS[settings.SYSTEM_ALIAS],'active_root_thread_id':'root'},'selection':{'available':True,'thread_id':'root'}})
        self.gate=NativeGate(self.loader,self.upstream,lambda:self.now)
        self.headers={'Authorization':'Bearer native-token','ChatGPT-Account-Id':'system-account'}

    def test_missing_authentication_never_reads_credentials_or_connects(self):
        for headers in [{},{'Authorization':'Bearer native-token'},{'Authorization':'invalid','ChatGPT-Account-Id':'system-account'}]:
            with self.assertRaises(ValueError):self.gate.require(headers)
        self.loader.assert_not_called();self.upstream.assert_not_called()

    def test_foreign_account_never_connects(self):
        with self.assertRaises(ValueError):self.gate.require({**self.headers,'ChatGPT-Account-Id':'other'})
        self.upstream.assert_not_called()

    def test_verifies_incoming_bearer_not_stored_token(self):
        self.gate.require(self.headers)
        self.assertEqual(self.upstream.call_args.args[0]['token'],'native-token')

    def test_invalid_remote_identity_never_caches(self):
        self.upstream.return_value={'profile':{},'selection':{'available':True,'thread_id':'root'}}
        for _ in range(2):
            with self.assertRaises(ValueError):self.gate.require(self.headers)
        self.assertEqual(self.upstream.call_count,2)

    def test_cache_expires_and_changed_bearer_requires_verification(self):
        self.gate.require(self.headers);self.gate.require(self.headers)
        self.assertEqual(self.upstream.call_count,1)
        self.gate.require({**self.headers,'Authorization':'Bearer another-token'})
        self.assertEqual(self.upstream.call_count,2)
        self.now=61;self.gate.require(self.headers)
        self.assertEqual(self.upstream.call_count,3)

    def test_failed_verification_never_caches(self):
        self.upstream.side_effect=ValueError('rejected')
        for _ in range(2):
            with self.assertRaises(ValueError):self.gate.require(self.headers)
        self.assertEqual(self.upstream.call_count,2)


class HandlerGateTests(unittest.TestCase):
    def test_rejects_before_fixture_or_upstream_access(self):
        import io,probe
        from unittest.mock import MagicMock,patch
        handler=probe.Handler.__new__(probe.Handler)
        handler.connection=MagicMock();handler.command='GET';handler.path='/backend-api/tbo'
        handler.headers={};handler.rfile=io.BytesIO();handler.wfile=io.BytesIO()
        handler.send_response=MagicMock();handler.send_header=MagicMock();handler.end_headers=MagicMock()
        with patch.object(probe.NATIVE_GATE,'require',side_effect=ValueError('missing')),patch('probe.catalog.fixture') as fixture,patch('probe.http.client.HTTPSConnection') as upstream,patch('probe.event'):
            handler.handle_request()
        fixture.assert_not_called();upstream.assert_not_called()
        handler.send_response.assert_called_once_with(401)

if __name__=='__main__':unittest.main()
