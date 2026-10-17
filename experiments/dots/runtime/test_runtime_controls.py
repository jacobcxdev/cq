import test_support
import hashlib,io,json,time,unittest
from unittest.mock import MagicMock,patch
import probe

class RuntimeControlTests(unittest.TestCase):
    def setUp(self):
        gate = patch.object(probe.NATIVE_GATE, 'require')
        gate.start(); self.addCleanup(gate.stop)
        self.main={'account_id':'main','alias':'11111111'}
        self.owner={'account_id':'worker','alias':'22222222','token':'worker-token'}
        self.profile={'id':'dot','active_root_thread_id':'root'}
        self.fixture=(self.main,[self.profile],{'dot':self.owner,'root':self.owner},[])
        self.authority=(hashlib.sha256(b'Bearer native').digest(),time.monotonic())
        self.headers={'Authorization':'Bearer native','ChatGPT-Account-Id':'main'}
    def test_exact_owner_pause_and_resume(self):
        for operation in ['pause','resume']:
            path='/backend-api/tbo/dot/runtime/'+operation
            self.assertIs(probe.runtime_control_owner(path,b'',self.fixture),self.owner)
            probe.verify_native_control_identity(self.headers,self.main,self.authority)
    def test_other_resources_bodies_and_operations_rejected(self):
        for path,body in [('/backend-api/tbo/unknown/runtime/pause',b''),
                          ('/backend-api/tbo/root/runtime/pause',b''),
                          ('/backend-api/tbo/dot/runtime/delete',b''),
                          ('/backend-api/tbo/dot/runtime/pause?owner=other',b''),
                          ('/backend-api/tbo/dot%2Fruntime/runtime/pause',b''),
                          ('/backend-api/tbo/dot/runtime/pause',b'{}')]:
            self.assertIsNone(probe.runtime_control_owner(path,body,self.fixture))
    def test_primary_owner_and_conflicting_root_rejected(self):
        self.fixture[2]['dot']=self.main
        self.assertIsNone(probe.runtime_control_owner('/backend-api/tbo/dot/runtime/pause',b'',self.fixture))
        self.fixture[2]['dot']=self.owner;self.fixture[2]['root']=self.main
        self.assertIsNone(probe.runtime_control_owner('/backend-api/tbo/dot/runtime/pause',b'',self.fixture))
    def test_unverified_native_caller_rejected(self):
        for headers,authority in [({},self.authority),
                                  ({'Authorization':'Bearer foreign'},self.authority),
                                  ({**self.headers,'ChatGPT-Account-Id':'foreign'},self.authority),
                                  (self.headers,None)]:
            with self.assertRaises(ValueError):probe.verify_native_control_identity(headers,self.main,authority)
    def handler(self,operation):
        handler=probe.Handler.__new__(probe.Handler)
        handler.connection=MagicMock()
        handler.path='/backend-api/tbo/dot/runtime/'+operation;handler.command='POST'
        handler.headers={**self.headers,'Cookie':'main-cookie','X-OpenAI-Expected-Account-Id':'main',
                         'X-OpenAI-Orbit-Selection':'main-selection','X-Sentinel-DC':'native-proof'}
        handler.rfile=io.BytesIO(b'');handler.wfile=io.BytesIO()
        handler.send_response=MagicMock();handler.send_header=MagicMock();handler.end_headers=MagicMock()
        return handler
    def test_handler_routes_pause_and_resume_with_owner_authentication(self):
        for operation in ['pause','resume']:
            handler=self.handler(operation);connection=MagicMock()
            response=connection.getresponse.return_value
            response.status=200;response.read.return_value=json.dumps({'status':'applied','thread_id':'root'}).encode();response.getheaders.return_value=[]
            with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.NATIVE_REBOOT_AUTH',self.authority),patch('probe.http.client.HTTPSConnection',return_value=connection),patch('probe.event') as event:
                handler.handle_request()
            request=connection.request.call_args
            self.assertEqual(request.args,('POST',handler.path))
            self.assertIsNone(request.kwargs['body'])
            headers=request.kwargs['headers']
            self.assertEqual(headers['Authorization'],'Bearer worker-token')
            self.assertEqual(headers['ChatGPT-Account-Id'],'worker')
            self.assertEqual(headers['X-Sentinel-DC'],'native-proof')
            for name in ['Cookie','X-OpenAI-Expected-Account-Id','X-OpenAI-Orbit-Selection']:self.assertNotIn(name,headers)
            self.assertTrue(event.call_args.args[0]['runtime_result_known_root'])
            handler.send_response.assert_called_once_with(200)
    def test_handler_rejects_unverified_caller_before_network(self):
        handler=self.handler('pause')
        handler.headers['ChatGPT-Account-Id']='foreign'
        with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.NATIVE_REBOOT_AUTH',None),patch('probe.http.client.HTTPSConnection') as connection,patch('probe.event'):
            handler.handle_request()
        connection.assert_not_called();handler.send_response.assert_called_once_with(409)
    def test_stale_verification_renews_using_native_bearer_before_pause(self):
        handler=self.handler('pause');profile={'id':'main-dot','active_root_thread_id':'main-root'}
        self.fixture[1].append(profile);self.fixture[2].update({'main-dot':self.main,'main-root':self.main})
        primary={'selection':{'available':True,'thread_id':'main-root'},'profile':profile}
        connection=MagicMock();response=connection.getresponse.return_value
        response.status=200;response.read.return_value=b'{"status":"applied","thread_id":"root"}';response.getheaders.return_value=[]
        with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.NATIVE_REBOOT_AUTH',None),patch('probe.catalog.get',return_value=primary) as get,patch('probe.http.client.HTTPSConnection',return_value=connection),patch('probe.event'):
            handler.handle_request()
        self.assertEqual(get.call_args.args[0]['token'],'native')
        self.assertEqual(get.call_args.args[0]['account_id'],'main')
        self.assertEqual(connection.request.call_count,1)
        handler.send_response.assert_called_once_with(200)
    def test_identity_renewal_failure_never_submits_control(self):
        for error in [ValueError('not verified'),TimeoutError()]:
            handler=self.handler('pause')
            with patch('probe.CATALOG_FIXTURE',self.fixture),patch('probe.NATIVE_REBOOT_AUTH',None),patch('probe.catalog.get',side_effect=error),patch('probe.http.client.HTTPSConnection') as connection,patch('probe.event'):
                handler.handle_request()
            connection.assert_not_called();handler.send_response.assert_called_once_with(409)

if __name__=='__main__':unittest.main()
