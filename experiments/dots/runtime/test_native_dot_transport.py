import test_support
import json, pathlib, tempfile, unittest
from unittest.mock import patch
import native_dot_transport as transport


class NativeTargetTests(unittest.TestCase):
    def test_single_owner_target_uses_its_verified_profile_and_root(self):
        self.assert_targets({'dot-one':('one-profile','one-root')})

    def test_duplicate_display_names_keep_distinct_stable_targets(self):
        self.assert_targets({'dot-one':('one-profile','one-root'),
                             'dot-two':('two-profile','two-root')})

    def assert_targets(self, dots):
        with tempfile.TemporaryDirectory() as directory, \
             patch.object(transport,'DOTS',dots), \
             patch.object(transport,'SYSTEM_DOT',dots['dot-one']), \
             patch.object(transport.settings,'DOT_NAMES',{handle:'Dot' for handle in dots},create=True), \
             patch.object(transport.native,'evaluate',return_value={'accepted':True,'message_id':'synthetic'}) as evaluate:
            for index,(handle,(profile,root)) in enumerate(dots.items()):
                ledger=pathlib.Path(directory)/str(index)
                result=transport.send(handle,'synthetic message',ledger)
                self.assertEqual(result['target'],handle)
                self.assertEqual(result['outcome'],'accepted')
                expression=evaluate.call_args.args[0]
                expected={'profile':profile,'root':root,'text':'synthetic message','request':result['request_id']}
                self.assertIn(json.dumps(expected),expression)
                self.assertEqual(json.loads(ledger.read_text())['target'],handle)
            with self.assertRaises(ValueError):
                transport.send('Dot','synthetic message',pathlib.Path(directory)/'ambiguous-name')
            self.assertEqual(evaluate.call_count,len(dots))

    def test_missing_system_dot_fails_before_native_evaluation_or_ledger(self):
        with tempfile.TemporaryDirectory() as directory, \
             patch.object(transport,'SYSTEM_DOT',None), \
             patch.object(transport.native,'evaluate') as evaluate:
            ledger=pathlib.Path(directory)/'ledger'
            with self.assertRaisesRegex(ValueError,'System Dot unavailable'):
                transport.send(next(iter(transport.DOTS)),'synthetic message',ledger)
            with self.assertRaisesRegex(ValueError,'System Dot unavailable'):
                transport.matching_reply(next(iter(transport.DOTS)),'synthetic marker')
            self.assertFalse(ledger.exists())
            evaluate.assert_not_called()


if __name__=='__main__': unittest.main()
