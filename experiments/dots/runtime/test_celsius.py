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
