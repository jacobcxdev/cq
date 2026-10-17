"""Task-local owner-pinned Celsius fan-out. Credentials and signed URLs stay in memory."""
import copy, json, queue, secrets, ssl, threading, time, urllib.parse
from websockets.sync.client import connect
from websockets.sync.server import serve
import self_projection
import settings

ROOT = settings.ROOT
PRIMARY = settings.SYSTEM_ALIAS
SECONDARY = settings.SECONDARY_ALIAS
TOPICS = {'calpico-chatgpt-messaging', 'calpico-chatgpt-messaging-all'}
EVENT_TYPES = {'calpico-room-read-receipt','calpico-is-responding-heartbeat',
               'calpico-message-update','calpico-room-update','calpico-message-add',
               'calpico-room-read','calpico-room-metadata-update','calpico-room-leave','calpico-room-live'}

def owner_aliases(owners):
    aliases={owner['alias'] for owner in owners.values()}
    return [PRIMARY,*sorted(aliases-{PRIMARY})]

def targets(command, owners):
    kind = command.get('type')
    if kind in {'connect','presence'}:
        return owner_aliases(owners)
    topic = command.get('topic_id')
    if kind in {'subscribe','unsubscribe'} and topic in TOPICS:
        return owner_aliases(owners)
    if kind in {'subscribe','unsubscribe'} and isinstance(topic,str) and topic.startswith('calpico-room-live:'):
        owner=owners.get(topic.removeprefix('calpico-room-live:'))
        if owner is None: raise ValueError('Unknown live room')
        return [owner['alias']]
    return [PRIMARY]

def event_room(payload):
    if not isinstance(payload,dict): return None
    room=payload.get('room_id')
    if room is None and isinstance(payload.get('room'),dict):room=payload['room'].get('id')
    return room

def secondary_frame(frame, owners, viewer_ids, source=None):
    source=SECONDARY if source is None else source
    if source==PRIMARY: return None
    if not isinstance(frame,dict) or frame.get('type')!='message': return None
    payload=frame.get('payload')
    if not isinstance(payload,dict) or payload.get('type') not in EVENT_TYPES: return None
    room=event_room(payload.get('payload'))
    owner=owners.get(room) if isinstance(room,str) else None
    if owner is None or owner['alias']!=source: return None
    if frame.get('topic_id') not in TOPICS | {'calpico-room-live:'+room}: return None
    def conflicting(value):
        if isinstance(value,dict):
            for key,item in value.items():
                if key in {'room_id','thread_id','aeon_id'} and isinstance(item,str):
                    found=owners.get(item)
                    if found is not None and found['alias']!=source:return True
                if conflicting(item):return True
        elif isinstance(value,list):return any(conflicting(item) for item in value)
        return False
    if conflicting(payload):return None
    result=copy.deepcopy(frame)
    result.pop('offset',None)  # Owner offsets are independent; never overwrite primary cursor.
    value=result['payload']['payload']
    if isinstance(value.get('message'),dict):self_projection.project_message(value['message'],viewer_ids)
    if isinstance(value.get('room'),dict):self_projection.project_envelope(value['room'],viewer_ids)
    return result

