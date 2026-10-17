import test_support
import io, json, queue, types, unittest
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
    def __init__(self, results=None, refresh=False):
        self.messages=[]
        self.output=queue.Queue()
        self.stdin=self
        self.stdout=self
        self.results=results or {}
        self.refresh=refresh
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
    def wait(self, timeout=None): return 0


class ScriptedSocket:
    def __init__(self, *args, **kwargs):
        self.messages=[]
        self.output=queue.Queue()
        self.response=types.SimpleNamespace(status_code=101)
    def send(self, raw):
        value=json.loads(raw)
        self.messages.append(value)
        if value.get('method')=='initialize':
            self.output.put(json.dumps({'id':value['id'],'result':{}}))
    def recv(self, timeout=None): return self.output.get(timeout=timeout or 2)
    def close(self): self.output.put('')


class NativeBootstrapTests(unittest.TestCase):
    def owner(self, alias=None):
        return {'alias':alias or cloud_stdio.settings.SYSTEM_ALIAS,
                'token':'synthetic-sensitive-access-token','account_id':'synthetic-system-account'}

    def run_adapter(self, process, marked=False, requests=None, accounts=None):
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
        def open_socket(*args,**kwargs):
            if not marked: raise AssertionError('Cloud connection outside test scope')
            socket=ScriptedSocket(); sockets.append(socket); return socket
        def emit(value):
            self.emitted.append(value)
            # The original initialize result must follow native policy verification.
            if value.get('id')==17 and 'result' in value:
                self.assertEqual(process.messages[-1]['method'],'configRequirements/read')
            output.write(json.dumps(value)+'\n')
        with patch.object(cloud_stdio.sys,'argv',arguments), \
             patch.object(cloud_stdio.sys,'stdin',io.StringIO(''.join(json.dumps(r)+'\n' for r in requests))), \
             patch.object(cloud_stdio.catalog,'inventory',return_value=(system,accounts if accounts is not None else [system,secondary])), \
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

if __name__=='__main__': unittest.main()
