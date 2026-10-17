import test_support
import os,ssl,unittest
from unittest.mock import patch
import catalog,cloud_stdio,realtime_probe

class RootRefreshTests(unittest.TestCase):
    def setUp(self):
        self.accounts={alias:{'alias':alias,'account_id':alias,'token':'test-token'} for alias in catalog.PROOF_PROFILE_IDS}
        self.roots={alias:'current-'+alias for alias in self.accounts}
    def primary(self,account,path,headers):
        alias=account['alias'];root=self.roots[alias]
        return {'selection':{'available':True,'thread_id':root},
                'profile':{'id':catalog.PROOF_PROFILE_IDS[alias],'active_root_thread_id':root}}
    def test_authenticated_current_root_replaces_frozen_routing(self):
        with patch('catalog.get',side_effect=self.primary) as get:
            roots=catalog.primary_roots(self.accounts,{})
        for alias,root in self.roots.items():self.assertEqual(roots[root],(alias,catalog.PROOF_PROFILE_IDS[alias]))
        self.assertEqual(get.call_count,2)
    def test_foreign_profile_root_and_unavailable_selection_rejected(self):
        for invalid in [{'selection':{'available':False},'profile':{}},
                        {'selection':{'available':True,'thread_id':'x'},'profile':{'id':'foreign','active_root_thread_id':'x'}},
                        {'selection':{'available':True,'thread_id':'x'},'profile':{'id':catalog.PROOF_PROFILE_IDS['11111111'],'active_root_thread_id':'other'}}]:
            with patch('catalog.get',return_value=invalid):
                with self.assertRaises(ValueError):catalog.primary_roots(self.accounts,{})
    def test_conflicting_root_owner_rejected(self):
        self.roots={alias:'shared' for alias in self.accounts}
        with patch('catalog.get',side_effect=self.primary):
            with self.assertRaises(ValueError):catalog.primary_roots(self.accounts,{})
    def test_new_stdio_root_resolves_to_secondary_never_local(self):
        with patch.dict(cloud_stdio.THREAD_OWNERS,{},clear=True),patch('catalog.get',side_effect=self.primary),patch('cloud_stdio.event'):
            self.assertEqual(cloud_stdio.resolve_thread_owner(self.roots['22222222'],self.accounts),'22222222')
            self.assertEqual(cloud_stdio.select_owner('thread/resume',{'threadId':self.roots['22222222']},'11111111'),('22222222',self.roots['22222222']))
    def test_refresh_failure_does_not_assign_unknown_root(self):
        with patch.dict(cloud_stdio.THREAD_OWNERS,{},clear=True),patch('catalog.get',side_effect=ValueError('unavailable')):
            with self.assertRaises(ValueError):cloud_stdio.resolve_thread_owner('new',self.accounts)
            self.assertNotIn('new',cloud_stdio.THREAD_OWNERS)
    def test_new_computer_pair_resolves_from_owner_primary(self):
        import urllib.parse
        path='/v1/cua?'+urllib.parse.urlencode({'source':'aeon','thread_id':self.roots['22222222'],'aeon_id':catalog.PROOF_PROFILE_IDS['22222222']})
        with patch.dict(realtime_probe.RESOURCES,{},clear=True),patch('catalog.get',side_effect=self.primary):
            self.assertEqual(realtime_probe.signalling_owner('POST',path,'application/sdp',self.accounts),'22222222')
    def test_public_upstream_uses_explicit_public_ca_despite_probe_environment(self):
        with patch.dict(os.environ,{'SSL_CERT_FILE':'/probe-only.pem'}),patch('catalog.http.client.HTTPSConnection') as connection:
            response=connection.return_value.getresponse.return_value
            response.status=200;response.read.return_value=b'{}'
            catalog.get(self.accounts['11111111'],'/backend-api/tbo/primary',{})
        context=connection.call_args.kwargs.get('context')
        self.assertIsInstance(context,ssl.SSLContext)
        self.assertEqual(context.verify_mode,ssl.CERT_REQUIRED)
        self.assertTrue(context.check_hostname)
        self.assertGreater(context.cert_store_stats()['x509_ca'],0)

if __name__=='__main__':unittest.main()
