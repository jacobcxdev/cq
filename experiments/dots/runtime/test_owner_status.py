import test_support
import json,time,unittest
from unittest.mock import patch,MagicMock
import cloud_stdio

class OwnerStatusTests(unittest.TestCase):
 def setUp(self):
  self.root='secondary-root-example'
  self.owner={'alias':'22222222','account_id':'secondary-account','token':'test-token'}
 def read(self,status_code=200,thread_id=None,status=None,content_type='application/json'):
  conn=MagicMock();response=conn.getresponse.return_value;response.status=status_code
  response.getheader.return_value=content_type
  response.read.return_value=json.dumps({'thread':{'id':thread_id or self.root,'status':status or {'type':'active','activeFlags':['waitingOnApproval']}}}).encode()
  with patch('cloud_stdio.http.client.HTTPSConnection',return_value=conn),patch('cloud_stdio.catalog.claims',return_value={'exp':time.time()+600}):
   value=cloud_stdio.owner_thread_status(self.owner,self.root,None)
  return value,conn
 def test_real_owner_status_and_approval_flags_preserved(self):
  value,conn=self.read()
  self.assertEqual(value,{'type':'active','activeFlags':['waitingOnApproval']})
  request=conn.request.call_args;headers=request.kwargs['headers']
  self.assertEqual(headers['Authorization'],'Bearer test-token')
  self.assertEqual(headers['ChatGPT-Account-Id'],'secondary-account')
  self.assertEqual(headers['X-OpenAI-Product-Sku'],'aeon')
  self.assertTrue(headers['User-Agent'].startswith('Codex Desktop/'))
  conn.close.assert_called_once()
 def test_foreign_thread_response_rejected(self):
  with self.assertRaises(ValueError):self.read(thread_id='foreign-root')
 def test_http_failure_rejected_before_decoding(self):
  with self.assertRaises(ValueError):self.read(status_code=403,content_type='text/html')
 def test_invalid_active_flags_rejected(self):
  with self.assertRaises(ValueError):self.read(status={'type':'active','activeFlags':'invalid'})
 def test_unknown_owner_request_rejected(self):
  with self.assertRaises(ValueError):cloud_stdio.owner_thread_status({**self.owner,'alias':'11111111'},self.root,None)

if __name__=='__main__':unittest.main()
