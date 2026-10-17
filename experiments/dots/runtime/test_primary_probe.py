import test_support
import copy, hashlib, io, time, unittest
from unittest.mock import patch, MagicMock
import probe

class PrimaryProbeTests(unittest.TestCase):
    def setUp(self):
        gate = patch.object(probe.NATIVE_GATE, 'require')
        gate.start(); self.addCleanup(gate.stop)
        self.owner={'alias':'22222222','account_id':'secondary','token':'test-token'}
        self.profile={'id':'secondary-dot','active_root_thread_id':'secondary-root'}
        self.fixture=({'alias':'11111111','account_id':'main','token':'main-token'},[self.profile],
                      {'secondary-dot':self.owner,'secondary-root':self.owner},[])
        self.response={'selection':{'available':True,'thread_id':'secondary-root','generation':'real-generation'},
                       'profile':copy.deepcopy(self.profile)}
        self.native_authority=(hashlib.sha256(b'Bearer main-token').digest(),time.monotonic())
    def read(self,response=None):
        with patch('probe.catalog.get',return_value=response or self.response) as get:
            result=probe.primary_probe_response({'Authorization':'Bearer main','Cookie':'main-cookie'},self.fixture,'22222222')
        return result,get
    def test_genuine_owner_selection_preserved(self):
        original=copy.deepcopy(self.response)
        result,get=self.read()
        self.assertEqual(result,original)
        args=get.call_args.args
        self.assertEqual(args[0],self.owner)
        self.assertEqual(args[1],'/backend-api/tbo/primary')
        self.assertEqual(args[2]['Authorization'],'Bearer test-token')
        self.assertNotIn('Cookie',args[2])
    def test_foreign_selection_rejected(self):
        self.response['selection']['thread_id']='foreign-root'
        with self.assertRaises(ValueError):self.read()
    def test_unavailable_selection_rejected(self):
        self.response['selection']['available']=False
        with self.assertRaises(ValueError):self.read()
    def test_foreign_profile_rejected(self):
        self.response['profile']['id']='foreign-dot'
        with self.assertRaises(ValueError):self.read()
    def test_unknown_owner_rejected_before_network(self):
        with patch('probe.catalog.get') as get:
            with self.assertRaises(ValueError):probe.primary_probe_response({},self.fixture,'unknown')
            get.assert_not_called()
    def test_reboot_handler_never_connects_upstream(self):
        handler=probe.Handler.__new__(probe.Handler)
        handler.connection=MagicMock()
        handler.path='/backend-api/cloud-aeons/primary/reboot'
        handler.command='POST'
        handler.headers={}
        handler.rfile=io.BytesIO(b'')
        handler.wfile=io.BytesIO()
        handler.send_response=MagicMock()
        handler.end_headers=MagicMock()
        with patch('probe.event') as event,patch('probe.http.client.HTTPSConnection') as connection:
            handler.handle_request()
        connection.assert_not_called()
        handler.send_response.assert_called_once_with(409)
        self.assertFalse(event.call_args.args[0]['upstream_request_sent'])
        self.assertIn(b'no automatic retry',handler.wfile.getvalue())
    def test_claim_requires_configured_secondary_context(self):
        state={'primary_probe_owner':None,'reboot_armed':True}
        with self.assertRaises(ValueError):probe.claim_reboot_owner({'Authorization':'Bearer main-token'},b'',self.fixture,state,self.native_authority)
    def test_claim_rejects_foreign_native_identity_and_body(self):
        for headers,body in [({'Authorization':'Bearer foreign'},b''),
                             ({'Authorization':'Bearer main-token','ChatGPT-Account-Id':'foreign'},b''),
                             ({'Authorization':'Bearer main-token'},b'{}')]:
            with self.subTest(headers=list(headers),body=bool(body)):
                state={'primary_probe_owner':'22222222','reboot_armed':True}
                with self.assertRaises(ValueError):probe.claim_reboot_owner(headers,body,self.fixture,state,self.native_authority)
                self.assertTrue(state['reboot_armed'])
    def test_claim_is_single_use(self):
        state={'primary_probe_owner':'22222222','reboot_armed':True}
        self.assertEqual(probe.claim_reboot_owner({'Authorization':'Bearer main-token'},b'',self.fixture,state,self.native_authority),self.owner)
        with self.assertRaises(ValueError):probe.claim_reboot_owner({'Authorization':'Bearer main-token'},b'',self.fixture,state,self.native_authority)
    def test_backend_verified_desktop_token_can_differ_from_cli_token(self):
        main_profile={'id':'main-dot','active_root_thread_id':'main-root'}
        self.fixture[1].append(main_profile)
        self.fixture[2].update({'main-dot':self.fixture[0],'main-root':self.fixture[0]})
        primary={'selection':{'available':True,'thread_id':'main-root'},'profile':main_profile}
        headers={'Authorization':'Bearer desktop-token','ChatGPT-Account-Id':'main'}
        authority=probe.native_reboot_authority(headers,self.fixture,primary,now=100)
        state={'primary_probe_owner':'22222222','reboot_armed':True}
        self.assertEqual(probe.claim_reboot_owner(headers,b'',self.fixture,state,authority,now=101),self.owner)
        self.assertFalse(state['reboot_armed'])
    def test_native_authority_rejects_secondary_profile_and_foreign_account(self):
        for headers in [{'Authorization':'Bearer desktop-token','ChatGPT-Account-Id':'main'},
                        {'Authorization':'Bearer desktop-token','ChatGPT-Account-Id':'foreign'}]:
            with self.assertRaises(ValueError):probe.native_reboot_authority(headers,self.fixture,self.response)
    def test_native_authority_absent_or_expired_never_consumes_arm(self):
        for authority in [None,(self.native_authority[0],100)]:
            state={'primary_probe_owner':'22222222','reboot_armed':True}
            with self.assertRaises(ValueError):probe.claim_reboot_owner({'Authorization':'Bearer main-token'},b'',self.fixture,state,authority,now=401)
            self.assertTrue(state['reboot_armed'])
    def test_native_authority_requires_main_profile_root_and_bearer(self):
        profile={'id':'main-dot','active_root_thread_id':'main-root'}
        self.fixture[1].append(profile)
        self.fixture[2].update({'main-dot':self.fixture[0],'main-root':self.fixture[0]})
        primary={'selection':{'available':True,'thread_id':'main-root'},'profile':profile}
        headers={'Authorization':'Bearer desktop-token','ChatGPT-Account-Id':'main'}
        cases=[({},primary),(dict(headers,**{'ChatGPT-Account-Id':'foreign'}),primary),
               (headers,{'selection':{'available':False,'thread_id':'main-root'},'profile':profile}),
               (headers,{'selection':{'available':True,'thread_id':'foreign-root'},'profile':profile}),
               (headers,{'selection':primary['selection'],'profile':dict(profile,active_root_thread_id='foreign-root')})]
        for incoming,value in cases:
            with self.assertRaises(ValueError):probe.native_reboot_authority(incoming,self.fixture,value)
    def handler(self):
        handler=probe.Handler.__new__(probe.Handler)
        handler.connection=MagicMock()
        handler.path='/backend-api/cloud-aeons/primary/reboot'
        handler.command='POST'
        handler.headers={'Authorization':'Bearer main-token','ChatGPT-Account-Id':'main','Host':'localhost',
                         'Cookie':'main-cookie','X-OpenAI-Expected-Account-Id':'main','X-Sentinel-DC':'native-proof'}
        handler.rfile=io.BytesIO(b'');handler.wfile=io.BytesIO()
        handler.send_response=MagicMock();handler.send_header=MagicMock();handler.end_headers=MagicMock()
        return handler
    def test_owner_reboot_revalidates_then_routes_with_real_owner(self):
        handler=self.handler();conn=MagicMock()
        response=conn.getresponse.return_value;response.status=202;response.read.return_value=b'';response.getheaders.return_value=[]
        state={'primary_probe_owner':'22222222','reboot_armed':True}
        with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.STATE',state),patch('probe.NATIVE_REBOOT_AUTH',self.native_authority),patch('probe.catalog.get',return_value=self.response) as get,patch('probe.http.client.HTTPSConnection',return_value=conn),patch('probe.event'):
            handler.handle_request()
        self.assertEqual(get.call_args.args[0],self.owner)
        self.assertEqual(conn.request.call_args.args,('POST','/backend-api/cloud-aeons/primary/reboot'))
        headers=conn.request.call_args.kwargs['headers']
        self.assertEqual(headers['Authorization'],'Bearer test-token')
        self.assertEqual(headers['ChatGPT-Account-Id'],'secondary')
        self.assertEqual(headers['X-Sentinel-DC'],'native-proof')
        self.assertEqual(headers['Accept-Encoding'],'identity')
        for key in ['Cookie','Host','X-OpenAI-Expected-Account-Id']:self.assertNotIn(key,headers)
        self.assertFalse(state['reboot_armed'])
        handler.send_response.assert_called_once_with(202)
    def test_revalidation_failure_never_submits(self):
        handler=self.handler();state={'primary_probe_owner':'22222222','reboot_armed':True}
        self.response['selection']['thread_id']='foreign-root'
        with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.STATE',state),patch('probe.NATIVE_REBOOT_AUTH',self.native_authority),patch('probe.catalog.get',return_value=self.response),patch('probe.http.client.HTTPSConnection') as conn,patch('probe.event'):
            handler.handle_request()
        conn.assert_not_called();self.assertFalse(state['reboot_armed'])
        handler.send_response.assert_called_once_with(409)
    def test_uncertain_submit_never_retries(self):
        handler=self.handler();state={'primary_probe_owner':'22222222','reboot_armed':True};conn=MagicMock()
        conn.request.side_effect=TimeoutError()
        with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.STATE',state),patch('probe.NATIVE_REBOOT_AUTH',self.native_authority),patch('probe.catalog.get',return_value=self.response),patch('probe.http.client.HTTPSConnection',return_value=conn),patch('probe.event'):
            handler.handle_request()
        self.assertEqual(conn.request.call_count,1);self.assertFalse(state['reboot_armed'])
        handler.send_response.assert_called_once_with(502)

if __name__=='__main__':unittest.main()
