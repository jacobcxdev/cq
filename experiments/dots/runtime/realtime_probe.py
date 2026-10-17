"""Task-only Chromium proxy. Only known Dot view signalling is decrypted/routed.

All other accepted CONNECT traffic remains an opaque tunnel. No SDP, ICE,
cookies, auth values, or response bodies are logged or persisted.
"""
import http.client, http.server, json, re, select, socket, socketserver, ssl, threading, time, urllib.parse
import catalog
import settings
from native_gate import NativeGate
NATIVE_GATE=NativeGate()
ROOT = settings.ROOT
LOCK = threading.Lock()
RESOURCE_LOCK=threading.Lock()
RESOURCES = {root: (settings.DOT_OWNERS[name], profile)
             for name, (profile, root) in settings.DOTS.items()}

def event(value):
    with LOCK:
        with (ROOT/'realtime-events.jsonl').open('a') as f:
            f.write(json.dumps({'at':time.time(),**value})+'\n')

def signalling_owner(method, path, content_type, accounts=None):
    parsed=urllib.parse.urlsplit(path)
    query=urllib.parse.parse_qs(parsed.query)
    if method!='POST' or parsed.path!='/v1/cua' or parsed.netloc or content_type!='application/sdp':
        raise ValueError('Not the known native view signalling operation')
    if set(query)!={'source','thread_id','aeon_id'} or any(len(v)!=1 for v in query.values()) or query['source']!=['aeon']:
        raise ValueError('Invalid signalling resource scope')
    with RESOURCE_LOCK:owner=RESOURCES.get(query['thread_id'][0])
    if owner is None and accounts is not None and query['aeon_id'][0] in catalog.PROOF_PROFILE_IDS.values():
        roots=catalog.primary_roots(accounts,{'User-Agent':f'Codex Desktop/{settings.APP_VERSION} (Mac OS; arm64)','originator':'codex_desktop'})
        with RESOURCE_LOCK:
            if any(root in RESOURCES and RESOURCES[root]!=pair for root,pair in roots.items()):
                raise ValueError('Signalling resource owner changed')
            RESOURCES.update(roots)
            owner=RESOURCES.get(query['thread_id'][0])
    if owner is None or query['aeon_id']!=[owner[1]]:
        raise ValueError('Unknown or conflicting signalling resource owner')
    return owner[0]

class Realtime(http.server.BaseHTTPRequestHandler):
    def log_message(self,*args): pass
    def do_GET(self): self.reject()
    def reject(self):
        self.send_response(403); self.end_headers()
    def do_POST(self):
        try:
            NATIVE_GATE.require(dict(self.headers.items()))
            alias=signalling_owner(self.command,self.path,self.headers.get('Content-Type',''),self.server.owners)
            size=int(self.headers.get('Content-Length','0'))
            if self.headers.get('Transfer-Encoding') or size<1 or size>1024*1024:
                raise ValueError('Unsupported or unbounded signalling body')
        except Exception as error:
            event({'signalling_rejected':True,'error_type':type(error).__name__}); self.reject(); return
        body=self.rfile.read(size)
        if len(body)!=size:
            event({'signalling_truncated':True}); self.reject(); return
        owner=self.server.owners[alias]
        headers={k:v for k,v in self.headers.items() if k.lower() not in {
            'host','connection','content-length','accept-encoding','authorization','cookie',
            'chatgpt-account-id','x-openai-account-routing-override','x-openai-expected-account-id'}}
        headers.update({'Authorization':'Bearer '+owner['token'],
                        'ChatGPT-Account-Id':owner['account_id'],'Accept-Encoding':'identity'})
        upstream=http.client.HTTPSConnection('realtime.chatgpt.com',context=self.server.public_tls,timeout=25)
        try:
            upstream.request('POST',self.path,body=body,headers=headers)
            response=upstream.getresponse(); data=response.read(2*1024*1024+1)
            if len(data)>2*1024*1024: raise ValueError('Signalling response exceeds bound')
            event({'owner_alias':alias,'signalling_status':response.status,
                   'content_type':response.getheader('Content-Type'),
                   'sdp_answer':200<=response.status<300 and data.startswith(b'v=0')})
            self.send_response(response.status)
            for k,v in response.getheaders():
                if k.lower() not in {'content-length','transfer-encoding','content-encoding','connection','set-cookie'}:
                    self.send_header(k,v)
            self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)
        except (BrokenPipeError,ConnectionResetError): pass
        except Exception as error:
            event({'owner_alias':alias,'signalling_failure_type':type(error).__name__})
            self.send_response(502); self.end_headers()
        finally: upstream.close()

