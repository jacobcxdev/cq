import test_support
import json,unittest
from native_executor_reuse import validate_native
class NativeReuseBoundaries(unittest.TestCase):
    def setUp(self):
        self.system={'alias':test_support.CONFIG['system_alias'],'account_id':'main','token':'test-secret'}
        self.headers={'Authorization':'Bearer test-secret','ChatGPT-Account-Id':'main'}
        self.proof={'owner_alias':test_support.CONFIG['secondary_alias'],'registration_accepted':True,'root_thread_id':'null-root','environment_id':'research-env'}
        self.body=json.dumps({'displayName':'Mac','installation_id':'5403b4ce-fdb9-4818-afe5-c936b63b7e8d','selected_capability_roots':[]})
    def test_exact_existing_owner(self):validate_native(self.headers,self.body,self.system,self.proof,'null-root')
    def test_foreign_native_identity(self):
        for headers in [{**self.headers,'ChatGPT-Account-Id':'other'},{**self.headers,'Authorization':'Bearer wrong'}]:
            with self.assertRaises(ValueError):validate_native(headers,self.body,self.system,self.proof,'null-root')
    def test_stale_root_or_foreign_registration(self):
        for p in [{**self.proof,'owner_alias':test_support.CONFIG['system_alias']},{**self.proof,'root_thread_id':'old'},{**self.proof,'registration_accepted':False}]:
            with self.assertRaises(ValueError):validate_native(self.headers,self.body,self.system,p,'null-root')
    def test_unexpected_registration_body(self):
        b=json.loads(self.body);b['newGrant']=True
        with self.assertRaises(ValueError):validate_native(self.headers,json.dumps(b),self.system,self.proof,'null-root')
if __name__=='__main__':unittest.main()
