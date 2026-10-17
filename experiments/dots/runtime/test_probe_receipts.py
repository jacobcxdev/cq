import test_support
import json,unittest
from probe import receipt_owner,valid_read_receipt

class ReceiptTests(unittest.TestCase):
    def setUp(self):
        self.owner={'alias':'worker'}
        self.fixture=({},[{'messaging_room_id':'room'}],{'room':self.owner},[])

    def test_real_room_only(self):
        self.assertIs(receipt_owner('/backend-api/messaging/rooms/room/read',self.fixture),self.owner)
        for room in ['unknown','room/read','room%2Fread','room/../other']:
            self.assertIsNone(receipt_owner('/backend-api/messaging/rooms/'+room+'/read',self.fixture))
        self.assertIsNone(receipt_owner('/backend-api/messaging/rooms/room/messages',self.fixture))
        self.assertIsNone(receipt_owner('/backend-api/messaging/rooms/room/read',None))
        self.fixture[1].append({'messaging_room_id':'room'})
        self.assertIsNone(receipt_owner('/backend-api/messaging/rooms/room/read',self.fixture))

    def test_only_native_timestamp_body(self):
        for value in ['2026-10-11T11:00:00Z','2026-10-11T11:00:00.123+00:00']:
            self.assertTrue(valid_read_receipt(json.dumps({'last_read_at':value})))
        for value in [None,[],{}, {'last_read_at':None},{'last_read_at':'bad'},
                      {'last_read_at':'2026-10-11T11:00:00'},
                      {'last_read_at':'2026-10-11T11:00:00Z','extra':True}]:
            self.assertFalse(valid_read_receipt(json.dumps(value)))
        self.assertFalse(valid_read_receipt('bad json'))

if __name__ == '__main__':unittest.main()
