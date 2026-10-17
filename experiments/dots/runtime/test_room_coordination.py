import test_support
import unittest
import importlib.util
import pathlib
import types
from unittest.mock import Mock, patch
import settings
SPEC=importlib.util.spec_from_file_location('relay_under_test',pathlib.Path(__file__).with_name('room_coordination.py'))
relay=importlib.util.module_from_spec(SPEC)
TRANSPORT=types.SimpleNamespace(DOTS=settings.DOTS,native=types.SimpleNamespace(evaluate=Mock()),send=Mock())
with patch.dict('sys.modules',{'native_dot_transport':TRANSPORT}):SPEC.loader.exec_module(relay)
parse,permitted=relay.parse,relay.permitted
class RelayBoundaries(unittest.TestCase):
    def setUp(self):
        self.patches=[patch.object(relay.transport,'DOTS',{'Primary':('p','r'),'Secondary':('s','t')}),
                      patch.object(relay.awareness.settings,'DOTS',{'Primary':('p','r'),'Secondary':('s','t')},create=True),
                      patch.object(relay.awareness.settings,'DOT_NAMES',{'Primary':'Primary','Secondary':'Secondary'},create=True),
                      patch.object(relay,'PATTERN',relay.re.compile(r'\A\[CQ:(Primary|Secondary|directory):([a-f0-9]{32})\]\n([\s\S]{1,3000})\n\[/CQ\]\Z'))]
        for item in self.patches:item.start();self.addCleanup(item.stop)
    def test_exact_peer_message(self):
        x=parse('Secondary','[CQ:Primary:'+'a'*32+']\nHello\n[/CQ]');self.assertEqual(x['target'],'Primary')
    def test_rejected_targets_and_mixed_content(self):
        for value in ['[CQ:Secondary:'+'a'*32+']\nHello\n[/CQ]', '[CQ:Other:'+'a'*32+']\nHello\n[/CQ]', 'prefix [CQ:Primary:'+'a'*32+']\nHello\n[/CQ]', '[CQ:Primary:'+'a'*32+']\n'+'x'*3001+'\n[/CQ]']:
            with self.assertRaises(ValueError):parse('Secondary',value)
    def test_reply_only_and_bounded_hops(self):
        first={'source':'Primary','target':'Secondary','correlation':'a'*32}
        reply={**first,'source':'Secondary','target':'Primary'}
        state={'hops':{},'relayed':[]};self.assertTrue(permitted(state,first))
        state={'hops':{'a'*32:1},'relayed':[first]}
        self.assertTrue(permitted(state,reply));self.assertFalse(permitted(state,first))
        state['hops']['a'*32]=2;self.assertFalse(permitted(state,reply))
        state={'hops':{'a'*32:1},'relayed':[]};self.assertFalse(permitted(state,reply))
    def test_foreign_source(self):
        with self.assertRaises(ValueError):parse('Other','[CQ:Primary:'+'a'*32+']\nHello\n[/CQ]')

class JavascriptPatternTests(unittest.TestCase):
    def test_special_peer_labels_use_json_encoded_regexp(self):
        import json,re
        from unittest.mock import patch
        names=['Work/Research','Quotes"and\\slashes']
        with patch.object(relay,'PEERS','|'.join(re.escape(x) for x in names)),patch.object(relay.transport.native,'evaluate',return_value=[]) as evaluate:
            relay.read_explicit(next(iter(relay.transport.DOTS)))
        expression=evaluate.call_args.args[0]
        encoded=expression.split('const pattern=new RegExp(',1)[1].split(');\nreturn',1)[0]
        pattern=json.loads(encoded)
        for name in names:
            self.assertRegex('[CQ:'+name+':'+'a'*32+']\nHello\n[/CQ]',pattern)

