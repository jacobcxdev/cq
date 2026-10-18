import test_support
import io, socket, threading, types, unittest, urllib.parse
from unittest.mock import MagicMock, patch
import realtime_probe
from realtime_probe import signalling_owner, RESOURCES, Realtime

class SignallingBoundaryTests(unittest.TestCase):
    def url(self,thread,dot,extra=None):
        return '/v1/cua?'+urllib.parse.urlencode({'source':'aeon','thread_id':thread,'aeon_id':dot,**(extra or {})})

    def test_known_resource_pairs_pin_owner(self):
        for thread,(owner,dot) in RESOURCES.items():
            self.assertEqual(signalling_owner('POST',self.url(thread,dot),'application/sdp'),owner)

    def test_single_owner_and_duplicate_labels_do_not_change_resource_authority(self):
        for resources in [{'one-root':('11111111','one-profile')},
                          {'one-root':('11111111','one-profile'),'two-root':('33333333','two-profile')}]:
            with self.subTest(resources=resources), patch.dict(RESOURCES,resources,clear=True):
                for root,(alias,profile) in resources.items():
                    self.assertEqual(signalling_owner('POST',self.url(root,profile),'application/sdp'),alias)
                with self.assertRaises(ValueError):
                    signalling_owner('POST',self.url('unverified-root','one-profile'),'application/sdp')
                if len(resources)>1:
                    with self.assertRaises(ValueError):
                        signalling_owner('POST',self.url('one-root','two-profile'),'application/sdp')

    def test_cross_owner_pair_is_rejected(self):
        threads=list(RESOURCES)
        with self.assertRaises(ValueError):
            signalling_owner('POST',self.url(threads[0],RESOURCES[threads[1]][1]),'application/sdp')

    def test_unknown_missing_duplicate_or_unapproved_parameters_rejected(self):
        thread,(owner,dot)=next(iter(RESOURCES.items()))
        for path in [self.url('unknown',dot), '/v1/cua', self.url(thread,dot)+'&thread_id='+thread,
                     self.url(thread,dot,{'environment':'other'}),self.url(thread,dot).replace('/v1/cua','/v1/other')]:
            with self.assertRaises(ValueError): signalling_owner('POST',path,'application/sdp')
        for method,ctype in [('GET','application/sdp'),('POST','application/json')]:
            with self.assertRaises(ValueError): signalling_owner(method,self.url(thread,dot),ctype)