class Broker:
    def __init__(self,event):
        self.event=event
        self.lock=threading.Lock()
        self.grants={}
        context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(ROOT/'leaf-cert.pem',ROOT/'leaf-key.pem')
        self.public_tls=ssl.create_default_context(cafile=ssl.get_default_verify_paths().openssl_cafile)
        self.server=serve(self.handle,'127.0.0.1',0,max_size=2*1024*1024,
                          compression=None,open_timeout=20,close_timeout=3)
        self.port=self.server.socket.getsockname()[1]
        (ROOT/'celsius-port').write_text(str(self.port))
        threading.Thread(target=self.server.serve_forever,daemon=True).start()
        self.event({'celsius_broker_port':self.port})

    def issue(self,urls,owners):
        aliases=set(owner_aliases(owners))
        if aliases!=set(settings.DOT_OWNERS.values())|{PRIMARY} or set(urls)!=aliases:
            raise ValueError('Configured owner URLs required')
        for url in urls.values():
            parsed=urllib.parse.urlsplit(url)
            if parsed.scheme!='wss' or parsed.hostname!='ws.chatgpt.com' or parsed.port not in {None,443}:
                raise ValueError('Unexpected Celsius service origin')
        token=secrets.token_urlsafe(32)
        with self.lock:
            now=time.monotonic()
            self.grants={k:v for k,v in self.grants.items() if v[0]>now}
            if len(self.grants)>=16:raise ValueError('Celsius URL grant limit')
            self.grants['/cq-owner-events/'+token]=(now+1500,dict(urls),dict(owners))
        return 'wss://ws.chatgpt.com/cq-owner-events/'+token

    def handle(self,client):
        with self.lock:
            grant=self.grants.get(client.request.path)
        if grant is None or grant[0]<=time.monotonic():
            client.close(1008,'Unknown or expired local session');return
        _,urls,owners=grant
        sockets={}
        stopped=threading.Event()
        incoming=queue.Queue(maxsize=512)
        pending={}
        awaiting={}
        next_id=1
        def receive(source,socket):
            try:
                for data in socket:
                    if stopped.is_set():break
                    incoming.put((source,data),timeout=5)
            except Exception as error:
                self.event({'celsius_receive_error_type':type(error).__name__,'source':source})
            finally:
                try:incoming.put((source,None),timeout=5)
                except queue.Full:pass
        try:
            for alias,url in urls.items():
                sockets[alias]=connect(url,ssl=self.public_tls,proxy=None,compression=None,legacy=True,
                    max_size=2*1024*1024,open_timeout=20,close_timeout=3)
                self.event({'celsius_owner_connected':alias,'upgrade_status':101})
                threading.Thread(target=receive,args=(alias,sockets[alias]),daemon=True).start()
            threading.Thread(target=receive,args=('client',client),daemon=True).start()
            while not stopped.is_set():
                try:
                    source,data=incoming.get(timeout=180)
                except queue.Empty:
                    continue
                if data is None:break
                value=json.loads(data)
                if source=='client':
                    if not isinstance(value,list) or len(value)>128:raise ValueError('Invalid native command batch')
                    for item in value:
                        if not isinstance(item,dict) or not isinstance(item.get('command'),dict):raise ValueError('Invalid native command')
                        native_id=item.get('id')
                        if not isinstance(native_id,int) or native_id in awaiting:raise ValueError('Duplicate native command ID')
                        command=item['command']; destinations=targets(command,owners)
                        awaiting[native_id]={'remaining':set(destinations),'replies':{},'command':command}
                        for alias in destinations:
                            upstream_command=copy.deepcopy(command)
                            if alias!=PRIMARY:upstream_command.pop('offset',None)
                            pending[(alias,next_id)]=native_id
                            sockets[alias].send(json.dumps([{'id':next_id,'command':upstream_command}]))
                            next_id+=1
                elif isinstance(value,list):
                    forwarded=[]
                    for frame in value:
                        if not isinstance(frame,dict):continue
                        if 'reply' in frame:
                            native_id=pending.pop((source,frame.get('id')),None)
                            if native_id is None:continue
                            state=awaiting[native_id]
                            state['remaining'].discard(source);state['replies'][source]=frame['reply']
                            if not state['remaining']:
                                reply=copy.deepcopy(state['replies'].get(PRIMARY,next(iter(state['replies'].values()))))
                                if state['command'].get('type')=='subscribe' and len(state['replies'])>1 and isinstance(reply,dict):
                                    reply['recovered']=False;reply['catchups']=[]
                                forwarded.append({'id':native_id,'reply':reply})
                                self.event({'celsius_command_ack':state['command'].get('type'),
                                            'owner_count':len(state['replies']),
                                            'reply_type':reply.get('type') if isinstance(reply,dict) else None})
                                del awaiting[native_id]
                        elif source==PRIMARY:
                            forwarded.append(frame)
                        else:
                            projected=secondary_frame(frame,owners,next(v['viewer_ids'] for v in owners.values() if v['alias']==source),source)
                            if projected is not None:
                                forwarded.append(projected)
                                self.event({'celsius_secondary_event_type':projected['payload'].get('type')})
                    if forwarded:client.send(json.dumps(forwarded))
                elif source==PRIMARY:
                    client.send(data)
        except Exception as error:
            self.event({'celsius_broker_error_type':type(error).__name__})
        finally:
            stopped.set()
            for socket in sockets.values():socket.close()
            client.close()
