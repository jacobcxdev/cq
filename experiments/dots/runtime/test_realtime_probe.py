import test_support
import unittest, urllib.parse
from unittest.mock import patch
from realtime_probe import signalling_owner, RESOURCES

class SignallingBoundaryTests(unittest.TestCase):
    def url(self,thread,dot,extra=None):
        return '/v1/cua?'+urllib.parse.urlencode({'source':'aeon','thread_id':thread,'aeon_id':dot,**(extra or {})})

    def test_known_resource_pairs_pin_owner(self):
        for thread,(owner,dot) in RESOURCES.items():
            self.assertEqual(signalling_owner('POST',self.url(thread,dot),'application/sdp'),owner)

    def test_single_owner_and_duplicate_labels_do_not_change_resource_authority(self):
        for resources in [{'one-root':('11111111','one-profile')},
                          {'one-root':('11111111','one-profile'),'two-root':('33333333','two-profile')}]:
            with self.subTest(resources=resources), patch.dict(RESOURCES,resources,clear=True):
                for root,(alias,profile) in resources.items():
                    self.assertEqual(signalling_owner('POST',self.url(root,profile),'application/sdp'),alias)
                with self.assertRaises(ValueError):
                    signalling_owner('POST',self.url('unverified-root','one-profile'),'application/sdp')
                if len(resources)>1:
                    with self.assertRaises(ValueError):
                        signalling_owner('POST',self.url('one-root','two-profile'),'application/sdp')

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