class NativeRealtimeTests(unittest.TestCase):
    def handler(self, method='GET', path='/v1/voice', body=b''):
        handler=Realtime.__new__(Realtime)
        handler.command=method;handler.path=path;handler.request_version='HTTP/1.1'
        handler.raw_requestline=f'{method} {path} HTTP/1.1\r\n'.encode()
        handler.headers={'Authorization':'Bearer desktop', 'Cookie':'native-cookie',
                         'ChatGPT-Account-Id':'primary', 'Content-Type':'application/json',
                         'Content-Length':str(len(body))}
        handler.rfile=io.BytesIO(body);handler.wfile=io.BytesIO();handler.connection=MagicMock()
        handler.server=MagicMock();handler.server.owners={}
        handler.send_response=MagicMock();handler.end_headers=MagicMock()
        return handler

    def test_normal_native_operations_bypass_dot_gate(self):
        for method in ['GET','POST','HEAD','PUT','PATCH','DELETE','OPTIONS','TRACE']:
            with self.subTest(method=method), patch.object(realtime_probe.settings,'INTEGRATION_MODE','native'), \
                 patch.object(realtime_probe.NATIVE_GATE,'require') as gate:
                handler=self.handler(method)
                with patch.object(handler,'native_passthrough') as forward:
                    getattr(handler,'do_'+method)()
                forward.assert_called_once_with();gate.assert_not_called()

    def test_unknown_dot_signalling_keeps_native_credentials(self):
        handler=self.handler('POST','/v1/cua?source=aeon&thread_id=unowned&aeon_id=unowned')
        handler.headers['Content-Type']='application/sdp'
        with patch.object(realtime_probe.settings,'INTEGRATION_MODE','native'), \
             patch.object(realtime_probe.NATIVE_GATE,'require') as gate, \
             patch.object(handler,'native_passthrough') as forward:
            handler.do_POST()
        forward.assert_called_once_with();gate.assert_not_called()

    def test_isolated_normal_requests_remain_blocked(self):
        with patch.object(realtime_probe.settings,'INTEGRATION_MODE','isolated'):
            for method in ['GET','POST','PUT']:
                handler=self.handler(method)
                with patch.object(handler,'native_passthrough') as forward, patch.object(realtime_probe,'event'):
                    getattr(handler,'do_'+method)()
                forward.assert_not_called();handler.send_response.assert_called_once_with(403)

    def test_known_dot_signalling_still_substitutes_only_verified_owner(self):
        root,(alias,profile)=next(iter(RESOURCES.items()))
        handler=self.handler('POST','/v1/cua?'+urllib.parse.urlencode(
            {'source':'aeon','thread_id':root,'aeon_id':profile}),b'v=0\r\n')
        handler.headers['Content-Type']='application/sdp'
        handler.server.owners={alias:{'token':'owner-token','account_id':'owner-account'}}
        handler.send_header=MagicMock()
        connection=MagicMock();response=connection.getresponse.return_value
        response.status=200;response.read.return_value=b'v=0\r\nanswer'
        response.getheaders.return_value=[]
        with patch.object(realtime_probe.settings,'INTEGRATION_MODE','native'), \
             patch.object(realtime_probe.NATIVE_GATE,'require') as gate, \
             patch.object(realtime_probe.http.client,'HTTPSConnection',return_value=connection), \
             patch.object(handler,'native_passthrough') as forward, patch.object(realtime_probe,'event'):
            handler.do_POST()
        forward.assert_not_called();self.assertTrue(gate.called)
        headers=connection.request.call_args.kwargs['headers']
        self.assertEqual(headers['Authorization'],'Bearer owner-token')
        self.assertEqual(headers['ChatGPT-Account-Id'],'owner-account')
        self.assertNotIn('Cookie',headers)
        self.assertEqual(handler.wfile.getvalue(),b'v=0\r\nanswer')

    def test_known_dot_signalling_rejects_invalid_native_identity(self):
        root,(_,profile)=next(iter(RESOURCES.items()))
        handler=self.handler('POST','/v1/cua?'+urllib.parse.urlencode(
            {'source':'aeon','thread_id':root,'aeon_id':profile}),b'v=0\r\n')
        handler.headers['Content-Type']='application/sdp'
        with patch.object(realtime_probe.settings,'INTEGRATION_MODE','native'), \
             patch.object(realtime_probe.NATIVE_GATE,'require',side_effect=ValueError('identity mismatch')), \
             patch.object(realtime_probe.http.client,'HTTPSConnection') as connection, \
             patch.object(handler,'native_passthrough') as forward, patch.object(realtime_probe,'event'):
            handler.do_POST()
        forward.assert_not_called();connection.assert_not_called()
        handler.send_response.assert_called_once_with(403)

    def test_native_chunked_http_stream_preserves_body_and_response(self):
        client,proxy=socket.socketpair();upstream,backend=socket.socketpair()
        self.addCleanup(client.close);self.addCleanup(proxy.close)
        self.addCleanup(upstream.close);self.addCleanup(backend.close)
        client.settimeout(2);backend.settimeout(2)
        tls=MagicMock();tls.wrap_socket.return_value=upstream
        server=types.SimpleNamespace(public_tls=tls,owners={})
        request=(b'POST /v1/voice HTTP/1.1\r\nHost: realtime.chatgpt.com\r\n'
                 b'Authorization: Bearer desktop\r\nCookie: primary=value\r\n'
                 b'Transfer-Encoding: chunked\r\nConnection: keep-alive\r\n\r\n'
                 b'4\r\nbody\r\n0\r\nX-Trailer: value\r\n\r\n')
        response=(b'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n'
                  b'Set-Cookie: updated=value\r\n\r\n4\r\nlive\r\n0\r\n\r\n')
        with patch.object(realtime_probe.settings,'INTEGRATION_MODE','native'), \
             patch.object(realtime_probe.socket,'create_connection',return_value=upstream), \
             patch.object(realtime_probe.NATIVE_GATE,'require') as gate:
            worker=threading.Thread(target=Realtime,args=(proxy,('127.0.0.1',1),server),daemon=True)
            worker.start();client.sendall(request)
            received=b''
            while not received.endswith(b'X-Trailer: value\r\n\r\n'):
                received+=backend.recv(65536)
            self.assertIn(b'Authorization: Bearer desktop\r\n',received)
            self.assertIn(b'Cookie: primary=value\r\n',received)
            self.assertIn(b'Connection: close\r\n',received)
            self.assertTrue(received.endswith(request.split(b'\r\n\r\n',1)[1]))
            backend.sendall(response);backend.shutdown(socket.SHUT_WR)
            result=b''
            while chunk:=client.recv(65536):result+=chunk
            worker.join(2)
        self.assertFalse(worker.is_alive());gate.assert_not_called()
        self.assertEqual(result,response)

    def test_native_passthrough_streams_upgrade_and_original_headers(self):
        handler=self.handler('GET','/v1/realtime?model=voice')
        handler.headers.update({'Connection':'Upgrade','Upgrade':'websocket','Sec-WebSocket-Key':'native-key'})
        handler.rfile=io.BytesIO(b'client-frame')
        upstream=MagicMock()
        sent=threading.Event()
        def send(data):
            if data==b'client-frame':sent.set()
        upstream.sendall.side_effect=send
        response=b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nserver-frame'
        upstream.recv.side_effect=[response,b'']
        handler.server.public_tls.wrap_socket.return_value=upstream
        with patch.object(realtime_probe.socket,'create_connection') as connect:
            handler.native_passthrough()
        connect.assert_called_once_with(('realtime.chatgpt.com',443),timeout=25)
        request=upstream.sendall.call_args_list[0].args[0]
        for header in [b'Authorization: Bearer desktop\r\n',b'Cookie: native-cookie\r\n',
                       b'ChatGPT-Account-Id: primary\r\n',b'Connection: Upgrade\r\n',
                       b'Upgrade: websocket\r\n',b'Sec-WebSocket-Key: native-key\r\n']:
            self.assertIn(header,request)
        self.assertTrue(request.startswith(handler.raw_requestline))
        self.assertTrue(sent.wait(1));self.assertEqual(handler.wfile.getvalue(),response)
        self.assertTrue(handler.close_connection)

if __name__=='__main__': unittest.main()
