import test_support
import copy,json,queue,time,unittest
from unittest.mock import Mock,patch
from celsius import Broker,targets,secondary_frame,PRIMARY,SECONDARY

class CelsiusTests(unittest.TestCase):
    def setUp(self):
        self.owners={'main-room':{'alias':PRIMARY},'worker-room':{'alias':SECONDARY},
                     'main-thread':{'alias':PRIMARY}}
        self.frame={'type':'message','topic_id':'calpico-chatgpt-messaging','offset':'worker-offset',
                    'payload':{'type':'calpico-message-add','payload':{'room_id':'worker-room',
                    'message':{'id':'message','account_user_id':'worker-user','content':{'text':'example'}}}}}

    def test_room_topic_routes_only_to_owner(self):
        self.assertEqual(targets({'type':'subscribe','topic_id':'calpico-room-live:worker-room'},self.owners),[SECONDARY])
        self.assertEqual(targets({'type':'subscribe','topic_id':'calpico-room-live:main-room'},self.owners),[PRIMARY])
        with self.assertRaises(ValueError):targets({'type':'subscribe','topic_id':'calpico-room-live:unknown'},self.owners)
        self.assertEqual(targets({'type':'subscribe','topic_id':'conversations'},self.owners),[PRIMARY])
        self.assertEqual(targets({'type':'subscribe','topic_id':'calpico-chatgpt-messaging'},self.owners),[PRIMARY,SECONDARY])

    def test_secondary_preserves_message_and_primary_cursor(self):
        original=copy.deepcopy(self.frame)
        result=secondary_frame(self.frame,self.owners,{'worker-user'})
        self.assertNotIn('offset',result)
        self.assertTrue(result['payload']['payload']['message']['is_self'])
        self.assertEqual(result['payload']['payload']['message']['content'],original['payload']['payload']['message']['content'])
        self.assertEqual(self.frame,original)

    def test_unknown_or_cross_owner_events_reject(self):
        for room in ['unknown','main-room']:
            frame=copy.deepcopy(self.frame);frame['payload']['payload']['room_id']=room
            self.assertIsNone(secondary_frame(frame,self.owners,{'worker-user'}))
        frame=copy.deepcopy(self.frame);frame['topic_id']='conversations'
        self.assertIsNone(secondary_frame(frame,self.owners,{'worker-user'}))
        frame=copy.deepcopy(self.frame);frame['payload']['payload']['thread_id']='main-thread'
        self.assertIsNone(secondary_frame(frame,self.owners,{'worker-user'}))
        self.assertIsNone(secondary_frame({'id':1,'reply':{}},self.owners,{'worker-user'}))

    def test_single_owner_shared_topics_do_not_require_secondary(self):
        owners={'main-room':{'alias':PRIMARY}}
        for command in [{'type':'connect'},{'type':'presence'},
                        {'type':'subscribe','topic_id':'calpico-chatgpt-messaging'}]:
            self.assertEqual(targets(command,owners),[PRIMARY])

    def test_third_owner_frames_are_pinned_to_source_and_project_its_viewer(self):
        third='33333333'
        owners={**self.owners,'third-room':{'alias':third},'third-thread':{'alias':third}}
        frame=copy.deepcopy(self.frame)
        frame['payload']['payload']['room_id']='third-room'
        frame['payload']['payload']['message']['account_user_id']='third-user'
        result=secondary_frame(frame,owners,{'third-user'},third)
        self.assertTrue(result['payload']['payload']['message']['is_self'])
        self.assertNotIn('offset',result)
        self.assertIsNone(secondary_frame(frame,owners,{'worker-user'},SECONDARY))
        frame['payload']['payload']['thread_id']='worker-room'
        self.assertIsNone(secondary_frame(frame,owners,{'third-user'},third))

    def test_broker_grants_cover_exact_configured_owners_for_single_and_duplicate_labels(self):
        for aliases in [[PRIMARY],[PRIMARY,SECONDARY,'33333333']]:
            with self.subTest(aliases=aliases):
                owners={str(index):{'alias':alias} for index,alias in enumerate(aliases)}
                urls={alias:'wss://ws.chatgpt.com/events?owner='+alias for alias in aliases}
                broker=Broker.__new__(Broker)
                broker.lock=__import__('threading').Lock();broker.grants={}
                handles={'dot-'+str(index):alias for index,alias in enumerate(aliases)}
                with patch('celsius.settings.DOT_OWNERS',handles), \
                     patch('celsius.settings.DOT_NAMES',{handle:'Dot' for handle in handles},create=True):
                    issued=broker.issue(urls,owners)
                    self.assertEqual(broker.grants[issued.removeprefix('wss://ws.chatgpt.com')][1],urls)
                    with self.assertRaises(ValueError):broker.issue({**urls,'unconfigured':'wss://ws.chatgpt.com/events'},owners)
                    if len(aliases)>1:
                        with self.assertRaises(ValueError):broker.issue({PRIMARY:urls[PRIMARY]},owners)

    def test_shared_subscription_ack_and_offsets_cover_each_configured_owner(self):
        for aliases in [[PRIMARY],[PRIMARY,SECONDARY,'33333333']]:
            with self.subTest(aliases=aliases):
                aliases=[PRIMARY,*sorted(set(aliases)-{PRIMARY})]
                broker=Broker.__new__(Broker)
                broker.lock=__import__('threading').Lock();broker.public_tls=None
                owners={alias+'-room':{'alias':alias} for alias in aliases}
                urls={alias:'wss://ws.chatgpt.com/'+alias for alias in aliases}
                broker.grants={'/cq-owner-events/test':(time.monotonic()+60,urls,owners)}
                records=[];broker.event=records.append
                sockets={alias:Mock() for alias in aliases}
                client=Mock();client.request.path='/cq-owner-events/test'
                command={'type':'subscribe','topic_id':'calpico-chatgpt-messaging','offset':'primary-offset'}
                messages=[('client',json.dumps([{'id':99,'command':command}]))]
                for identifier,alias in enumerate(aliases,1):
                    messages.append((alias,json.dumps([{'id':identifier,'reply':{'type':'ok','recovered':True,
                                                                             'catchups':['primary-catchup']}}])))
                messages.append(('client',None))
                incoming=Mock();incoming.get.side_effect=messages
                with patch('celsius.queue.Queue',return_value=incoming), \
                     patch('celsius.connect',side_effect=lambda url,**kw:sockets[url.rsplit('/',1)[1]]), \
                     patch('celsius.threading.Thread'):
                    broker.handle(client)
                for alias,socket in sockets.items():
                    sent=json.loads(socket.send.call_args.args[0])[0]['command']
                    self.assertEqual(sent.get('offset'),'primary-offset' if alias==PRIMARY else None)
                ack=json.loads(client.send.call_args.args[0])
                self.assertEqual(ack[0]['id'],99)
                self.assertEqual(ack[0]['reply']['recovered'],len(aliases)==1)
                self.assertEqual(ack[0]['reply']['catchups'],['primary-catchup'] if len(aliases)==1 else [])
                self.assertFalse(any('celsius_broker_error_type' in event for event in records))

    def test_idle_queue_timeout_keeps_broker_connected(self):
        broker=Broker.__new__(Broker)
        broker.lock=__import__('threading').Lock()
        broker.grants={'/cq-owner-events/test':(time.monotonic()+60,
                       {PRIMARY:'main',SECONDARY:'worker'},self.owners)}
        broker.public_tls=None
        records=[];broker.event=records.append
        client=Mock();client.request.path='/cq-owner-events/test'
        incoming=Mock();incoming.get.side_effect=[queue.Empty,('client',None)]
        with patch('celsius.queue.Queue',return_value=incoming), \
             patch('celsius.connect',side_effect=[Mock(),Mock()]), \
             patch('celsius.threading.Thread'):
            broker.handle(client)
        self.assertEqual(incoming.get.call_count,2)
        self.assertFalse(any('celsius_broker_error_type' in event for event in records))

if __name__ == '__main__':unittest.main()
