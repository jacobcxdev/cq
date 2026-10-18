import test_support
import io, json, pathlib, queue, subprocess, sys, tempfile, threading, types, unittest
from unittest.mock import patch
import cloud_stdio
from cloud_stdio import select_owner, THREAD_OWNERS, CLOUD_READS, LOCAL_READS, thread_id

class OwnerBoundaryTests(unittest.TestCase):
    def test_secondary_resource_does_not_follow_system_identity(self):
        for resource,owner in THREAD_OWNERS.items():
            self.assertEqual(select_owner('thread/resume',{'threadId':resource},'11111111'),(owner,resource))

    def test_unknown_resource_cannot_fall_back_to_main(self):
        with self.assertRaises(ValueError):
            select_owner('thread/resume',{'threadId':'unmapped-thread'},'11111111')

    def test_global_reads_preserve_system_principal(self):
        self.assertEqual(select_owner('model/list',{},'11111111'),('11111111',None))

    def test_control_and_turn_methods_not_in_allowlist(self):
        for method in ['turn/start','thread/start','thread/fork','thread/realtime/start','command/exec',
                       'thread/unsubscribe','account/login/start']:
            self.assertNotIn(method,CLOUD_READS|LOCAL_READS)

    def test_notification_resource_extraction_preserves_owner(self):
        self.assertEqual(thread_id({'thread':{'id':'x'}}),'x')
        self.assertIsNone(thread_id({'thread':False}))


class ScriptedProcess:
    """A pipe-backed fake child; only synthetic tokens cross its stdin."""
    def __init__(self, results=None, refresh=False, exit_code=0):
        self.messages=[]
        self.output=queue.Queue()
        self.stdin=self
        self.stdout=self
        self.results=results or {}
        self.refresh=refresh
        self.exit_code=exit_code
        self.terminated=False

    def write(self, raw):
        value=json.loads(raw)
        self.messages.append(value)
        method=value.get('method')
        if 'id' not in value or method is None: return
        if method=='account/login/start' and self.refresh:
            self.output.put(json.dumps({'id':'refresh-request','method':'account/chatgptAuthTokens/refresh',
                                       'params':{'reason':'unauthorized'}})+'\n')
        result={
            'initialize':{'userAgent':'synthetic-native'},
            'account/login/start':{},
            'account/read':{'workspaceRouting':{'chatgptAccountId':'synthetic-system-account',
                            'backendOrigin':'https://chatgpt.com','accountRoutingOverride':'NO_CONSTRAINT'}},
            'configRequirements/read':{'requirements':{'application':{'network':{'enabled':True,
                                       'domains':{'chatgpt.com':'allow'}}}}},
        }.get(method,{})
        response=self.results.get(method,{'result':result})
        if response is not None:
            self.output.put(json.dumps({'id':value['id'],**response})+'\n')

    def flush(self): pass
    def readline(self): return self.output.get(timeout=2)
    def terminate(self):
        self.terminated=True
        self.output.put('')
    def wait(self, timeout=None): return self.exit_code


class ScriptedSocket:
    def __init__(self, *args, **kwargs):
        self.messages=[]
        self.output=queue.Queue()
        self.response=types.SimpleNamespace(status_code=101)
    def send(self, raw):
        value=json.loads(raw)
        self.messages.append(value)
        if 'id' in value and value.get('method') is not None:
            result={'thread':{'id':value['params']['threadId']}} if value['method']=='thread/resume' else {}
            self.output.put(json.dumps({'id':value['id'],'result':result}))
    def recv(self, timeout=None): return self.output.get(timeout=timeout or 2)
    def close(self): self.output.put('')


class AdapterFixture(unittest.TestCase):
    def owner(self, alias=None):
        return {'alias':alias or cloud_stdio.settings.SYSTEM_ALIAS,
                'token':'synthetic-sensitive-access-token','account_id':'synthetic-system-account'}

    def run_adapter(self, process, marked=False, requests=None, accounts=None,
                    native=False, input_stream=None, on_emit=None, socket_error=None,
                    inventory_error=None, socket_hook=None, socket_factory=None):
        requests=requests or [{'id':17,'method':'initialize','params':{'clientInfo':{'name':'test'}}},
                              {'method':'initialized'}]
        system=self.owner()
        secondary={**self.owner(cloud_stdio.settings.SECONDARY_ALIAS),'account_id':'synthetic-secondary-account'}
        arguments=['adapter','app-server']
        if marked: arguments+=['-c',cloud_stdio.APP_MARKER if marked is True else marked]
        output=io.StringIO()
        events=[]
        self.events=events
        self.emitted=[]
        sockets=[]
        pending={request['id'] for request in requests if 'id' in request and request.get('method')}
        completed=threading.Event()
        def inputs():
            for request in requests: yield json.dumps(request)+'\n'
            if native and pending:
                self.assertTrue(completed.wait(3),'Adapter did not complete native requests')
        def open_socket(*args,**kwargs):
            if not marked: raise AssertionError('Cloud connection outside test scope')
            if socket_hook is not None: socket_hook()
            if socket_error is not None: raise socket_error
            socket=(socket_factory or ScriptedSocket)(*args,**kwargs)
            socket.headers=kwargs.get('additional_headers',{})
            sockets.append(socket);return socket
        def emit(value):
            self.emitted.append(value)
            # The original initialize result must follow native policy verification.
            if not native and value.get('id')==17 and 'result' in value:
                self.assertEqual(process.messages[-1]['method'],'configRequirements/read')
            output.write(json.dumps(value)+'\n')
            if value.get('method') is None and value.get('id') in pending:
                pending.remove(value['id'])
                if not pending: completed.set()
            if on_emit is not None: on_emit(value)
        with patch.object(cloud_stdio.sys,'argv',arguments), \
             patch.object(cloud_stdio.sys,'stdin',input_stream if input_stream is not None else inputs()), \
             patch.object(cloud_stdio.settings,'INTEGRATION_MODE','native' if native else 'isolated',create=True), \
             patch.object(cloud_stdio.catalog,'inventory',side_effect=inventory_error,return_value=(system,accounts if accounts is not None else [system,secondary])), \
             patch.object(cloud_stdio,'event',side_effect=events.append), \
             patch.object(cloud_stdio,'emit',side_effect=emit), \
             patch.object(cloud_stdio,'refresh_thread_owners') as roots, \
             patch.object(cloud_stdio.subprocess,'Popen',return_value=process) as popen, \
             patch.object(cloud_stdio,'connect',side_effect=open_socket) as connect:
            try: cloud_stdio.main()
            finally:
                self.assertTrue(process.terminated)
                self.assertNotIn(system['token'],json.dumps(events))
                if popen.called:
                    self.assertNotIn(system['token'],json.dumps(popen.call_args.args))
                    self.assertNotIn(system['token'],json.dumps(popen.call_args.kwargs['env']))
        self.sockets=sockets
        return [json.loads(line) for line in output.getvalue().splitlines()],roots,connect,popen