class Connect(socketserver.StreamRequestHandler):
    def handle(self):
        try:
            line=self.rfile.readline(8193).decode('ascii').strip().split(' ')
            if len(line)!=3 or line[0]!='CONNECT':
                self.wfile.write(b'HTTP/1.1 405 Method Not Allowed\r\n\r\n'); return
            while self.rfile.readline(8193) not in (b'\r\n',b'\n',b''): pass
            host,_,port=line[1].rpartition(':')
            if port!='443' or not re.fullmatch(r'[A-Za-z0-9.-]+',host) or host in {'localhost','127.0.0.1'}:
                event({'connect_rejected':True}); self.wfile.write(b'HTTP/1.1 403 Forbidden\r\n\r\n'); return
            if host=='ws.chatgpt.com':
                self.wfile.write(b'HTTP/1.1 200 Connection Established\r\n\r\n'); self.wfile.flush()
                wrapped=self.server.signalling_tls.wrap_socket(self.connection,server_side=True)
                upstream=None
                try:
                    request=b''
                    while b'\r\n\r\n' not in request:
                        chunk=wrapped.recv(4096)
                        if not chunk:return
                        request+=chunk
                        if len(request)>65536:raise ValueError('WebSocket handshake exceeds bound')
                    request_line=request.split(b'\r\n',1)[0].decode('ascii').split(' ')
                    if len(request_line)!=3:raise ValueError('Invalid WebSocket handshake')
                    if request_line[0]=='GET' and re.fullmatch(r'/cq-owner-events/[A-Za-z0-9_-]{43}',request_line[1]):
                        upstream=socket.create_connection(('127.0.0.1',int((ROOT/'celsius-port').read_text())),timeout=20)
                        event({'celsius_native_proxy_connection':True})
                    else:
                        raw=socket.create_connection(('ws.chatgpt.com',443),timeout=20)
                        upstream=self.server.public_tls.wrap_socket(raw,server_hostname='ws.chatgpt.com')
                    upstream.sendall(request)
                    wrapped.settimeout(None);upstream.settimeout(None)
                    def copy_stream(source,destination):
                        try:
                            while True:
                                data=source.recv(65536)
                                if not data:break
                                destination.sendall(data)
                        except (OSError,ssl.SSLError):pass
                        finally:
                            try:destination.shutdown(socket.SHUT_WR)
                            except OSError:pass
                    thread=threading.Thread(target=copy_stream,args=(wrapped,upstream),daemon=True)
                    thread.start();copy_stream(upstream,wrapped)
                finally:
                    if upstream is not None:upstream.close()
                    wrapped.close()
            elif host=='realtime.chatgpt.com':
                self.wfile.write(b'HTTP/1.1 200 Connection Established\r\n\r\n'); self.wfile.flush()
                wrapped=self.server.signalling_tls.wrap_socket(self.connection,server_side=True)
                try: Realtime(wrapped,self.client_address,self.server)
                finally: wrapped.close()
            else:
                upstream=socket.create_connection((host,443),timeout=20)
                self.wfile.write(b'HTTP/1.1 200 Connection Established\r\n\r\n'); self.wfile.flush()
                self.connection.settimeout(None); upstream.settimeout(None)
                event({'opaque_tunnel_host':host if host=='chatgpt.com' or host.endswith('.chatgpt.com') else 'other'})
                try:
                    while True:
                        for source in select.select([self.connection,upstream],[],[],60)[0]:
                            data=source.recv(65536)
                            if not data: return
                            (upstream if source is self.connection else self.connection).sendall(data)
                finally: upstream.close()
        except Exception as error:
            event({'connect_failure_type':type(error).__name__})

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address=True
    daemon_threads=True

if __name__=='__main__':
    _,accounts=catalog.inventory()
    aliases=set(settings.DOT_OWNERS.values())|{settings.SYSTEM_ALIAS}
    owners={a['alias']:a for a in accounts if a['alias'] in aliases}
    if set(owners)!=aliases: raise ValueError('Configured signalling principals unavailable')
    port_file=ROOT/'realtime-port'
    server=Server(('127.0.0.1',int(port_file.read_text()) if port_file.exists() else 0),Connect)
    server.owners=owners
    server.public_tls=ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)
    server.signalling_tls=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    server.signalling_tls.load_cert_chain(ROOT/'realtime-cert.pem',ROOT/'leaf-key.pem')
    (ROOT/'realtime-port').write_text(str(server.server_address[1]))
    event({'listening_port':server.server_address[1]})
    server.serve_forever()
