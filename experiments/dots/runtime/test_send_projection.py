import test_support
import copy, unittest
import self_projection

class SendProjectionTests(unittest.TestCase):
    def test_accepted_send_remains_self_and_preserves_message(self):
        message={'id':'message','account_user_id':'worker-user','role':'user',
                 'content':{'text':'example'},'created_at':'timestamp'}
        expected=copy.deepcopy(message)
        expected['is_self']=True
        self.assertEqual(self_projection.project_response(message,{'worker-user'},direct=True),1)
        self.assertEqual(message,expected)

    def test_foreign_sender_and_assistant_not_marked_self(self):
        for message in [{'account_user_id':'other','role':'user'},
                        {'account_user_id':'worker-user','role':'assistant'},
                        {'account_user_id':'worker-user','raw_messages':[]},None,[]]:
            original=copy.deepcopy(message)
            self.assertEqual(self_projection.project_response(message,{'worker-user'},direct=True),0)
            self.assertEqual(message,original)

    def test_history_projection_keeps_existing_scope(self):
        value={'items':[{'account_user_id':'worker-user'},{'account_user_id':'other'}]}
        self.assertEqual(self_projection.project_response(value,{'worker-user'},direct=False),1)
        self.assertTrue(value['items'][0]['is_self'])
        self.assertNotIn('is_self',value['items'][1])

if __name__ == '__main__': unittest.main()