class NativeBootstrapTests(AdapterFixture):
    def test_unmarked_app_server_bootstraps_same_child_without_cloud(self):
        process=ScriptedProcess()
        output,roots,connect,popen=self.run_adapter(process)
        self.assertEqual([m.get('method') for m in process.messages],
                         ['initialize','initialized','account/login/start','account/read','configRequirements/read'])
        self.assertEqual(process.messages[2]['params'],{'type':'chatgptAuthTokens',
                         'accessToken':self.owner()['token'],'chatgptAccountId':self.owner()['account_id']})
        self.assertEqual(process.messages[3]['params'],{'refreshToken':False})
        self.assertEqual(output,[{'id':17,'result':{'userAgent':'synthetic-native'}}])
        roots.assert_not_called()
        connect.assert_not_called()
        self.assertIn('cli_auth_credentials_store="ephemeral"',popen.call_args.args[0])

    def test_marked_app_session_initializes_cloud_after_verified_local_bootstrap(self):
        process=ScriptedProcess()
        output,roots,connect,_=self.run_adapter(process,marked=True)
        self.assertEqual(connect.call_count,2)
        roots.assert_called_once()
        self.assertEqual([m.get('method') for m in process.messages].count('initialized'),1)
        for socket in self.sockets:
            self.assertEqual([m.get('method') for m in socket.messages],['initialize','initialized'])
        self.assertEqual(output,[{'id':17,'result':{'userAgent':'synthetic-native'}}])

    def test_disabled_app_tools_plugin_still_identifies_desktop_session(self):
        marker=cloud_stdio.APP_MARKER.removesuffix('true')+'false'
        _,roots,connect,_=self.run_adapter(ScriptedProcess(),marked=marker)
        roots.assert_called_once()
        self.assertEqual(connect.call_count,2)
        self.assertTrue(self.events[0]['app_session'])

    def test_single_configured_owner_starts_one_cloud_session(self):
        system=self.owner()
        with patch.object(cloud_stdio.settings,'DOT_OWNERS',{'stable-handle':system['alias']}):
            _,_,connect,_=self.run_adapter(ScriptedProcess(),marked=True,accounts=[system])
        self.assertEqual(connect.call_count,1)

    def test_duplicate_labels_keep_distinct_owner_sessions_and_resource_routes(self):
        system=self.owner()
        other={**self.owner('33333333'),'account_id':'another-account'}
        unconfigured={**self.owner('44444444'),'account_id':'unconfigured-account'}
        handles={'dot-system':system['alias'],'dot-other':other['alias']}
        roots={'system-root':system['alias'],'other-root':other['alias']}
        with patch.object(cloud_stdio.settings,'DOT_OWNERS',handles), \
             patch.object(cloud_stdio.settings,'DOT_NAMES',{'dot-system':'Dot','dot-other':'Dot'},create=True), \
             patch.dict(cloud_stdio.THREAD_OWNERS,roots,clear=True):
            _,_,connect,_=self.run_adapter(ScriptedProcess(),marked=True,accounts=[system,other,unconfigured])
            self.assertEqual(select_owner('thread/resume',{'threadId':'system-root'},system['alias']),(system['alias'],'system-root'))
            self.assertEqual(select_owner('thread/resume',{'threadId':'other-root'},system['alias']),(other['alias'],'other-root'))
            with self.assertRaises(ValueError):
                select_owner('thread/resume',{'threadId':'unconfigured-root'},system['alias'])
        self.assertEqual(connect.call_count,2)
        routed={call.kwargs['additional_headers']['ChatGPT-Account-Id'] for call in connect.call_args_list}
        self.assertEqual(routed,{system['account_id'],other['account_id']})

    def test_missing_or_wrong_owner_routing_never_releases_initialize(self):
        for routing in [None,{}, {'chatgptAccountId':'another-owner','backendOrigin':'https://chatgpt.com',
                                'accountRoutingOverride':'NO_CONSTRAINT'},
                        {'chatgptAccountId':self.owner()['account_id'],'backendOrigin':'http://chatgpt.com',
                         'accountRoutingOverride':'NO_CONSTRAINT'}]:
            with self.subTest(routing=routing):
                process=ScriptedProcess({'account/read':{'result':{'workspaceRouting':routing}}})
                with self.assertRaisesRegex(ValueError,'routing verification failed'):
                    self.run_adapter(process)
                self.assertNotIn('configRequirements/read',[m.get('method') for m in process.messages])
                self.assertEqual(self.emitted,[])

    def test_backend_errors_are_sanitized_and_fail_closed(self):
        for method in ['initialize','account/login/start','account/read','configRequirements/read']:
            with self.subTest(method=method):
                process=ScriptedProcess({method:{'error':{'code':-32000,'message':self.owner()['token']}}})
                with self.assertRaisesRegex(ValueError,'^Local authentication bootstrap failed$'):
                    self.run_adapter(process)
                self.assertEqual(self.emitted,[])

    def test_routing_errors_use_exact_safe_categories(self):
        cases=[('workspace routing discovery failed','discovery_failed'),
               ('failed to load workspace requirements','requirements_load'),
               ('workspace routing discovery failed '+self.owner()['token'],'unclassified')]
        for message,category in cases:
            with self.subTest(category=category):
                process=ScriptedProcess({'account/read':{'error':{'code':-32603,'message':message}}})
                with self.assertRaisesRegex(ValueError,'^Local authentication bootstrap failed$'):
                    self.run_adapter(process)
                rejected=[event for event in self.events if 'bootstrap_rejected_method' in event]
                self.assertEqual(rejected,[{'bootstrap_rejected_method':'account/read',
                                          'bootstrap_error_code':-32603,
                                          'bootstrap_error_category':category}])
                self.assertEqual(self.emitted,[])

    def test_policy_must_be_read_without_substitution(self):
        process=ScriptedProcess({'configRequirements/read':{'result':{}}})
        with self.assertRaisesRegex(ValueError,'policy verification failed'):
            self.run_adapter(process)

    def test_refresh_callback_is_denied_without_native_forwarding(self):
        process=ScriptedProcess(refresh=True)
        output,*_=self.run_adapter(process)
        denied=[message for message in process.messages if message.get('id')=='refresh-request']
        self.assertEqual(len(denied),1)
        self.assertIn('error',denied[0])
        self.assertNotIn('accessToken',denied[0])
        self.assertFalse(any(message.get('id')=='refresh-request' for message in output))

    def test_bootstrap_timeout_is_bounded(self):
        process=ScriptedProcess({'initialize':None})
        bootstrap=cloud_stdio.LocalBootstrap(process.stdin)
        with self.assertRaisesRegex(ValueError,'timed out'):
            bootstrap.initialize({'params':{}},self.owner(),timeout=0)

    def test_local_reader_exit_fails_pending_rpc(self):
        process=ScriptedProcess({'initialize':None})
        bootstrap=cloud_stdio.LocalBootstrap(process.stdin)
        def send(value): bootstrap.closed()
        with patch.object(bootstrap,'send',side_effect=send):
            with self.assertRaisesRegex(ValueError,'bootstrap failed'):
                bootstrap.initialize({'params':{}},self.owner(),timeout=1)

    def test_ephemeral_store_override_replaces_conflicting_forms(self):
        arguments=['-c','cli_auth_credentials_store="file"','--config=cli_auth_credentials_store="keyring"',
                   '-c=cli_auth_credentials_store="auto"','-c','model="test"','app-server']
        actual=cloud_stdio.local_arguments(arguments)
        self.assertEqual(actual,[cloud_stdio.REAL,'-c','cli_auth_credentials_store="ephemeral"',
                                 '-c','model="test"','app-server'])

    def test_non_app_server_exec_is_unchanged(self):
        class Executed(Exception): pass
        with patch.object(cloud_stdio.sys,'argv',['adapter','--version']), \
             patch.object(cloud_stdio.os,'execve',side_effect=Executed) as execute, \
             patch.object(cloud_stdio.catalog,'inventory') as inventory:
            with self.assertRaises(Executed): cloud_stdio.main()
        self.assertEqual(execute.call_args.args[:2],(cloud_stdio.REAL,[cloud_stdio.REAL,'--version']))
        inventory.assert_not_called()


