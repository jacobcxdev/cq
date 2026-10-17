import test_support
import unittest
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

if __name__=='__main__': unittest.main()
