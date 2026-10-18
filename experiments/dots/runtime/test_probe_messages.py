import test_support
import json, unittest
from probe import message_owner, valid_text_message

class MessageBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.main = {'alias':'main'}
        self.worker = {'alias':'worker'}
        self.fixture = (self.main,
            [{'messaging_room_id':'room-main'}, {'messaging_room_id':'room-worker'}],
            {'room-main':self.main,'room-worker':self.worker,'dot-worker':self.worker}, [])

    def test_known_rooms_pin_separate_owners(self):
        for room, owner in [('room-main',self.main),('room-worker',self.worker)]:
            for operation in ['messages','aeon/prepare']:
                self.assertIs(message_owner(f'/backend-api/messaging/rooms/{room}/{operation}',self.fixture),owner)

    def test_unknown_room_or_other_resource_cannot_be_sent_to(self):
        for room in ['unknown','dot-worker','room-worker%2Fmessages','room-worker/../room-main']:
            self.assertIsNone(message_owner(f'/backend-api/messaging/rooms/{room}/messages',self.fixture))
        self.assertIsNone(message_owner('/backend-api/messaging/rooms/room-worker/messages',None))

    def test_unapproved_operations_remain_blocked(self):
        for suffix in ['live','read','files','messages/id/reactions','aeon/reboot']:
            self.assertIsNone(message_owner('/backend-api/messaging/rooms/room-worker/'+suffix,self.fixture))

    def test_collision_rejects_owner(self):
        self.fixture[1].append({'messaging_room_id':'room-worker'})
        self.assertIsNone(message_owner('/backend-api/messaging/rooms/room-worker/messages',self.fixture))

    def test_text_request_preserves_idempotency_boundary(self):
        value={'content':{'text':'test'},'request_id':'r','idempotency_token':'r'}
        self.assertTrue(valid_text_message(json.dumps(value)))
        for invalid in [None, [], {}, {'content':None}, {**value,'idempotency_token':'other'},
                        {**value,'content':{'text':' '}},
                        {**value,'content':{'text':'test','attachments':[{'file_id':'foreign'}]}}]:
            self.assertFalse(valid_text_message(json.dumps(invalid)))
        self.assertFalse(valid_text_message(b'invalid json'))

    def test_native_file_messages_keep_idempotency_and_attachment_schema(self):
        value = {'content': {'text': '', 'attachments': [{'type': 'file', 'file_id': 'CalpicoFile_proof'}]},
                 'request_id': 'request', 'idempotency_token': 'request'}
        self.assertTrue(valid_text_message(json.dumps(value), allow_attachments=True))
        for content in [{'text': '', 'attachments': []},
                        {'text': '', 'attachments': [{'type': 'file'}]},
                        {'text': '', 'attachments': [{'type': 'file', 'file_id': '../foreign'}]},
                        {'text': '', 'attachments': [{'type': 'thread', 'file_id': 'foreign'}]}]:
            self.assertFalse(valid_text_message(json.dumps({**value, 'content': content}), allow_attachments=True))
        self.assertFalse(valid_text_message(json.dumps({**value, 'idempotency_token': 'other'}), allow_attachments=True))
        self.assertFalse(valid_text_message(json.dumps(value)))

if __name__ == '__main__': unittest.main()