class NativeRootSnapshotTests(AdapterFixture):
    def setUp(self):
        self.directory=tempfile.TemporaryDirectory(prefix='cq-native-roots-')
        self.addCleanup(self.directory.cleanup)
        self.root=pathlib.Path(self.directory.name)
        self.root.chmod(0o700)
        self.snapshot={'version':1,'roots':[
            {'owner_alias':alias,'profile_id':profile,'root_thread_id':'fresh-'+alias}
            for alias,profile in cloud_stdio.settings.PROOF_PROFILE_IDS.items()]}

    def publish(self, value=None):
        path=self.root/'native-roots.json'
        path.write_text(json.dumps(self.snapshot if value is None else value))
        path.chmod(0o600)

    def test_published_rotated_roots_use_verified_owner_sessions_and_keep_normal_tasks_local(self):
        self.publish()
        primary='fresh-'+cloud_stdio.settings.SYSTEM_ALIAS
        secondary='fresh-'+cloud_stdio.settings.SECONDARY_ALIAS
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':primary}},
                  {'id':19,'method':'thread/resume','params':{'threadId':secondary}},
                  {'id':20,'method':'turn/start','params':{'threadId':secondary,'input':[]}},
                  {'id':21,'method':'thread/resume','params':{'threadId':'ordinary-local-task'}}]
        with patch.object(cloud_stdio,'ROOT',self.root), \
             patch.dict(cloud_stdio.THREAD_OWNERS,{},clear=True), \
             patch.object(cloud_stdio.catalog,'primary_roots',side_effect=AssertionError('No native network lookup')) as lookup:
            process=ScriptedProcess()
            output,refresh,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
            self.assertEqual(cloud_stdio.THREAD_OWNERS.get(primary),cloud_stdio.settings.SYSTEM_ALIAS)
            self.assertEqual(cloud_stdio.THREAD_OWNERS.get(secondary),cloud_stdio.settings.SECONDARY_ALIAS)
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertEqual(connect.call_count,2)
        routed={socket.headers['ChatGPT-Account-Id']:socket.messages[-1] for socket in self.sockets}
        self.assertEqual(routed,{'synthetic-system-account':requests[2],'synthetic-secondary-account':requests[3]})
        self.assertEqual([value['id'] for value in output if 'error' in value],[20])
        refresh.assert_not_called();lookup.assert_not_called()

    def test_primary_task_completes_before_any_snapshot_is_published_then_rotated_root_routes(self):
        primary='fresh-'+cloud_stdio.settings.SYSTEM_ALIAS
        ordinary_done=threading.Event(); dot_done=threading.Event()
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'turn/start','params':{'threadId':'ordinary-local-task','input':[]}},
                  {'id':19,'method':'thread/resume','params':{'threadId':primary}}]
        def inputs():
            for request in requests[:3]:yield json.dumps(request)+'\n'
            self.assertTrue(ordinary_done.wait(1),'Ordinary turn waited for root discovery')
            self.publish()
            yield json.dumps(requests[3])+'\n'
            self.assertTrue(dot_done.wait(2),'Published replacement root was not routed')
        def observed(value):
            if value.get('id')==18 and 'result' in value:ordinary_done.set()
            if value.get('id')==19 and 'result' in value:dot_done.set()
        with patch.object(cloud_stdio,'ROOT',self.root), \
             patch.dict(cloud_stdio.THREAD_OWNERS,{},clear=True), \
             patch.object(cloud_stdio.catalog,'primary_roots',side_effect=AssertionError('No native network lookup')) as lookup:
            process=ScriptedProcess()
            _,refresh,connect,_=self.run_adapter(process,marked=True,native=True,
                input_stream=inputs(),on_emit=observed)
        self.assertEqual(process.messages,requests[:3])
        self.assertEqual(connect.call_count,1)
        self.assertEqual(self.sockets[0].messages[-1],requests[3])
        refresh.assert_not_called();lookup.assert_not_called()

    def test_invalid_snapshot_is_rejected_as_a_whole_without_changing_existing_owners(self):
        cases=[]
        def changed():return json.loads(json.dumps(self.snapshot))
        value=changed();value['roots'][0]['profile_id']='unconfigured-profile';cases.append(value)
        value=changed();value['roots'][0]['owner_alias']='unconfigured-owner';cases.append(value)
        value=changed();value['roots'][1]['root_thread_id']=value['roots'][0]['root_thread_id'];cases.append(value)
        value=changed();value['roots'][1]=value['roots'][0];cases.append(value)
        value=changed();value['roots']=value['roots'][:1];cases.append(value)
        value=changed();value['version']=True;cases.append(value)
        value=changed();value['roots'][0]['root_thread_id']='';cases.append(value)
        value=changed();value['roots'][0]['extra']='unexpected';cases.append(value)
        value=changed();value['roots'][1]['root_thread_id']='conflicting-root';cases.append(value)
        original={'conflicting-root':self.snapshot['roots'][0]['owner_alias']}
        for value in cases:
            with self.subTest(value=value), patch.object(cloud_stdio,'ROOT',self.root), \
                 patch.dict(cloud_stdio.THREAD_OWNERS,original,clear=True), patch.object(cloud_stdio,'event'):
                self.publish(value)
                self.assertIsNone(cloud_stdio.native_thread_owner('fresh-'+cloud_stdio.settings.SYSTEM_ALIAS))
                self.assertEqual(cloud_stdio.THREAD_OWNERS,original)

    def test_stale_snapshot_preserves_newer_verified_roots_without_inventing_an_unpublished_root(self):
        self.publish()
        current={'current-root':cloud_stdio.settings.SYSTEM_ALIAS}
        with patch.object(cloud_stdio,'ROOT',self.root), \
             patch.dict(cloud_stdio.THREAD_OWNERS,current,clear=True):
            self.assertIsNone(cloud_stdio.native_thread_owner('unpublished-new-root'))
            self.assertEqual(cloud_stdio.native_thread_owner('current-root'),cloud_stdio.settings.SYSTEM_ALIAS)
            self.assertEqual(cloud_stdio.THREAD_OWNERS['current-root'],cloud_stdio.settings.SYSTEM_ALIAS)

    def test_invalid_snapshot_keeps_unknown_native_task_rpc_and_response_unchanged(self):
        self.publish({'version':1,'roots':[]})
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'ordinary-local-task'}}]
        with patch.object(cloud_stdio,'ROOT',self.root),patch.dict(cloud_stdio.THREAD_OWNERS,{},clear=True):
            process=ScriptedProcess()
            output,refresh,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
            self.assertEqual(cloud_stdio.THREAD_OWNERS,{})
        self.assertEqual(process.messages,requests)
        self.assertTrue(any(value.get('id')==18 and value.get('result')=={} for value in output))
        refresh.assert_not_called();connect.assert_not_called()


