import test_support
import unittest
from room_coordination import parse,permitted
class RelayBoundaries(unittest.TestCase):
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
        import json,re,room_coordination as relay
        from unittest.mock import patch
        names=['Work/Research','Quotes"and\\slashes']
        with patch.object(relay,'PEERS','|'.join(re.escape(x) for x in names)),patch.object(relay.transport.native,'evaluate',return_value=[]) as evaluate:
            relay.read_explicit('Primary')
        expression=evaluate.call_args.args[0]
        encoded=expression.split('const pattern=new RegExp(',1)[1].split(');\nreturn',1)[0]
        pattern=json.loads(encoded)
        for name in names:
            self.assertRegex('[CQ:'+name+':'+'a'*32+']\nHello\n[/CQ]',pattern)

class RelayFilesystemTests(unittest.TestCase):
    def test_symlink_relay_directory_cannot_change_outside_state(self):
        import pathlib,tempfile,room_coordination as relay
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
        import pathlib,tempfile,room_coordination as relay
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
        import pathlib,tempfile,room_coordination as relay
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
