import test_support
import collections,copy,json,queue,time,unittest
from unittest.mock import Mock,patch
from celsius import Broker,targets,secondary_frame,PRIMARY,SECONDARY

class CelsiusTests(unittest.TestCase):
    def setUp(self):
        self.owners={'main-room':{'alias':PRIMARY},'worker-room':{'alias':SECONDARY},
                     'main-thread':{'alias':PRIMARY}}
        self.frame={'type':'message','topic_id':'calpico-chatgpt-messaging','offset':'worker-offset',
                    'payload':{'type':'calpico-message-add','payload':{'room_id':'worker-room',
                    'message':{'id':'message','account_user_id':'worker-user','content':{'text':'example'}}}}}

    def handle_scenario(self,steps,mode='native',secondary_error=None,secondary_send_error=None):
        broker=Broker.__new__(Broker)
        broker.lock=__import__('threading').Lock();broker.public_tls=None
        broker.grants={'/test':(1000,{SECONDARY:'worker',PRIMARY:'main'},self.owners)}
        state={'events':[],'primary':Mock(),'secondary':Mock(),'client':Mock(),
               'connectors':[],'queued':collections.deque(),'now':100,'reads':0}
        state['secondary'].send.side_effect=secondary_send_error
        broker.event=state['events'].append
        state['client'].request.path='/test'
        actions=collections.deque(steps)
        def get(**kwargs):
            state['reads']+=1
            if state['reads']>1000:raise AssertionError('Broker did not finish')
            while not state['queued']:
                if not actions:return ('client',None)
                action=actions.popleft()
                if callable(action):
                    value=action(state)
                    if value is not None:state['queued'].append(value)
                else:state['queued'].append(action)
            return state['queued'].popleft()
        incoming=Mock();incoming.get.side_effect=get
        incoming.put.side_effect=lambda value,**kwargs:state['queued'].append(value)
        def thread(*,target,args=(),**kwargs):
            result=Mock()
            if target.__name__=='connect_secondary':
                result.start.side_effect=lambda:state['connectors'].append((target,args))
            return result
        def connect(url,**kwargs):
            if url=='main':return state['primary']
            if secondary_error is not None:raise secondary_error
            return state['secondary']
        with patch('celsius.settings.INTEGRATION_MODE',mode), \
             patch('celsius.time.monotonic',side_effect=lambda:state['now']), \
             patch('celsius.queue.Queue',return_value=incoming), \
             patch('celsius.connect',side_effect=connect),patch('celsius.threading.Thread',side_effect=thread):
            broker.handle(state['client'])
        state['frames']=[frame for call in state['client'].send.call_args_list for frame in json.loads(call.args[0])]
        return state

    def start_secondary(self,state):
        target,args=state['connectors'].pop()
        target(*args)

    def command(self,identifier,kind='subscribe',topic='calpico-chatgpt-messaging'):
        value={'type':kind}
        if kind=='subscribe':value['topic_id']=topic
        return ('client',json.dumps([{'id':identifier,'command':value}]))

    def primary_ack(self,identifier):
        return (PRIMARY,json.dumps([{'id':identifier,'reply':{'type':'subscribe',
                'topic_id':'calpico-chatgpt-messaging','recovered':True,'catchups':[]}}]))

    def test_native_secondary_connect_failure_does_not_interrupt_primary(self):
        marker={'type':'message','topic_id':'conversations','payload':{'type':'primary-update'}}
        state=self.handle_scenario([self.command(91),self.primary_ack(1),self.start_secondary,
                                    (PRIMARY,json.dumps([marker]))],secondary_error=OSError('offline'))
        self.assertEqual([frame.get('id') for frame in state['frames']],[91,None])
        self.assertEqual(state['frames'][-1],marker)
        self.assertTrue(any(event.get('celsius_owner_unavailable')==SECONDARY for event in state['events']))
        self.assertFalse(any('celsius_broker_error_type' in event for event in state['events']))

    def test_native_secondary_eof_fails_only_its_own_subscription(self):
        marker={'type':'message','topic_id':'conversations','payload':{'type':'primary-update'}}
        state=self.handle_scenario([self.start_secondary,self.command(91),
                                    self.command(92,topic='calpico-room-live:worker-room'),
                                    (SECONDARY,None),self.primary_ack(1),(PRIMARY,json.dumps([marker]))])
        replies={frame['id']:frame['reply'] for frame in state['frames'] if 'id' in frame}
        self.assertEqual(replies[92],{'type':'error','code':'cq_owner_unavailable'})
        self.assertEqual(replies[91]['type'],'subscribe')
        self.assertEqual(state['frames'][-1],marker)
        self.assertFalse(any('celsius_broker_error_type' in event for event in state['events']))

    def test_native_primary_connect_and_subscribe_ack_do_not_wait_for_secondary(self):
        def primary_already_acknowledged(state):
            replies=[frame for call in state['client'].send.call_args_list for frame in json.loads(call.args[0])]
            self.assertEqual([frame['id'] for frame in replies],[91,92])
            state['primary'].close.assert_not_called()
            self.start_secondary(state)
        state=self.handle_scenario([self.command(91,'connect'),self.command(92),
                                    self.primary_ack(1),self.primary_ack(3),primary_already_acknowledged])
        self.assertEqual([frame['id'] for frame in state['frames']],[91,92])
        self.assertFalse(state['frames'][1]['reply']['recovered'])
        commands=[json.loads(call.args[0])[0]['command']['type'] for call in state['secondary'].send.call_args_list]
        self.assertEqual(commands,['connect','subscribe'])

    def test_native_never_acknowledging_secondary_does_not_hold_primary_ack(self):
        state=self.handle_scenario([self.start_secondary,self.command(91,'connect'),self.command(92),
                                    self.primary_ack(1),self.primary_ack(3)])
        self.assertEqual([frame['id'] for frame in state['frames']],[91,92])
        self.assertEqual(state['secondary'].send.call_count,2)
        self.assertFalse(any('celsius_broker_error_type' in event for event in state['events']))

    def test_native_pending_secondary_scoped_command_is_sent_after_connect(self):
        state=self.handle_scenario([self.command(91,topic='calpico-room-live:worker-room'),
                                    self.start_secondary,(SECONDARY,json.dumps([{'id':1,'reply':{'type':'ok'}}]))])
        sent=json.loads(state['secondary'].send.call_args.args[0])[0]
        self.assertEqual(sent['command']['topic_id'],'calpico-room-live:worker-room')
        self.assertEqual(state['frames'],[{'id':91,'reply':{'type':'ok'}}])

    def test_native_secondary_ack_timeout_preserves_primary(self):
        marker={'type':'message','topic_id':'conversations','payload':{'type':'primary-update'}}
        def timeout(state):
            state['now']=121
            raise queue.Empty
        state=self.handle_scenario([self.start_secondary,
                                    self.command(91,topic='calpico-room-live:worker-room'),
                                    timeout,(PRIMARY,json.dumps([marker]))])
        self.assertEqual(state['frames'][0],{'id':91,'reply':{'type':'error','code':'cq_owner_unavailable'}})
        self.assertEqual(state['frames'][-1],marker)

    def test_native_secondary_send_failure_does_not_hold_primary_ack(self):
        state=self.handle_scenario([self.start_secondary,self.command(91),self.primary_ack(1)],
                                    secondary_send_error=OSError('offline'))
        self.assertEqual([frame['id'] for frame in state['frames']],[91])
        self.assertTrue(any(event.get('celsius_owner_unavailable')==SECONDARY for event in state['events']))
        self.assertFalse(any('celsius_broker_error_type' in event for event in state['events']))

    def test_native_secondary_startup_queue_is_bounded(self):
        commands=[{'id':identifier,'command':{'type':'subscribe','topic_id':'calpico-room-live:worker-room'}}
                  for identifier in range(1,129)]
        marker={'type':'message','topic_id':'conversations','payload':{'type':'primary-update'}}
        state=self.handle_scenario([('client',json.dumps(commands)),
                                    self.command(129,topic='calpico-room-live:worker-room'),
                                    (PRIMARY,json.dumps([marker]))])
        replies=[frame for frame in state['frames'] if 'id' in frame]
        self.assertEqual({frame['id'] for frame in replies},set(range(1,130)))
        self.assertTrue(all(frame['reply']['code']=='cq_owner_unavailable' for frame in replies))
        self.assertEqual(state['frames'][-1],marker)
        state['secondary'].send.assert_not_called()

    def test_isolated_secondary_failure_and_eof_remain_strict(self):
        for failure,steps in [(OSError('offline'),[]),(None,[(SECONDARY,None),self.command(91)])]:
            with self.subTest(failure=failure):
                state=self.handle_scenario(steps,mode='isolated',secondary_error=failure)
                self.assertEqual(state['frames'],[])
                state['client'].close.assert_called_once()

    def test_isolated_primary_ack_still_waits_for_secondary(self):
        state=self.handle_scenario([self.command(91),self.primary_ack(2)],mode='isolated')
        self.assertEqual(state['frames'],[])

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