class NativeProfileTests(AdapterFixture):
    def test_native_app_server_receives_environment_base_override(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'}]
        with patch.dict(cloud_stdio.os.environ,{'OPENAI_BASE_URL':'http://127.0.0.1:19321'}):
            *_,popen=self.run_adapter(ScriptedProcess(),marked=True,native=True,requests=requests)
        self.assertEqual(popen.call_args.args[0][-2:],['-c','openai_base_url="http://127.0.0.1:19321"'])

    def test_unmarked_native_app_server_receives_same_environment_override(self):
        class Executed(Exception): pass
        with patch.object(cloud_stdio.sys,'argv',['adapter','app-server']), \
             patch.object(cloud_stdio.settings,'INTEGRATION_MODE','native',create=True), \
             patch.dict(cloud_stdio.os.environ,{'OPENAI_BASE_URL':'http://127.0.0.1:19321'}), \
             patch.object(cloud_stdio.os,'execve',side_effect=Executed) as execute:
            with self.assertRaises(Executed): cloud_stdio.main()
        self.assertEqual(execute.call_args.args[1],
                         [cloud_stdio.REAL,'app-server','-c','openai_base_url="http://127.0.0.1:19321"'])

    def test_launch_environment_overrides_model_base_without_file_config(self):
        arguments=['app-server','-c','openai_base_url="https://example.invalid"']
        routed=cloud_stdio.native_arguments(arguments,{'OPENAI_BASE_URL':'http://127.0.0.1:19321'})
        self.assertEqual(routed,[*arguments,'-c','openai_base_url="http://127.0.0.1:19321"'])
        self.assertEqual(arguments,['app-server','-c','openai_base_url="https://example.invalid"'])
        self.assertIs(cloud_stdio.native_arguments(arguments,{}),arguments)

    def test_invalid_proxy_environment_stops_before_native_launch(self):
        for base in ['', 'https://127.0.0.1:19321', 'http://localhost:19321',
                     'http://127.0.0.1', 'http://user@127.0.0.1:19321',
                     'http://127.0.0.1:19321?token=bad', 'http://127.0.0.1:99999']:
            with self.subTest(base=base),self.assertRaises(ValueError):
                cloud_stdio.native_arguments(['app-server'],{'OPENAI_BASE_URL':base})

    def test_native_primary_rpcs_keep_original_arguments_and_stock_authentication(self):
        requests=[{'id':17,'method':'initialize','params':{'clientInfo':{'name':'test'}}},
                  {'method':'initialized'},
                  {'id':18,'method':'thread/start','params':{'cwd':'/synthetic-project'}},
                  {'id':19,'method':'turn/start','params':{'threadId':'ordinary-local-task','input':[]}},
                  {'id':20,'method':'future/stockOperation','params':{'value':True}},
                  {'id':21,'method':'account/read','params':{'refreshToken':True}},
                  {'id':'cq-internal-user','method':'future/stockOperation','params':{}}]
        process=ScriptedProcess()
        output,roots,connect,popen=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(process.messages,requests)
        self.assertEqual({value['id'] for value in output},{17,18,19,20,21,'cq-internal-user'})
        self.assertNotIn('cli_auth_credentials_store="ephemeral"',popen.call_args.args[0])
        self.assertFalse(any(message.get('method')=='account/login/start' for message in process.messages))
        roots.assert_not_called()
        connect.assert_not_called()

    def test_native_large_stock_rpc_keeps_stock_frame_policy(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'future/stockOperation','params':{'data':'x'*(4*1024*1024)}}]
        process=ScriptedProcess()
        output,*_=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(process.messages,requests)
        self.assertTrue(any(value.get('id')==18 and 'result' in value for value in output))

    def test_native_auth_refresh_server_request_and_client_response_pass_unchanged(self):
        callback=threading.Event()
        complete=threading.Event()
        reply={'id':'refresh-request','result':{'accessToken':'synthetic-refreshed-access-token',
                                              'chatgptAccountId':'synthetic-system-account'}}
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'account/login/start','params':{'type':'chatgpt'}}]
        def inputs():
            for request in requests: yield json.dumps(request)+'\n'
            self.assertTrue(callback.wait(2),'Stock auth refresh callback was not forwarded')
            yield json.dumps(reply)+'\n'
            yield json.dumps({'id':19,'method':'account/read','params':{'refreshToken':True}})+'\n'
            self.assertTrue(complete.wait(2),'Stock response was not forwarded')
        def observed(value):
            if value.get('method')=='account/chatgptAuthTokens/refresh':callback.set()
            if value.get('id')==19:complete.set()
        process=ScriptedProcess(refresh=True)
        output,*_=self.run_adapter(process,marked=True,native=True,input_stream=inputs(),on_emit=observed)
        self.assertIn(reply,process.messages)
        refresh=[value for value in output if value.get('id')=='refresh-request']
        self.assertEqual(refresh,[{'id':'refresh-request','method':'account/chatgptAuthTokens/refresh',
                                  'params':{'reason':'unauthorized'}}])
        self.assertFalse(any('error' in value for value in output))

    def test_native_secondary_root_opens_only_own_cloud_session_lazily(self):
        roots={'system-root':cloud_stdio.settings.SYSTEM_ALIAS,'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS}
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}}]
        with patch.dict(cloud_stdio.THREAD_OWNERS,roots,clear=True):
            process=ScriptedProcess()
            _,_,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(connect.call_count,1)
        self.assertEqual(connect.call_args.kwargs['additional_headers']['ChatGPT-Account-Id'],
                         'synthetic-secondary-account')
        self.assertEqual(process.messages,requests[:2])
        self.assertEqual(self.sockets[0].messages[-1],requests[-1])
        self.assertEqual([message.get('method') for message in self.sockets[0].messages],
                         ['initialize','initialized','thread/resume'])

    def test_native_primary_dot_root_uses_cloud_while_normal_primary_task_stays_local(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'primary-root'}},
                  {'id':19,'method':'thread/resume','params':{'threadId':'ordinary-local-task'}}]
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'primary-root':cloud_stdio.settings.SYSTEM_ALIAS},clear=True):
            process=ScriptedProcess()
            _,_,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(connect.call_count,1)
        self.assertEqual(connect.call_args.kwargs['additional_headers']['ChatGPT-Account-Id'],
                         'synthetic-system-account')
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertEqual(self.sockets[0].messages[-1],requests[2])

    def test_native_child_exit_propagates_while_client_stdin_remains_open(self):
        initialized=threading.Event(); release=threading.Event()
        process=ScriptedProcess(exit_code=7)
        request={'id':17,'method':'initialize','params':{}}
        def inputs():
            yield json.dumps(request)+'\n'
            self.assertTrue(initialized.wait(1))
            process.output.put('')
            release.wait(3)
            yield json.dumps({'id':18,'method':'future/stockOperation','params':{}})+'\n'
        def observed(value):
            if value.get('id')==17 and 'result' in value:initialized.set()
        try:
            with self.assertRaises(SystemExit) as exited:
                self.run_adapter(process,marked=True,native=True,input_stream=inputs(),on_emit=observed)
            self.assertEqual(exited.exception.code,7)
        finally:release.set()
        self.assertEqual(process.messages,[request])

    def test_native_real_child_exit_closes_adapter_with_client_pipe_open(self):
        script='''
import sys
import test_support
import cloud_stdio
cloud_stdio.settings.INTEGRATION_MODE='native'
cloud_stdio.event=lambda value: None
def unavailable(): raise ValueError('Synthetic inventory unavailable')
cloud_stdio.catalog.inventory=unavailable
real_popen=cloud_stdio.subprocess.Popen
def spawn(*args,**kwargs):
    child='import json,sys; request=json.loads(sys.stdin.readline()); print(json.dumps({"id":request["id"],"result":{}}),flush=True); print("synthetic-stock-error",file=sys.stderr,flush=True); sys.exit(7)'
    return real_popen([sys.executable,'-u','-c',child],**kwargs)
cloud_stdio.subprocess.Popen=spawn
sys.argv=['adapter','app-server','-c',cloud_stdio.APP_MARKER]
cloud_stdio.main()
'''
        process=subprocess.Popen([sys.executable,'-u','-c',script],
            cwd=str(cloud_stdio.pathlib.Path(cloud_stdio.__file__).parent),
            stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        try:
            process.stdin.write(json.dumps({'id':17,'method':'initialize','params':{}})+'\n')
            process.stdin.flush()
            self.assertEqual(process.wait(timeout=5),7)
            self.assertEqual(json.loads(process.stdout.read()),{'id':17,'result':{}})
            self.assertEqual(process.stderr.read(),'synthetic-stock-error\n')
        finally:
            if process.poll() is None:process.kill();process.wait(timeout=5)
            process.stdin.close();process.stdout.close();process.stderr.close()

    def test_native_unsupported_secondary_write_never_reaches_primary(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'turn/start','params':{'threadId':'secondary-root','input':[]}},
                  {'id':19,'method':'future/stockOperation','params':{'threadId':'secondary-root'}},
                  {'id':20,'method':'turn/start','params':{'threadId':'primary-root','input':[]}}]
        roots={'primary-root':cloud_stdio.settings.SYSTEM_ALIAS,'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS}
        with patch.dict(cloud_stdio.THREAD_OWNERS,roots,clear=True):
            process=ScriptedProcess()
            output,_,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertEqual({value['id'] for value in output if 'error' in value},{18,19})
        connect.assert_not_called()

    def test_native_secondary_outage_does_not_break_following_primary_operation(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                  {'id':19,'method':'thread/start','params':{}}]
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            process=ScriptedProcess()
            output,*_=self.run_adapter(process,marked=True,native=True,requests=requests,
                                      socket_error=OSError('synthetic upstream failure'))
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertTrue(any(value.get('id')==18 and 'error' in value for value in output))

    def test_native_closed_owner_session_reconnects_on_later_request(self):
        for failure in ['',OSError('Synthetic owner receive failure')]:
            with self.subTest(failure=type(failure).__name__):
                closed=threading.Event();first_done=threading.Event();second_done=threading.Event()
                created=[]
                class ClosingSocket(ScriptedSocket):
                    def send(self,raw):
                        super().send(raw)
                        if json.loads(raw).get('method')=='thread/resume':self.output.put(failure)
                    def recv(self,timeout=None):
                        value=super().recv(timeout)
                        if isinstance(value,Exception):raise value
                        return value
                    def close(self):
                        super().close();closed.set()
                def create(*args,**kwargs):
                    socket=(ClosingSocket if not created else ScriptedSocket)(*args,**kwargs)
                    created.append(socket)
                    return socket
                requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                          {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                          {'id':19,'method':'thread/resume','params':{'threadId':'secondary-root'}}]
                def inputs():
                    for request in requests[:3]:yield json.dumps(request)+'\n'
                    self.assertTrue(first_done.wait(1),'Initial owner read did not complete')
                    self.assertTrue(closed.wait(1),'Dead owner session was not closed')
                    yield json.dumps(requests[3])+'\n'
                    self.assertTrue(second_done.wait(1),'Later owner request did not reconnect')
                def observed(value):
                    if value.get('id')==18 and 'result' in value:first_done.set()
                    if value.get('id')==19 and 'result' in value:second_done.set()
                with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
                    process=ScriptedProcess()
                    _,_,connect,_=self.run_adapter(process,marked=True,native=True,
                        input_stream=inputs(),on_emit=observed,socket_factory=create)
                self.assertEqual(connect.call_count,2)
                self.assertEqual(process.messages,requests[:2])
                self.assertEqual([value['method'] for value in created[1].messages],
                                 ['initialize','initialized','thread/resume'])

    def test_native_owner_eof_fails_pending_request_once_with_original_id_then_reconnects(self):
        closed=threading.Event();first_done=threading.Event();second_done=threading.Event()
        created=[]
        class LostReadSocket(ScriptedSocket):
            def send(self,raw):
                if json.loads(raw).get('method')=='thread/resume':
                    self.messages.append(json.loads(raw));self.output.put('')
                else:super().send(raw)
            def close(self):super().close();closed.set()
        def create(*args,**kwargs):
            socket=(LostReadSocket if not created else ScriptedSocket)(*args,**kwargs)
            created.append(socket)
            return socket
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                  {'id':19,'method':'thread/resume','params':{'threadId':'secondary-root'}}]
        def inputs():
            for request in requests[:3]:yield json.dumps(request)+'\n'
            self.assertTrue(first_done.wait(1),'Lost owner request was left waiting indefinitely')
            self.assertTrue(closed.wait(1),'Dead socket was not closed')
            yield json.dumps(requests[3])+'\n'
            self.assertTrue(second_done.wait(1),'Replacement owner session did not answer')
        def observed(value):
            if value.get('id')==18 and 'error' in value:first_done.set()
            if value.get('id')==19 and 'result' in value:second_done.set()
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            output,_,connect,_=self.run_adapter(ScriptedProcess(),marked=True,native=True,
                input_stream=inputs(),on_emit=observed,socket_factory=create)
        failures=[value for value in output if value.get('id')==18]
        self.assertEqual(len(failures),1)
        self.assertIs(type(failures[0]['id']),int)
        self.assertEqual(connect.call_count,2)
        with cloud_stdio.PENDING_LOCK:
            self.assertFalse(any(key[0]==cloud_stdio.settings.SECONDARY_ALIAS for key in cloud_stdio.PENDING))

    def test_native_secondary_initialization_cannot_block_primary_and_is_not_duplicated(self):
        started=threading.Event(); release=threading.Event(); expired=threading.Event()
        primary_done=threading.Event(); secondary_done=threading.Event()
        secondary_results=set()
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                  {'id':19,'method':'turn/start','params':{'threadId':'ordinary-local-task','input':[]}},
                  {'id':20,'method':'thread/resume','params':{'threadId':'secondary-root'}}]
        def delayed_connect():
            started.set()
            if not release.wait(2):
                expired.set()
                raise OSError('Synthetic delayed connection expired')
        def inputs():
            try:
                for request in requests[:3]: yield json.dumps(request)+'\n'
                self.assertTrue(started.wait(1),'Secondary connection did not start')
                self.assertFalse(expired.is_set(),'Secondary connection blocked primary stdin')
                for request in requests[3:]: yield json.dumps(request)+'\n'
                self.assertTrue(primary_done.wait(1),'Primary turn was blocked by secondary initialization')
                release.set()
                self.assertTrue(secondary_done.wait(2),'Secondary reads did not complete')
            finally: release.set()
        def observed(value):
            if value.get('id')==19 and 'result' in value:primary_done.set()
            if value.get('id') in {18,20} and 'result' in value:
                secondary_results.add(value['id'])
                if len(secondary_results)==2:secondary_done.set()
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            process=ScriptedProcess()
            _,_,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests,
                input_stream=inputs(),on_emit=observed,socket_hook=delayed_connect)
        self.assertEqual(process.messages,requests[:2]+[requests[3]])
        self.assertEqual(connect.call_count,1)
        self.assertEqual([value['method'] for value in self.sockets[0].messages],
                         ['initialize','initialized','thread/resume','thread/resume'])

    def test_native_secondary_timeout_cannot_block_primary_turn(self):
        started=threading.Event(); release=threading.Event(); expired=threading.Event()
        primary_done=threading.Event(); secondary_done=threading.Event()
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                  {'id':19,'method':'turn/start','params':{'threadId':'ordinary-local-task','input':[]}}]
        def delayed_failure():
            started.set()
            if not release.wait(2):expired.set()
            raise TimeoutError('Synthetic owner connection timeout')
        def inputs():
            try:
                for request in requests[:3]: yield json.dumps(request)+'\n'
                self.assertTrue(started.wait(1))
                self.assertFalse(expired.is_set(),'Owner timeout blocked primary stdin')
                yield json.dumps(requests[3])+'\n'
                self.assertTrue(primary_done.wait(1),'Primary turn waited for owner timeout')
                release.set()
                self.assertTrue(secondary_done.wait(2))
            finally:release.set()
        def observed(value):
            if value.get('id')==19 and 'result' in value:primary_done.set()
            if value.get('id')==18 and 'error' in value:secondary_done.set()
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            process=ScriptedProcess()
            output,*_=self.run_adapter(process,marked=True,native=True,requests=requests,
                input_stream=inputs(),on_emit=observed,socket_hook=delayed_failure)
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertTrue(any(value.get('id')==18 and 'error' in value for value in output))

    def test_native_full_owner_queue_rejects_overflow_without_blocking_primary(self):
        started=threading.Event(); release=threading.Event(); overflow=threading.Event()
        primary_done=threading.Event(); secondary_done=threading.Event()
        replies=set()
        prefix=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}}]
        queued=[{'id':identifier,'method':'thread/resume','params':{'threadId':'secondary-root'}}
                for identifier in range(20,53)]
        primary={'id':19,'method':'turn/start','params':{'threadId':'ordinary-local-task','input':[]}}
        def delayed_connect():
            started.set()
            if not release.wait(2):raise TimeoutError('Synthetic connection remained blocked')
        def inputs():
            try:
                for request in prefix:yield json.dumps(request)+'\n'
                self.assertTrue(started.wait(1))
                for request in queued:yield json.dumps(request)+'\n'
                yield json.dumps(primary)+'\n'
                self.assertTrue(overflow.wait(1),'Owner queue did not reject its overflow')
                self.assertTrue(primary_done.wait(1),'Full owner queue blocked primary turn')
                release.set()
                self.assertTrue(secondary_done.wait(2))
            finally:release.set()
        def observed(value):
            if value.get('id')==52 and 'error' in value:overflow.set()
            if value.get('id')==19 and 'result' in value:primary_done.set()
            if (value.get('id')==18 or value.get('id') in range(20,52)) and 'result' in value:
                replies.add(value['id'])
                if len(replies)==33:secondary_done.set()
        with patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            process=ScriptedProcess()
            _,_,connect,_=self.run_adapter(process,marked=True,native=True,
                input_stream=inputs(),on_emit=observed,socket_hook=delayed_connect)
        self.assertEqual(process.messages,prefix[:2]+[primary])
        self.assertEqual(connect.call_count,1)
        self.assertEqual(len(self.sockets[0].messages),35)

    def test_native_missing_secondary_credentials_preserves_stock_primary_startup(self):
        process=ScriptedProcess()
        _,_,connect,_=self.run_adapter(process,marked=True,native=True,
                                      inventory_error=ValueError('synthetic inventory unavailable'))
        self.assertEqual([value.get('method') for value in process.messages],['initialize','initialized'])
        connect.assert_not_called()

    def test_native_missing_public_ca_keeps_local_tasks_and_fails_closed_for_dot(self):
        requests=[{'id':17,'method':'initialize','params':{}},{'method':'initialized'},
                  {'id':18,'method':'thread/resume','params':{'threadId':'secondary-root'}},
                  {'id':19,'method':'thread/start','params':{}}]
        with patch.object(cloud_stdio.ssl,'get_default_verify_paths',
                          return_value=types.SimpleNamespace(openssl_cafile=None)), \
             patch.dict(cloud_stdio.THREAD_OWNERS,{'secondary-root':cloud_stdio.settings.SECONDARY_ALIAS},clear=True):
            process=ScriptedProcess()
            output,_,connect,_=self.run_adapter(process,marked=True,native=True,requests=requests)
        self.assertEqual(process.messages,requests[:2]+requests[-1:])
        self.assertTrue(any(value.get('id')==18 and 'error' in value for value in output))
        self.assertTrue(any(value.get('id')==19 and 'result' in value for value in output))
        connect.assert_not_called()

    def test_native_unmarked_app_server_executes_stock_without_catalog_access(self):
        class Executed(Exception): pass
        arguments=['app-server','-c','cli_auth_credentials_store="keyring"']
        with patch.object(cloud_stdio.settings,'INTEGRATION_MODE','native',create=True), \
             patch.object(cloud_stdio.sys,'argv',['adapter',*arguments]), \
             patch.object(cloud_stdio.os,'execve',side_effect=Executed) as execute, \
             patch.object(cloud_stdio.catalog,'inventory') as inventory:
            with self.assertRaises(Executed):cloud_stdio.main()
        self.assertEqual(execute.call_args.args[:2],(cloud_stdio.REAL,[cloud_stdio.REAL,*arguments]))
        inventory.assert_not_called()

if __name__=='__main__': unittest.main()