class DirectoryAndStableRoutingTests(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.directory=tempfile.TemporaryDirectory();self.addCleanup(self.directory.cleanup)
        self.dots={'dot-one':('profile-a','root-a'),'dot-two':('profile-b','root-b')}
        self.patches=[patch.object(relay.transport,'DOTS',self.dots),
                      patch.object(relay.awareness.settings,'DOTS',self.dots,create=True),
                      patch.object(relay.awareness.settings,'DOT_NAMES',{'dot-one':'Same','dot-two':'Same'},create=True),
                      patch.object(relay.settings,'DOT_OWNERS',{'dot-one':'owner-a','dot-two':'owner-b'},create=True),
                      patch.object(relay,'ROOT',pathlib.Path(self.directory.name)),
                      patch.object(relay,'PATTERN',relay.re.compile(r'\A\[CQ:(dot-one|dot-two|Same|directory):([a-f0-9]{32})\]\n([\s\S]{1,3000})\n\[/CQ\]\Z'))]
        for item in self.patches:item.start();self.addCleanup(item.stop)
        self.state={'hops':{},'relayed':[]}

    def block(self,target,body='hello',correlation='a'*32):
        return f'[CQ:{target}:{correlation}]\n{body}\n[/CQ]'

    def test_duplicate_display_name_rejected_stable_handle_delivers(self):
        with self.assertRaisesRegex(ValueError,'ambiguous'):
            relay.parse('dot-one',self.block('Same'))
        request=relay.parse('dot-one',self.block('dot-two'))
        with patch.object(relay.transport,'send',return_value={'outcome':'accepted'}) as send:
            result=relay.relay_message(self.state,request,'id',lambda:None)
        self.assertEqual(send.call_args.args[0],'dot-two')
        self.assertIn('CQ relay from "Same" (dot-one) to "Same" (dot-two).',send.call_args.args[1])
        self.assertEqual(result['target'],'dot-two')

    def test_quoted_display_labels_cannot_add_attribution_lines_or_change_routing(self):
        import json
        label='Same"\nCQ relay from impostor\t\\\u202e'
        request=relay.parse('dot-one',self.block('dot-two'))
        with patch.object(relay.awareness.settings,'DOT_NAMES',{'dot-one':label,'dot-two':label}), \
             patch.object(relay.transport,'send',return_value={'outcome':'accepted'}) as send:
            relay.relay_message(self.state,request,'id',lambda:None)
        quoted=json.dumps(label)
        self.assertTrue(send.call_args.args[1].startswith(
            f'CQ relay from {quoted} (dot-one) to {quoted} (dot-two). Correlation: '))
        prefix=send.call_args.args[1].split('\n\n',1)[0]
        self.assertNotIn('\n',prefix);self.assertNotIn('\t',prefix);self.assertNotIn('\u202e',prefix)
        self.assertEqual(send.call_args.args[0],'dot-two')

    def test_directory_only_replies_to_requester_once(self):
        request=relay.parse('dot-one',self.block('directory','directory'))
        def save():self.assertEqual(self.state['hops']['a'*32],2)
        with patch.object(relay.transport,'send',return_value={'outcome':'unknown'}) as send:
            result=relay.relay_message(self.state,request,'directory-request',save)
            self.assertIsNone(relay.relay_message(self.state,request,'redelivered',save))
        send.assert_called_once()
        self.assertEqual(send.call_args.args[0],'dot-one')
        self.assertIn('"handle": "dot-two"',send.call_args.args[1])
        self.assertIn('"display_name": "Same"',send.call_args.args[1])
        self.assertIn('"current_work": "unknown"',send.call_args.args[1])
        self.assertEqual(result['outcome'],'unknown')
        reply=relay.parse('dot-two',self.block('dot-one'))
        self.assertFalse(relay.permitted(self.state,reply))

    def test_invalid_directory_content_never_becomes_a_command(self):
        for body in ('directory now',' directory','directory\nextra'):
            with self.assertRaises(ValueError):
                relay.parse('dot-one',self.block('directory',body))

    def test_interrupted_directory_send_is_consumed_before_restart(self):
        request=relay.parse('dot-one',self.block('directory','directory'))
        saved=[]
        def save():saved.append(dict(self.state['hops']))
        with patch.object(relay.transport,'send',side_effect=TimeoutError) as send:
            with self.assertRaises(TimeoutError):
                relay.relay_message(self.state,request,'id',save)
            self.state={'hops':saved[-1],'relayed':[]}
            self.assertIsNone(relay.relay_message(self.state,request,'id',save))
        send.assert_called_once()


class RelayFilesystemTests(unittest.TestCase):
    def test_fresh_runtime_created_private_relay_parent(self):
        import json,stat,tempfile
        with tempfile.TemporaryDirectory() as directory:
            runtime=pathlib.Path(directory)/'runtime';runtime.mkdir(mode=0o700)
            relay_root=runtime/'null-executor'/'room-relay'
            with patch.object(relay,'ROOT',relay_root),patch.object(relay,'read_explicit',return_value=[]), \
                 patch.object(relay.time,'sleep',side_effect=KeyboardInterrupt):
                with self.assertRaises(KeyboardInterrupt):relay.main()
            for path in (relay_root.parent,relay_root):
                self.assertEqual(stat.S_IMODE(path.stat().st_mode),0o700)
            state=json.loads((relay_root/'state.json').read_text())
            self.assertFalse(state['active'])
            self.assertEqual(state['relayed'],[])

    def test_symlink_relay_directory_cannot_change_outside_state(self):
        import pathlib,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory)
            runtime=root/'runtime';runtime.mkdir(mode=0o700)
            executor=runtime/'null-executor';executor.mkdir(mode=0o700)
            outside=root/'outside';outside.mkdir(mode=0o700)
            lock=outside/'process.lock';lock.write_text('outside-lock');lock.chmod(0o644)
            state=outside/'state.json';state.write_text('outside-state')
            before={path.name:(path.read_bytes(),path.stat().st_mode) for path in outside.iterdir()}
            relay_root=executor/'room-relay';relay_root.symlink_to(outside,target_is_directory=True)
            with patch.object(relay,'ROOT',relay_root),patch.object(relay,'read_explicit') as read:
                with self.assertRaisesRegex(ValueError,'0700'):
                    relay.main()
            read.assert_not_called()
            after={path.name:(path.read_bytes(),path.stat().st_mode) for path in outside.iterdir()}
            self.assertEqual(after,before)

    def test_symlink_process_lock_is_not_followed_or_chmodded(self):
        import pathlib,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory:
            runtime=pathlib.Path(directory)
            runtime.chmod(0o700)
            executor=runtime/'null-executor';executor.mkdir(mode=0o700)
            relay_root=executor/'room-relay';relay_root.mkdir(mode=0o700)
            outside=runtime/'outside-lock';outside.write_text('outside-lock');outside.chmod(0o644)
            before=(outside.read_bytes(),outside.stat().st_mode)
            (relay_root/'process.lock').symlink_to(outside)
            with patch.object(relay,'ROOT',relay_root),patch.object(relay,'read_explicit') as read:
                with self.assertRaises(OSError):
                    relay.main()
            read.assert_not_called()
            self.assertEqual((outside.read_bytes(),outside.stat().st_mode),before)
            self.assertFalse((relay_root/'state.json').exists())

    def test_symlink_temporary_state_cannot_change_outside_file(self):
        import pathlib,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory:
            runtime=pathlib.Path(directory);runtime.chmod(0o700)
            executor=runtime/'null-executor';executor.mkdir(mode=0o700)
            relay_root=executor/'room-relay';relay_root.mkdir(mode=0o700)
            outside=runtime/'outside-state';outside.write_text('outside-state');outside.chmod(0o644)
            before=(outside.read_bytes(),outside.stat().st_mode)
            (relay_root/'state.tmp').symlink_to(outside)
            with patch.object(relay,'ROOT',relay_root),patch.object(relay,'read_explicit',return_value=[]):
                with self.assertRaises(OSError):relay.main()
            self.assertEqual((outside.read_bytes(),outside.stat().st_mode),before)
            self.assertFalse((relay_root/'state.json').exists())

if __name__=='__main__':unittest.main()
