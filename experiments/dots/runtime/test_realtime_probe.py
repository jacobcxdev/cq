import test_support
import unittest, urllib.parse
from realtime_probe import signalling_owner, RESOURCES

class SignallingBoundaryTests(unittest.TestCase):
    def url(self,thread,dot,extra=None):
        return '/v1/cua?'+urllib.parse.urlencode({'source':'aeon','thread_id':thread,'aeon_id':dot,**(extra or {})})

    def test_known_resource_pairs_pin_owner(self):
        for thread,(owner,dot) in RESOURCES.items():
            self.assertEqual(signalling_owner('POST',self.url(thread,dot),'application/sdp'),owner)

    def test_cross_owner_pair_is_rejected(self):
        threads=list(RESOURCES)
        with self.assertRaises(ValueError):
            signalling_owner('POST',self.url(threads[0],RESOURCES[threads[1]][1]),'application/sdp')

    def test_unknown_missing_duplicate_or_unapproved_parameters_rejected(self):
        thread,(owner,dot)=next(iter(RESOURCES.items()))
        for path in [self.url('unknown',dot), '/v1/cua', self.url(thread,dot)+'&thread_id='+thread,
                     self.url(thread,dot,{'environment':'other'}),self.url(thread,dot).replace('/v1/cua','/v1/other')]:
            with self.assertRaises(ValueError): signalling_owner('POST',path,'application/sdp')
        for method,ctype in [('GET','application/sdp'),('POST','application/json')]:
            with self.assertRaises(ValueError): signalling_owner(method,self.url(thread,dot),ctype)

if __name__=='__main__': unittest.main()
